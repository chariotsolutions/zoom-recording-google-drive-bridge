package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// ----------------------------------------------------------------------------
// Tier 1: Pure function tests
// ----------------------------------------------------------------------------

// signFor generates a Zoom-style signature header for a given body and
// timestamp using the supplied secret. Used in handler tests too.
func signFor(secret, timestamp string, body []byte) string {
	msg := "v0:" + timestamp + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyZoomSignature(t *testing.T) {
	secret := "test_secret"
	body := []byte(`{"event":"recording.completed","payload":{}}`)
	now := time.Now().Unix()
	nowStr := strconv.FormatInt(now, 10)
	validSig := signFor(secret, nowStr, body)

	tests := []struct {
		name      string
		secret    string
		timestamp string
		signature string
		body      []byte
		want      bool
	}{
		{
			name:      "valid signature",
			secret:    secret,
			timestamp: nowStr,
			signature: validSig,
			body:      body,
			want:      true,
		},
		{
			name:      "wrong secret",
			secret:    "wrong_secret",
			timestamp: nowStr,
			signature: validSig,
			body:      body,
			want:      false,
		},
		{
			name:      "tampered body",
			secret:    secret,
			timestamp: nowStr,
			signature: validSig,
			body:      []byte(`{"event":"recording.completed","payload":{"tampered":true}}`),
			want:      false,
		},
		{
			name:      "tampered timestamp",
			secret:    secret,
			timestamp: strconv.FormatInt(now-1, 10),
			signature: validSig,
			body:      body,
			want:      false,
		},
		{
			name:      "empty secret",
			secret:    "",
			timestamp: nowStr,
			signature: validSig,
			body:      body,
			want:      false,
		},
		{
			name:      "empty timestamp",
			secret:    secret,
			timestamp: "",
			signature: validSig,
			body:      body,
			want:      false,
		},
		{
			name:      "empty signature",
			secret:    secret,
			timestamp: nowStr,
			signature: "",
			body:      body,
			want:      false,
		},
		{
			name:      "non-numeric timestamp",
			secret:    secret,
			timestamp: "not-a-number",
			signature: validSig,
			body:      body,
			want:      false,
		},
		{
			name:      "stale timestamp (past)",
			secret:    secret,
			timestamp: strconv.FormatInt(now-301, 10),
			signature: signFor(secret, strconv.FormatInt(now-301, 10), body),
			body:      body,
			want:      false,
		},
		{
			name:      "stale timestamp (future)",
			secret:    secret,
			timestamp: strconv.FormatInt(now+301, 10),
			signature: signFor(secret, strconv.FormatInt(now+301, 10), body),
			body:      body,
			want:      false,
		},
		{
			name:      "boundary timestamp (just inside window, past)",
			secret:    secret,
			timestamp: strconv.FormatInt(now-299, 10),
			signature: signFor(secret, strconv.FormatInt(now-299, 10), body),
			body:      body,
			want:      true,
		},
		{
			name:      "boundary timestamp (just inside window, future)",
			secret:    secret,
			timestamp: strconv.FormatInt(now+299, 10),
			signature: signFor(secret, strconv.FormatInt(now+299, 10), body),
			body:      body,
			want:      true,
		},
		{
			name:      "signature without v0 prefix",
			secret:    secret,
			timestamp: nowStr,
			signature: strings.TrimPrefix(validSig, "v0="),
			body:      body,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := verifyZoomSignature(tt.secret, tt.timestamp, tt.signature, tt.body)
			if got != tt.want {
				t.Errorf("verifyZoomSignature() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildSignedDownloadURL(t *testing.T) {
	tests := []struct {
		name        string
		downloadURL string
		token       string
		want        string
		wantErr     bool
	}{
		{
			name:        "no existing query",
			downloadURL: "https://zoom.us/rec/download/abc",
			token:       "tok123",
			want:        "https://zoom.us/rec/download/abc?access_token=tok123",
		},
		{
			name:        "existing unrelated query param",
			downloadURL: "https://zoom.us/rec/download/abc?foo=bar",
			token:       "tok123",
			want:        "https://zoom.us/rec/download/abc?access_token=tok123&foo=bar",
		},
		{
			name:        "existing access_token replaced",
			downloadURL: "https://zoom.us/rec/download/abc?access_token=old_token",
			token:       "new_token",
			want:        "https://zoom.us/rec/download/abc?access_token=new_token",
		},
		{
			name:        "token with special characters",
			downloadURL: "https://zoom.us/rec/download/abc",
			token:       "tok+with/slash=padding",
			want:        "https://zoom.us/rec/download/abc?access_token=tok%2Bwith%2Fslash%3Dpadding",
		},
		{
			name:        "malformed URL",
			downloadURL: "://not-a-url",
			token:       "tok",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildSignedDownloadURL(tt.downloadURL, tt.token)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (result=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Compare via parsed URLs to be order-insensitive on query params
			gotU, _ := url.Parse(got)
			wantU, _ := url.Parse(tt.want)
			if gotU.Scheme != wantU.Scheme || gotU.Host != wantU.Host || gotU.Path != wantU.Path {
				t.Errorf("URL base mismatch: got %q, want %q", got, tt.want)
			}
			if gotU.Query().Get("access_token") != wantU.Query().Get("access_token") {
				t.Errorf("access_token mismatch: got %q, want %q",
					gotU.Query().Get("access_token"), wantU.Query().Get("access_token"))
			}
			// Verify all expected non-token params are present
			for k := range wantU.Query() {
				if k == "access_token" {
					continue
				}
				if gotU.Query().Get(k) != wantU.Query().Get(k) {
					t.Errorf("query param %q mismatch: got %q, want %q",
						k, gotU.Query().Get(k), wantU.Query().Get(k))
				}
			}
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "Meeting Notes", "Meeting Notes"},
		{"slashes", "a/b\\c", "a-b-c"},
		{"all bad chars", `?%*:|"<>`, "--------"},
		{"unicode preserved", "Meeting — Notes", "Meeting — Notes"},
		{
			name: "length truncated to 200",
			in:   strings.Repeat("a", 250),
			want: strings.Repeat("a", 200),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeFilename(tt.in)
			if got != tt.want {
				t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseStartDate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "unknown-date"},
		{"valid RFC3339 UTC", "2026-04-10T12:34:56Z", "2026-04-10"},
		{"valid RFC3339 with offset", "2026-04-10T12:34:56-04:00", "2026-04-10"},
		{"malformed", "not-a-date", "unknown-date"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStartDate(tt.in)
			if got != tt.want {
				t.Errorf("parseStartDate(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildFilename(t *testing.T) {
	tests := []struct {
		name  string
		topic string
		file  RecordingFile
		want  string
	}{
		{
			name:  "with extension",
			topic: "Strategy Meeting",
			file:  RecordingFile{FileExtension: "MP4", RecordingType: "shared_screen_with_speaker_view"},
			want:  "Strategy Meeting-shared_screen_with_speaker_view.mp4",
		},
		{
			name:  "no extension uses guess",
			topic: "Strategy Meeting",
			file:  RecordingFile{FileType: "M4A", RecordingType: "audio_only"},
			want:  "Strategy Meeting-audio_only.m4a",
		},
		{
			name:  "topic with bad chars sanitized",
			topic: "Strategy/Meeting?",
			file:  RecordingFile{FileExtension: "MP4", RecordingType: "shared_screen_with_speaker_view"},
			want:  "Strategy-Meeting--shared_screen_with_speaker_view.mp4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildFilename(tt.topic, tt.file)
			if got != tt.want {
				t.Errorf("buildFilename() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGuessExtension(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"MP4", "mp4"},
		{"M4A", "m4a"},
		{"TRANSCRIPT", "vtt"},
		{"CC", "vtt"},
		{"CHAT", "txt"},
		{"CSV", "csv"},
		{"TIMELINE", "json"},
		{"SUMMARY", "vtt"},
		{"mp4", "mp4"}, // case insensitive
		{"UNKNOWN", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := guessExtension(tt.in)
			if got != tt.want {
				t.Errorf("guessExtension(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEscapeQuery(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"with 'quote'", `with \'quote\'`},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := escapeQuery(tt.in)
			if got != tt.want {
				t.Errorf("escapeQuery(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHostUsername(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"standard", "skapadia@chariotsolutions.com", "skapadia"},
		{"mixed case lowercased", "Skapadia@Chariot.com", "skapadia"},
		{"trailing whitespace trimmed", " foo@bar.com ", "foo"},
		{"empty", "", "unknown-host"},
		{"no @ sign", "foo", "unknown-host"},
		{"@ at start", "@example.com", "unknown-host"},
		{"just @", "@", "unknown-host"},
		{"multiple @ uses last", "foo@bar@example.com", "foo@bar"},
		{"slash in local part sanitized", "first/last@example.com", "first-last"},
		{"dot and plus left alone", "first.last+tag@example.com", "first.last+tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hostUsername(tt.in)
			if got != tt.want {
				t.Errorf("hostUsername(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildMeetingFolderName(t *testing.T) {
	tests := []struct {
		name    string
		meeting ZoomMeeting
		want    string
	}{
		{
			name: "standard meeting",
			meeting: ZoomMeeting{
				Topic:     "Weekly Standup",
				StartTime: "2026-04-11T14:30:00Z",
			},
			want: "2026-04-11T14-30-Weekly Standup",
		},
		{
			name: "same topic different time produces different name",
			meeting: ZoomMeeting{
				Topic:     "Weekly Standup",
				StartTime: "2026-04-11T18:00:00Z",
			},
			want: "2026-04-11T18-00-Weekly Standup",
		},
		{
			name: "topic with special chars sanitized",
			meeting: ZoomMeeting{
				Topic:     "Sales: Acme Corp / Q2",
				StartTime: "2026-04-11T09:00:00Z",
			},
			want: "2026-04-11T09-00-Sales- Acme Corp - Q2",
		},
		{
			name: "empty start time",
			meeting: ZoomMeeting{
				Topic:     "My Meeting",
				StartTime: "",
			},
			want: "unknown-date-My Meeting",
		},
		{
			name: "malformed start time",
			meeting: ZoomMeeting{
				Topic:     "My Meeting",
				StartTime: "not-a-date",
			},
			want: "unknown-date-My Meeting",
		},
		{
			name: "empty topic",
			meeting: ZoomMeeting{
				Topic:     "",
				StartTime: "2026-04-11T10:00:00Z",
			},
			want: "2026-04-11T10-00-Untitled Meeting",
		},
		{
			name: "empty everything",
			meeting: ZoomMeeting{
				Topic:     "",
				StartTime: "",
			},
			want: "unknown-date-Untitled Meeting",
		},
		{
			name: "timezone offset in start time still parses as UTC output",
			meeting: ZoomMeeting{
				Topic:     "Team Call",
				StartTime: "2026-04-11T14:30:00-04:00",
			},
			want: "2026-04-11T18-30-Team Call", // converted to UTC
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildMeetingFolderName(tt.meeting)
			if got != tt.want {
				t.Errorf("buildMeetingFolderName() = %q, want %q", got, tt.want)
			}
		})
	}

	// Explicit collision test: two meetings with the same topic and date
	// but different start times MUST produce different folder names.
	meeting1 := ZoomMeeting{Topic: "Weekly Standup", StartTime: "2026-04-11T14:00:00Z", ID: 111}
	meeting2 := ZoomMeeting{Topic: "Weekly Standup", StartTime: "2026-04-11T16:00:00Z", ID: 222}
	name1 := buildMeetingFolderName(meeting1)
	name2 := buildMeetingFolderName(meeting2)
	if name1 == name2 {
		t.Errorf("collision: two different meetings produced the same folder name %q", name1)
	}
}

// ----------------------------------------------------------------------------
// Tier 2: HTTP handler tests
// ----------------------------------------------------------------------------

const testSecret = "test_webhook_secret"

func newTestServer() *Server {
	return &Server{
		cfg: &Config{
			ZoomWebhookSecret: testSecret,
			// Other fields are not exercised by these tests
		},
		// Default to an accepting, capturing fake. Tests that care
		// about enqueue behavior replace this with their own fake.
		taskEnqueuer: &fakeTaskEnqueuer{},
	}
}

// postWebhook builds a test request to /webhook with optional signing.
// If sign is true, it computes a valid signature for the body using nowStr.
func postWebhook(t *testing.T, srv *Server, body []byte, sign bool, timestamp string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	if sign {
		req.Header.Set("x-zm-request-timestamp", timestamp)
		req.Header.Set("x-zm-signature", signFor(testSecret, timestamp, body))
	}
	rec := httptest.NewRecorder()
	srv.handleWebhook(rec, req)
	return rec
}

func TestHandleWebhook_Validation(t *testing.T) {
	srv := newTestServer()
	body := []byte(`{"event":"endpoint.url_validation","payload":{"plainToken":"abc123"}}`)

	// Validation events are NOT signed
	rec := postWebhook(t, srv, body, false, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v; body=%q", err, rec.Body.String())
	}
	if resp["plainToken"] != "abc123" {
		t.Errorf("plainToken = %q, want %q", resp["plainToken"], "abc123")
	}

	// Verify the encryptedToken is correct HMAC of plainToken
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte("abc123"))
	expected := hex.EncodeToString(mac.Sum(nil))
	if resp["encryptedToken"] != expected {
		t.Errorf("encryptedToken = %q, want %q", resp["encryptedToken"], expected)
	}
}

func TestHandleValidation_MalformedInnerPayload_Returns400(t *testing.T) {
	srv := newTestServer()
	// Outer envelope parses (event + payload as json.RawMessage), but the
	// inner "payload" is a string instead of an object, so the second
	// Unmarshal into ZoomValidationPayload fails. We should return 400 —
	// NOT respond with a bogus encryptedToken for a missing plainToken.
	body := []byte(`{"event":"endpoint.url_validation","payload":"not-an-object"}`)
	rec := postWebhook(t, srv, body, false, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleWebhook_UnsignedEventRejected(t *testing.T) {
	srv := newTestServer()
	body := []byte(`{"event":"recording.completed","payload":{"object":{}}}`)

	rec := postWebhook(t, srv, body, false, "")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleWebhook_TamperedBodyRejected(t *testing.T) {
	srv := newTestServer()
	originalBody := []byte(`{"event":"recording.completed","payload":{"object":{"id":1}}}`)
	tamperedBody := []byte(`{"event":"recording.completed","payload":{"object":{"id":999}}}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	// Sign the original body but POST the tampered one
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(tamperedBody)))
	req.Header.Set("x-zm-request-timestamp", now)
	req.Header.Set("x-zm-signature", signFor(testSecret, now, originalBody))
	rec := httptest.NewRecorder()
	srv.handleWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleWebhook_StaleTimestampRejected(t *testing.T) {
	srv := newTestServer()
	body := []byte(`{"event":"recording.completed","payload":{"object":{"id":1}}}`)
	staleTs := strconv.FormatInt(time.Now().Unix()-3600, 10) // 1 hour ago

	rec := postWebhook(t, srv, body, true, staleTs)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleWebhook_SignedRecordingMissingDownloadToken(t *testing.T) {
	srv := newTestServer()
	// Valid signature, but no top-level download_token
	body := []byte(`{"event":"recording.completed","payload":{"object":{"id":1,"topic":"test","recording_files":[]}}}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	rec := postWebhook(t, srv, body, true, now)

	// We log and bail with 200 so Zoom doesn't retry forever
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleWebhook_MalformedJSON(t *testing.T) {
	srv := newTestServer()
	body := []byte(`{not valid json`)

	rec := postWebhook(t, srv, body, false, "")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleWebhook_OversizedBodyRejected(t *testing.T) {
	srv := newTestServer()
	// 2 MB of junk — well over the 1 MB limit. After truncation the
	// body won't be valid JSON, so the handler returns 400.
	body := make([]byte, 2<<20)
	for i := range body {
		body[i] = 'x'
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	srv.handleWebhook(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleWebhook_MethodNotAllowed(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rec := httptest.NewRecorder()
	srv.handleWebhook(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleWebhook_TranscriptCompleted(t *testing.T) {
	srv := newTestServer()
	// Signed transcript event, no download_token → 200 + bail (same as
	// the recording.completed path). We can't verify the downstream
	// processRecording call without interfaces, but we can verify the
	// handler accepts the event and dispatches on the new case.
	body := []byte(`{"event":"recording.transcript_completed","payload":{"object":{"id":1,"topic":"test","recording_files":[]}}}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	rec := postWebhook(t, srv, body, true, now)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleWebhook_SignedRecording_EnqueuesTask(t *testing.T) {
	srv := newTestServer()
	fakeEnq := &fakeTaskEnqueuer{}
	srv.taskEnqueuer = fakeEnq

	body := []byte(`{"event":"recording.completed","download_token":"tok-abc","payload":{"object":{"id":42,"topic":"Test Meeting","host_email":"h@c.com","recording_files":[{"id":"f1","status":"completed","file_type":"MP4","download_url":"http://zoom/f1"}]}}}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	rec := postWebhook(t, srv, body, true, now)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if len(fakeEnq.enqueued) != 1 {
		t.Fatalf("enqueued = %d, want exactly 1 task", len(fakeEnq.enqueued))
	}
	p := fakeEnq.enqueued[0]
	if p.EventName != "recording.completed" {
		t.Errorf("EventName = %q, want %q", p.EventName, "recording.completed")
	}
	if p.DownloadToken != "tok-abc" {
		t.Errorf("DownloadToken = %q, want %q", p.DownloadToken, "tok-abc")
	}
	if p.Meeting.ID != 42 {
		t.Errorf("Meeting.ID = %d, want 42", p.Meeting.ID)
	}
	if !p.WriteMetadata {
		t.Errorf("WriteMetadata = false, want true for recording.completed")
	}
}

func TestHandleWebhook_SignedTranscript_EnqueuesTaskWithoutWriteMetadata(t *testing.T) {
	srv := newTestServer()
	fakeEnq := &fakeTaskEnqueuer{}
	srv.taskEnqueuer = fakeEnq

	body := []byte(`{"event":"recording.transcript_completed","download_token":"tok-xyz","payload":{"object":{"id":42,"topic":"t","recording_files":[{"id":"t1","status":"completed","file_type":"TRANSCRIPT","download_url":"http://zoom/t1"}]}}}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	rec := postWebhook(t, srv, body, true, now)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if len(fakeEnq.enqueued) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(fakeEnq.enqueued))
	}
	p := fakeEnq.enqueued[0]
	if p.EventName != "recording.transcript_completed" {
		t.Errorf("EventName = %q, want %q", p.EventName, "recording.transcript_completed")
	}
	if p.WriteMetadata {
		t.Errorf("WriteMetadata = true, want false for transcript event")
	}
}

func TestHandleWebhook_EnqueueFailure_Returns500(t *testing.T) {
	srv := newTestServer()
	srv.taskEnqueuer = &fakeTaskEnqueuer{returnErr: errors.New("cloud tasks unavailable")}

	body := []byte(`{"event":"recording.completed","download_token":"tok","payload":{"object":{"id":42,"topic":"t","recording_files":[]}}}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	rec := postWebhook(t, srv, body, true, now)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (so Zoom retries); body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleWebhook_TranscriptUnsignedRejected(t *testing.T) {
	srv := newTestServer()
	body := []byte(`{"event":"recording.transcript_completed","payload":{"object":{}}}`)

	rec := postWebhook(t, srv, body, false, "")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleWebhook_UnknownEvent(t *testing.T) {
	srv := newTestServer()
	body := []byte(`{"event":"meeting.started","payload":{}}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	rec := postWebhook(t, srv, body, true, now)

	// Unknown events are accepted (200) so Zoom doesn't retry, but no processing happens
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
}

// Sanity check that the test helper signFor produces the same signature
// the production verifier accepts. Catches drift if either the helper or
// the verifier is changed in isolation.
func TestSignForRoundtrip(t *testing.T) {
	body := []byte(`{"event":"test"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := signFor(testSecret, ts, body)

	if !verifyZoomSignature(testSecret, ts, sig, body) {
		t.Errorf("signFor produced a signature that verifyZoomSignature rejected:\n  ts=%s\n  sig=%s",
			ts, sig)
	}
}

// ----------------------------------------------------------------------------
// Rate limiting middleware tests
// ----------------------------------------------------------------------------

func TestRateLimitMiddleware_AllowsUnderLimit(t *testing.T) {
	// Burst of 5 with a high refill rate: all 5 back-to-back requests
	// should pass through to the inner handler.
	limiter := rate.NewLimiter(rate.Limit(100), 5)
	called := 0
	handler := rateLimitMiddleware(limiter, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, rec.Code)
		}
	}
	if called != 5 {
		t.Fatalf("inner handler called %d times, want 5", called)
	}
}

func TestRateLimitMiddleware_RejectsOverLimit(t *testing.T) {
	// Burst 2 with rate 1 rps: after 2 requests the bucket is empty and
	// refills too slowly to affect the test.
	limiter := rate.NewLimiter(rate.Limit(1), 2)
	called := 0
	handler := rateLimitMiddleware(limiter, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust the burst.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("burst request %d: status = %d, want 200", i+1, rec.Code)
		}
	}

	// Next request must be rejected, and the inner handler must NOT run.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit request: status = %d, want 429", rec.Code)
	}
	if called != 2 {
		t.Fatalf("inner handler called %d times, want 2 (must not run on rejection)", called)
	}
}

func TestRateLimitMiddleware_RefillsOverTime(t *testing.T) {
	// 100 rps → one token every 10ms. Burst 1 keeps the bucket small so
	// a short sleep is enough to observe refill without slowing the test.
	limiter := rate.NewLimiter(rate.Limit(100), 1)
	handler := rateLimitMiddleware(limiter, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request consumes the only token.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", rec.Code)
	}

	// Immediately after, the bucket is empty.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("immediate retry: status = %d, want 429", rec.Code)
	}

	// Wait long enough for the bucket to refill at least one token.
	time.Sleep(50 * time.Millisecond)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("after refill: status = %d, want 200", rec.Code)
	}
}

// Diagnostic helper used in failing tests to dump useful info.
//
//nolint:unused
func dumpRequest(t *testing.T, prefix string, rec *httptest.ResponseRecorder) {
	t.Helper()
	t.Logf("%s: status=%d body=%s", prefix, rec.Code, rec.Body.String())
}

// Compile-time check we still use fmt (kept available for future tests).
var _ = fmt.Sprintf

// ----------------------------------------------------------------------------
// Tier 3: Cloud Tasks primitives
// ----------------------------------------------------------------------------

// fakeTaskEnqueuer captures enqueued payloads for test assertion. Safe
// for concurrent use. Always captures the payload, even if returnErr
// is set — callers assert on both behaviors.
type fakeTaskEnqueuer struct {
	mu        sync.Mutex
	enqueued  []TaskPayload
	returnErr error
}

func (f *fakeTaskEnqueuer) Enqueue(ctx context.Context, payload TaskPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, payload)
	return f.returnErr
}

// fakeTokenValidator captures the last (token, audience) pair handed to
// Validate and returns returnErr (nil = valid). Used by tests that
// exercise handleProcessEvent and by the synthetic test driver when
// running against the Cloud Tasks emulator.
type fakeTokenValidator struct {
	mu           sync.Mutex
	lastToken    string
	lastAudience string
	returnErr    error
}

func (f *fakeTokenValidator) Validate(ctx context.Context, token, audience string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastToken = token
	f.lastAudience = audience
	return f.returnErr
}

func TestTaskPayload_JSONRoundTrip(t *testing.T) {
	orig := TaskPayload{
		EventName: "recording.completed",
		Meeting: ZoomMeeting{
			ID:        12345,
			UUID:      "uuid-123",
			HostID:    "host-abc",
			HostEmail: "skapadia@chariotsolutions.com",
			Topic:     "Test Meeting",
			StartTime: "2026-04-18T12:00:00Z",
			Duration:  45,
			RecordingFiles: []RecordingFile{
				{
					ID:             "file-1",
					MeetingID:      "mtg-1",
					RecordingStart: "2026-04-18T12:00:00Z",
					RecordingEnd:   "2026-04-18T12:45:00Z",
					FileType:       "MP4",
					FileExtension:  "MP4",
					FileSize:       123456,
					PlayURL:        "https://zoom.us/rec/play/abc",
					DownloadURL:    "https://zoom.us/rec/download/abc",
					RecordingType:  "shared_screen_with_speaker_view",
					Status:         "completed",
				},
			},
		},
		DownloadToken: "abc-123-token",
		WriteMetadata: true,
	}

	b, err := json.Marshal(&orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got TaskPayload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !reflect.DeepEqual(orig, got) {
		t.Fatalf("round-trip mismatch:\norig = %+v\ngot  = %+v", orig, got)
	}
}

func TestFakeTaskEnqueuer_Captures(t *testing.T) {
	f := &fakeTaskEnqueuer{}
	p := TaskPayload{EventName: "recording.completed", DownloadToken: "abc"}
	if err := f.Enqueue(context.Background(), p); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if len(f.enqueued) != 1 {
		t.Fatalf("enqueued len = %d, want 1", len(f.enqueued))
	}
	if f.enqueued[0].EventName != "recording.completed" {
		t.Errorf("EventName = %q, want %q", f.enqueued[0].EventName, "recording.completed")
	}
	if f.enqueued[0].DownloadToken != "abc" {
		t.Errorf("DownloadToken = %q, want %q", f.enqueued[0].DownloadToken, "abc")
	}
}

func TestFakeTaskEnqueuer_ReturnsConfiguredError(t *testing.T) {
	want := errors.New("boom")
	f := &fakeTaskEnqueuer{returnErr: want}
	err := f.Enqueue(context.Background(), TaskPayload{})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	// Even on error, the fake captures the payload so tests can
	// assert on what was attempted.
	if len(f.enqueued) != 1 {
		t.Errorf("enqueued len = %d, want 1 (capture on error too)", len(f.enqueued))
	}
}

func TestFakeTokenValidator_AcceptsByDefault(t *testing.T) {
	f := &fakeTokenValidator{}
	if err := f.Validate(context.Background(), "tok", "aud"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if f.lastToken != "tok" || f.lastAudience != "aud" {
		t.Errorf("captured (%q, %q), want (tok, aud)", f.lastToken, f.lastAudience)
	}
}

func TestFakeTokenValidator_ReturnsConfiguredError(t *testing.T) {
	want := errors.New("invalid token")
	f := &fakeTokenValidator{returnErr: want}
	err := f.Validate(context.Background(), "x", "y")
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// ----------------------------------------------------------------------------
// Tier 4: /process-event handler tests
// ----------------------------------------------------------------------------

// newProcessEventTestServer builds a Server suitable for handleProcessEvent
// tests — pre-wired with accepting fakes. Tests override fields to
// exercise specific paths.
func newProcessEventTestServer() *Server {
	srv := &Server{
		cfg:            &Config{ProcessEventURL: "http://test.invalid/process-event"},
		tokenValidator: &fakeTokenValidator{}, // accept by default
	}
	srv.processEventFn = func(ctx context.Context, meeting ZoomMeeting, downloadToken string, writeMetadata bool) error {
		return nil
	}
	return srv
}

func postProcessEvent(t *testing.T, srv *Server, body []byte, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/process-event", bytes.NewReader(body))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	srv.handleProcessEvent(rec, req)
	return rec
}

func mustMarshalTask(t *testing.T, p TaskPayload) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal TaskPayload: %v", err)
	}
	return b
}

func TestHandleProcessEvent_MissingAuthorization_Returns401(t *testing.T) {
	srv := newProcessEventTestServer()
	body := mustMarshalTask(t, TaskPayload{EventName: "recording.completed"})
	rec := postProcessEvent(t, srv, body, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %q", rec.Code, rec.Body.String())
	}
}

func TestHandleProcessEvent_MalformedAuthorization_Returns401(t *testing.T) {
	srv := newProcessEventTestServer()
	body := mustMarshalTask(t, TaskPayload{EventName: "recording.completed"})
	// Not "Bearer <token>"
	rec := postProcessEvent(t, srv, body, "Basic abc123")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleProcessEvent_InvalidOIDCToken_Returns401(t *testing.T) {
	srv := newProcessEventTestServer()
	srv.tokenValidator = &fakeTokenValidator{returnErr: errors.New("invalid audience")}
	body := mustMarshalTask(t, TaskPayload{EventName: "recording.completed"})
	rec := postProcessEvent(t, srv, body, "Bearer fake-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleProcessEvent_PassesAudienceToValidator(t *testing.T) {
	srv := newProcessEventTestServer()
	fake := &fakeTokenValidator{}
	srv.tokenValidator = fake
	body := mustMarshalTask(t, TaskPayload{EventName: "recording.completed"})
	postProcessEvent(t, srv, body, "Bearer tok-xyz")
	if fake.lastToken != "tok-xyz" {
		t.Errorf("validator got token = %q, want tok-xyz", fake.lastToken)
	}
	if fake.lastAudience != "http://test.invalid/process-event" {
		t.Errorf("validator got audience = %q, want the configured ProcessEventURL", fake.lastAudience)
	}
}

func TestHandleProcessEvent_MalformedJSON_Returns400(t *testing.T) {
	srv := newProcessEventTestServer()
	rec := postProcessEvent(t, srv, []byte("{not-json"), "Bearer xyz")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleProcessEvent_ValidTask_InvokesProcessEventFn(t *testing.T) {
	srv := newProcessEventTestServer()

	var (
		called     int
		gotMeeting ZoomMeeting
		gotToken   string
		gotWriteMd bool
	)
	srv.processEventFn = func(ctx context.Context, meeting ZoomMeeting, downloadToken string, writeMetadata bool) error {
		called++
		gotMeeting = meeting
		gotToken = downloadToken
		gotWriteMd = writeMetadata
		return nil
	}

	payload := TaskPayload{
		EventName:     "recording.completed",
		Meeting:       ZoomMeeting{ID: 42, Topic: "Hi", HostEmail: "h@c.com"},
		DownloadToken: "tok-abc",
		WriteMetadata: true,
	}
	body := mustMarshalTask(t, payload)
	rec := postProcessEvent(t, srv, body, "Bearer xyz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.String())
	}
	if called != 1 {
		t.Fatalf("processEventFn called %d times, want 1", called)
	}
	if gotMeeting.ID != 42 || gotMeeting.Topic != "Hi" {
		t.Errorf("meeting = %+v, want ID=42 Topic=Hi", gotMeeting)
	}
	if gotToken != "tok-abc" {
		t.Errorf("downloadToken = %q, want tok-abc", gotToken)
	}
	if !gotWriteMd {
		t.Errorf("writeMetadata = false, want true")
	}
}

func TestHandleProcessEvent_ZoomUnauthorized_Returns410Permanent(t *testing.T) {
	srv := newProcessEventTestServer()
	srv.processEventFn = func(ctx context.Context, meeting ZoomMeeting, downloadToken string, writeMetadata bool) error {
		return fmt.Errorf("stream someFile: %w", errZoomUnauthorized)
	}
	body := mustMarshalTask(t, TaskPayload{EventName: "recording.completed", Meeting: ZoomMeeting{ID: 1}})
	rec := postProcessEvent(t, srv, body, "Bearer xyz")
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410 (permanent, no retry); body = %q", rec.Code, rec.Body.String())
	}
}

func TestHandleProcessEvent_ProcessorError_Returns500Retryable(t *testing.T) {
	srv := newProcessEventTestServer()
	srv.processEventFn = func(ctx context.Context, meeting ZoomMeeting, downloadToken string, writeMetadata bool) error {
		return errors.New("transient drive API error")
	}
	body := mustMarshalTask(t, TaskPayload{EventName: "recording.completed", Meeting: ZoomMeeting{ID: 1}})
	rec := postProcessEvent(t, srv, body, "Bearer xyz")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %q", rec.Code, rec.Body.String())
	}
}

func TestHandleProcessEvent_MethodNotAllowed_OnGET(t *testing.T) {
	srv := newProcessEventTestServer()
	req := httptest.NewRequest(http.MethodGet, "/process-event", nil)
	rec := httptest.NewRecorder()
	srv.handleProcessEvent(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestStreamFileToDrive_Zoom401 confirms that a 401 from Zoom's
// download endpoint surfaces as the errZoomUnauthorized sentinel.
// processRecording relies on this to short-circuit the loop and return
// a permanent-failure error up to handleProcessEvent.
func TestStreamFileToDrive_Zoom401_ReturnsErrZoomUnauthorized(t *testing.T) {
	zoom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer zoom.Close()

	srv := &Server{cfg: &Config{}}
	file := RecordingFile{DownloadURL: zoom.URL}
	// driveSvc is nil — the 401 check aborts before Drive is touched.
	err := srv.streamFileToDrive(context.Background(), nil, file, "some-token", "parent", "filename.mp4")
	if !errors.Is(err, errZoomUnauthorized) {
		t.Fatalf("err = %v, want wrapped errZoomUnauthorized", err)
	}
}

// TestStreamFileToDrive_Zoom500 confirms that non-401 errors from Zoom
// return a plain error (NOT the errZoomUnauthorized sentinel). This
// matters for retry classification: handleProcessEvent returns 410
// permanent on errZoomUnauthorized but 500 retryable on everything
// else. Misclassifying a transient 500 as permanent would drop the task.
func TestStreamFileToDrive_Zoom500_ReturnsGenericError(t *testing.T) {
	zoom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer zoom.Close()

	srv := &Server{cfg: &Config{}}
	file := RecordingFile{DownloadURL: zoom.URL}
	err := srv.streamFileToDrive(context.Background(), nil, file, "tok", "parent", "name.mp4")
	if err == nil {
		t.Fatal("expected error on Zoom 500, got nil")
	}
	if errors.Is(err, errZoomUnauthorized) {
		t.Errorf("err = %v, want generic error (500 is not 401 — must not be mis-classified)", err)
	}
}

// TestStreamFileToDrive_NetworkError confirms that connection-level
// failures (not HTTP-level) surface as errors without being
// mis-classified as auth failures.
func TestStreamFileToDrive_ConnectionError_ReturnsGenericError(t *testing.T) {
	// Start and immediately stop a server so we have a guaranteed-unused
	// local URL that will reject connections.
	zoom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := zoom.URL
	zoom.Close()

	srv := &Server{cfg: &Config{}}
	file := RecordingFile{DownloadURL: deadURL}
	err := srv.streamFileToDrive(context.Background(), nil, file, "tok", "parent", "name.mp4")
	if err == nil {
		t.Fatal("expected error on connection refused, got nil")
	}
	if errors.Is(err, errZoomUnauthorized) {
		t.Errorf("err = %v, want generic error (network error is not 401)", err)
	}
}

// ----------------------------------------------------------------------------
// Tier 5: loadConfig
// ----------------------------------------------------------------------------

func TestLoadConfig_MissingProcessEventURL_ReturnsError(t *testing.T) {
	t.Setenv("ZOOM_WEBHOOK_SECRET_TOKEN", "x")
	t.Setenv("DRIVE_ROOT_FOLDER_ID", "x")
	t.Setenv("PROCESS_EVENT_URL", "")
	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig returned nil error when PROCESS_EVENT_URL is missing")
	}
	if !strings.Contains(err.Error(), "PROCESS_EVENT_URL") {
		t.Errorf("error = %q, want it to mention PROCESS_EVENT_URL", err.Error())
	}
}

func TestLoadConfig_AllRequiredPresent_Succeeds(t *testing.T) {
	t.Setenv("ZOOM_WEBHOOK_SECRET_TOKEN", "secret")
	t.Setenv("DRIVE_ROOT_FOLDER_ID", "folder")
	t.Setenv("PROCESS_EVENT_URL", "https://example.run.app/process-event")
	t.Setenv("CLOUD_TASKS_QUEUE", "projects/p/locations/l/queues/q")
	t.Setenv("TASKS_INVOKER_SA", "bot@p.iam.gserviceaccount.com")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ProcessEventURL != "https://example.run.app/process-event" {
		t.Errorf("ProcessEventURL = %q", cfg.ProcessEventURL)
	}
	if cfg.CloudTasksQueue != "projects/p/locations/l/queues/q" {
		t.Errorf("CloudTasksQueue = %q", cfg.CloudTasksQueue)
	}
	if cfg.TasksInvokerSA != "bot@p.iam.gserviceaccount.com" {
		t.Errorf("TasksInvokerSA = %q", cfg.TasksInvokerSA)
	}
	if cfg.ZoomWebhookSecret != "secret" {
		t.Errorf("ZoomWebhookSecret = %q, want %q", cfg.ZoomWebhookSecret, "secret")
	}
	if cfg.DriveRootFolderID != "folder" {
		t.Errorf("DriveRootFolderID = %q, want %q", cfg.DriveRootFolderID, "folder")
	}
}

func TestLoadConfig_MissingCloudTasksQueue_ReturnsError(t *testing.T) {
	t.Setenv("ZOOM_WEBHOOK_SECRET_TOKEN", "x")
	t.Setenv("DRIVE_ROOT_FOLDER_ID", "x")
	t.Setenv("PROCESS_EVENT_URL", "x")
	t.Setenv("CLOUD_TASKS_QUEUE", "")
	t.Setenv("TASKS_INVOKER_SA", "x")
	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig returned nil error when CLOUD_TASKS_QUEUE missing")
	}
	if !strings.Contains(err.Error(), "CLOUD_TASKS_QUEUE") {
		t.Errorf("error = %q, want CLOUD_TASKS_QUEUE mentioned", err.Error())
	}
}

func TestLoadConfig_MissingTasksInvokerSA_ReturnsError(t *testing.T) {
	t.Setenv("ZOOM_WEBHOOK_SECRET_TOKEN", "x")
	t.Setenv("DRIVE_ROOT_FOLDER_ID", "x")
	t.Setenv("PROCESS_EVENT_URL", "x")
	t.Setenv("CLOUD_TASKS_QUEUE", "x")
	t.Setenv("TASKS_INVOKER_SA", "")
	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig returned nil error when TASKS_INVOKER_SA missing")
	}
	if !strings.Contains(err.Error(), "TASKS_INVOKER_SA") {
		t.Errorf("error = %q, want TASKS_INVOKER_SA mentioned", err.Error())
	}
}

func TestLoadConfig_InProcessFakeMode_SkipsCloudTasksVars(t *testing.T) {
	t.Setenv("ZOOM_WEBHOOK_SECRET_TOKEN", "x")
	t.Setenv("DRIVE_ROOT_FOLDER_ID", "x")
	t.Setenv("PROCESS_EVENT_URL", "x")
	t.Setenv("CLOUD_TASKS_QUEUE", "")
	t.Setenv("TASKS_INVOKER_SA", "")
	t.Setenv("BRIDGE_IN_PROCESS_FAKE_TASKS", "1")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.InProcessFakeTasks {
		t.Errorf("InProcessFakeTasks = false, want true")
	}
}

// ----------------------------------------------------------------------------
// Tier 6: in-process fake enqueuer (test-only bypass)
// ----------------------------------------------------------------------------

// TestInProcessFakeEnqueuer_DispatchesToProcessEventURL verifies that
// the in-process fake actually POSTs the serialized payload to the
// configured URL with a non-empty Authorization header. This is the
// seam the synthetic test driver relies on.
func TestInProcessFakeEnqueuer_DispatchesToProcessEventURL(t *testing.T) {
	received := make(chan []byte, 1)
	gotAuth := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotAuth <- r.Header.Get("Authorization")
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	enq := newInProcessFakeEnqueuer(target.URL)
	payload := TaskPayload{EventName: "recording.completed", DownloadToken: "abc"}
	if err := enq.Enqueue(context.Background(), payload); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	select {
	case body := <-received:
		var got TaskPayload
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal dispatched body: %v", err)
		}
		if got.EventName != "recording.completed" || got.DownloadToken != "abc" {
			t.Errorf("dispatched payload = %+v, want EventName=recording.completed DownloadToken=abc", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for fake enqueuer dispatch")
	}

	if auth := <-gotAuth; !strings.HasPrefix(auth, "Bearer ") {
		t.Errorf("Authorization header = %q, want 'Bearer ...'", auth)
	}
}

func TestPassThroughTokenValidator_AcceptsAnything(t *testing.T) {
	v := passThroughTokenValidator{}
	if err := v.Validate(context.Background(), "", ""); err != nil {
		t.Errorf("Validate with empty inputs should not error, got %v", err)
	}
	if err := v.Validate(context.Background(), "something", "something"); err != nil {
		t.Errorf("Validate with values should not error, got %v", err)
	}
}
