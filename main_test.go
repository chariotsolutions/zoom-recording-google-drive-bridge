package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
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

func TestHandleWebhook_TranscriptUnsignedRejected(t *testing.T) {
	srv := newTestServer()
	body := []byte(`{"event":"recording.transcript_completed","payload":{"object":{}}}`)

	rec := postWebhook(t, srv, body, false, "")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
}

func TestMeetingLockSerializesSameMeeting(t *testing.T) {
	srv := newTestServer()
	// Same meeting ID → same mutex
	lock1 := srv.meetingLock(42)
	lock2 := srv.meetingLock(42)
	if lock1 != lock2 {
		t.Errorf("expected same mutex for same meeting ID")
	}
	// Different meeting ID → different mutex
	lock3 := srv.meetingLock(43)
	if lock1 == lock3 {
		t.Errorf("expected different mutex for different meeting ID")
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

// Diagnostic helper used in failing tests to dump useful info.
//nolint:unused
func dumpRequest(t *testing.T, prefix string, rec *httptest.ResponseRecorder) {
	t.Helper()
	t.Logf("%s: status=%d body=%s", prefix, rec.Code, rec.Body.String())
}

// Compile-time check we still use fmt (kept available for future tests).
var _ = fmt.Sprintf
