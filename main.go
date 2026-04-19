// Package main implements an HTTP service that receives Zoom recording.completed
// webhooks, downloads the recording files from Zoom, and streams them directly
// into Google Drive without buffering the entire file in memory.
//
// Designed to run on Google Cloud Run.
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
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"golang.org/x/time/rate"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/durationpb"
)

// errZoomUnauthorized indicates Zoom rejected the per-event download_token
// when fetching a recording file (HTTP 401). This is treated as a permanent
// failure by handleProcessEvent — retrying with the same download_token will
// fail the same way, and the token itself can't be refreshed (Zoom issues a
// new one with each webhook event). Zoom documents the token as valid for
// ~24 hours.
var errZoomUnauthorized = errors.New("zoom rejected download_token (HTTP 401)")

// rateLimitRPS and rateLimitBurst are the global token-bucket parameters
// for /webhook. Both are well above legitimate Zoom traffic (events
// arrive seconds apart at most, a handful per day), so the limiter only
// trips under abusive load. Hardcoded for v1 — see issue #6.
const (
	rateLimitRPS   = 10
	rateLimitBurst = 20
)

// ----------------------------------------------------------------------------
// Configuration
// ----------------------------------------------------------------------------

type Config struct {
	ZoomWebhookSecret string
	DriveRootFolderID string
	Port              string

	// ProcessEventURL is the public URL Cloud Tasks calls to dispatch a
	// task to /process-event. Used as the OIDC audience claim that
	// /process-event's TokenValidator verifies, and as the target URL
	// on enqueued tasks.
	ProcessEventURL string

	// CloudTasksQueue is the full resource name of the Cloud Tasks
	// queue to enqueue into, e.g.
	// projects/PROJECT/locations/REGION/queues/QUEUE_NAME. Loaded from
	// CLOUD_TASKS_QUEUE env var.
	CloudTasksQueue string

	// TasksInvokerSA is the email of the service account that Cloud
	// Tasks impersonates when signing OIDC bearer tokens for
	// /process-event dispatches. Typically the same service account
	// the bridge runs as on Cloud Run. Loaded from TASKS_INVOKER_SA.
	TasksInvokerSA string
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		ZoomWebhookSecret: os.Getenv("ZOOM_WEBHOOK_SECRET_TOKEN"),
		DriveRootFolderID: os.Getenv("DRIVE_ROOT_FOLDER_ID"),
		Port:              getEnvDefault("PORT", "8080"),
		ProcessEventURL:   os.Getenv("PROCESS_EVENT_URL"),
		CloudTasksQueue:   os.Getenv("CLOUD_TASKS_QUEUE"),
		TasksInvokerSA:    os.Getenv("TASKS_INVOKER_SA"),
	}

	missing := []string{}
	if cfg.ZoomWebhookSecret == "" {
		missing = append(missing, "ZOOM_WEBHOOK_SECRET_TOKEN")
	}
	if cfg.DriveRootFolderID == "" {
		missing = append(missing, "DRIVE_ROOT_FOLDER_ID")
	}
	if cfg.ProcessEventURL == "" {
		missing = append(missing, "PROCESS_EVENT_URL")
	}
	if cfg.CloudTasksQueue == "" {
		missing = append(missing, "CLOUD_TASKS_QUEUE")
	}
	if cfg.TasksInvokerSA == "" {
		missing = append(missing, "TASKS_INVOKER_SA")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ----------------------------------------------------------------------------
// Zoom webhook payload types
// ----------------------------------------------------------------------------

type ZoomWebhookEvent struct {
	Event         string          `json:"event"`
	EventTS       int64           `json:"event_ts"`
	Payload       json.RawMessage `json:"payload"`
	DownloadToken string          `json:"download_token"`
}

type ZoomValidationPayload struct {
	PlainToken string `json:"plainToken"`
}

type ZoomRecordingPayload struct {
	AccountID string      `json:"account_id"`
	Object    ZoomMeeting `json:"object"`
}

type ZoomMeeting struct {
	ID             int64           `json:"id"`
	UUID           string          `json:"uuid"`
	HostID         string          `json:"host_id"`
	HostEmail      string          `json:"host_email"`
	Topic          string          `json:"topic"`
	StartTime      string          `json:"start_time"`
	Duration       int             `json:"duration"`
	RecordingFiles []RecordingFile `json:"recording_files"`
}

type RecordingFile struct {
	ID             string `json:"id"`
	MeetingID      string `json:"meeting_id"`
	RecordingStart string `json:"recording_start"`
	RecordingEnd   string `json:"recording_end"`
	FileType       string `json:"file_type"`
	FileExtension  string `json:"file_extension"`
	FileSize       int64  `json:"file_size"`
	PlayURL        string `json:"play_url"`
	DownloadURL    string `json:"download_url"`
	RecordingType  string `json:"recording_type"`
	Status         string `json:"status"`
}

// ----------------------------------------------------------------------------
// Cloud Tasks primitives
// ----------------------------------------------------------------------------

// TaskPayload is the JSON body of a Cloud Tasks task that triggers
// /process-event. It carries everything processRecording needs to
// download files from Zoom and upload them to Drive, independent of
// the original webhook request that enqueued the task. The payload is
// a few KB — well under Cloud Tasks' 1 MB per-task limit.
type TaskPayload struct {
	EventName     string      `json:"event_name"`
	Meeting       ZoomMeeting `json:"meeting"`
	DownloadToken string      `json:"download_token"`
	WriteMetadata bool        `json:"write_metadata"`
}

// TaskEnqueuer abstracts the dependency on Cloud Tasks so handleWebhook
// can be tested without hitting the real service. Production uses a
// cloud.google.com/go/cloudtasks-backed implementation; tests use a
// fake that captures what was enqueued.
type TaskEnqueuer interface {
	Enqueue(ctx context.Context, payload TaskPayload) error
}

// TokenValidator abstracts OIDC bearer token verification so
// handleProcessEvent can be tested without real Google-signed tokens.
// Production uses a google.golang.org/api/idtoken-backed implementation
// (idtokenValidator below); the synthetic test driver (running against
// the Cloud Tasks emulator, which does not sign OIDC tokens) uses a
// fake pass-through validator.
type TokenValidator interface {
	Validate(ctx context.Context, token string, audience string) error
}

// idtokenValidator is the production TokenValidator. It wraps
// google.golang.org/api/idtoken's Validator to conform to our
// TokenValidator interface. The Validator fetches Google's public
// OIDC signing keys (cached) to verify tokens signed by Cloud Tasks'
// service account.
type idtokenValidator struct {
	inner *idtoken.Validator
}

func newIDTokenValidator(ctx context.Context) (*idtokenValidator, error) {
	v, err := idtoken.NewValidator(ctx)
	if err != nil {
		return nil, err
	}
	return &idtokenValidator{inner: v}, nil
}

func (v *idtokenValidator) Validate(ctx context.Context, token string, audience string) error {
	_, err := v.inner.Validate(ctx, token, audience)
	return err
}

// cloudTasksEnqueuer is the production TaskEnqueuer. It creates tasks
// on a specific queue that Cloud Tasks will dispatch to
// cfg.ProcessEventURL with an OIDC token signed for
// cfg.TasksInvokerSA.
type cloudTasksEnqueuer struct {
	client           *cloudtasks.Client
	queuePath        string
	processEventURL  string
	invokerSA        string
	dispatchDeadline time.Duration
}

func newCloudTasksEnqueuer(ctx context.Context, cfg *Config) (*cloudTasksEnqueuer, error) {
	client, err := cloudtasks.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create cloud tasks client: %w", err)
	}
	return &cloudTasksEnqueuer{
		client:           client,
		queuePath:        cfg.CloudTasksQueue,
		processEventURL:  cfg.ProcessEventURL,
		invokerSA:        cfg.TasksInvokerSA,
		dispatchDeadline: 30 * time.Minute,
	}, nil
}

func (e *cloudTasksEnqueuer) Enqueue(ctx context.Context, payload TaskPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal task payload: %w", err)
	}
	req := &taskspb.CreateTaskRequest{
		Parent: e.queuePath,
		Task: &taskspb.Task{
			MessageType: &taskspb.Task_HttpRequest{
				HttpRequest: &taskspb.HttpRequest{
					HttpMethod: taskspb.HttpMethod_POST,
					Url:        e.processEventURL,
					Body:       body,
					Headers:    map[string]string{"Content-Type": "application/json"},
					AuthorizationHeader: &taskspb.HttpRequest_OidcToken{
						OidcToken: &taskspb.OidcToken{
							ServiceAccountEmail: e.invokerSA,
							Audience:            e.processEventURL,
						},
					},
				},
			},
			DispatchDeadline: durationpb.New(e.dispatchDeadline),
		},
	}
	if _, err := e.client.CreateTask(ctx, req); err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// Server
// ----------------------------------------------------------------------------

type Server struct {
	cfg *Config

	// taskEnqueuer creates Cloud Tasks tasks that will trigger
	// /process-event. handleWebhook calls this instead of spawning a
	// goroutine. Nil until wired in main() (production) or a test.
	taskEnqueuer TaskEnqueuer

	// tokenValidator verifies OIDC bearer tokens on /process-event
	// requests. Nil until wired in main() (production) or a test.
	tokenValidator TokenValidator

	// processEventFn is the per-event work function invoked by
	// handleProcessEvent. Defaults to (*Server).processRecording in
	// production; tests substitute a stub. Using a function field
	// rather than a method keeps the test seam small without
	// introducing a whole-processor interface.
	processEventFn func(ctx context.Context, meeting ZoomMeeting, downloadToken string, writeMetadata bool) error

	// meetingLocks serializes processRecording calls that share the same
	// meeting ID. Removed in a later commit once Cloud Tasks'
	// max-concurrent-dispatches=1 provides the same guarantee at the
	// queue layer (see issue #8).
	//
	// See docs/design-decisions.md "Decision 1" for the alternatives we
	// considered and why we landed on this approach.
	meetingLocks sync.Map
}

// meetingLock returns the mutex for a given meeting ID, creating one on
// first use. Safe for concurrent use.
func (s *Server) meetingLock(meetingID int64) *sync.Mutex {
	m, _ := s.meetingLocks.LoadOrStore(meetingID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	ctx := context.Background()

	validator, err := newIDTokenValidator(ctx)
	if err != nil {
		log.Fatalf("create id token validator: %v", err)
	}
	enqueuer, err := newCloudTasksEnqueuer(ctx, cfg)
	if err != nil {
		log.Fatalf("create cloud tasks enqueuer: %v", err)
	}

	srv := &Server{
		cfg:            cfg,
		taskEnqueuer:   enqueuer,
		tokenValidator: validator,
	}
	// Wire the per-event work function to the real implementation.
	// Tests substitute a stub directly on the field.
	srv.processEventFn = srv.processRecording

	// Single process-wide limiter shared across all /webhook requests.
	// Rejected requests return 429 before any body read or HMAC work.
	limiter := rate.NewLimiter(rate.Limit(rateLimitRPS), rateLimitBurst)

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleRoot)
	mux.Handle("/webhook", rateLimitMiddleware(limiter, http.HandlerFunc(srv.handleWebhook)))
	// /process-event is invoked by Cloud Tasks with an OIDC bearer token.
	// Not rate-limited — it is only reachable to holders of a valid
	// Google-signed token for our service account.
	mux.HandleFunc("/process-event", srv.handleProcessEvent)

	log.Printf("listening on :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Zoom recording → Google Drive bridge is running.")
}

// rateLimitMiddleware wraps an http.Handler with a token-bucket rate
// limiter. Requests that exceed the limit are rejected with 429 before
// the wrapped handler runs — so signature verification, body reads, and
// Drive work are all skipped on rejected requests.
//
// The limiter is global (not per-IP) by design: at Chariot's webhook
// volume a global limiter never touches legitimate traffic, and per-IP
// tracking would add state and complexity for marginal benefit. Per-IP
// enforcement is Cloud Armor's job — see
// docs/security-and-network-options.md.
func rateLimitMiddleware(limiter *rate.Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Allow() {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleWebhook receives Zoom webhook POSTs.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Cap the body at 1 MB to prevent oversized payloads from burning
	// memory before the signature check runs. Zoom's recording.completed
	// payloads are a few KB at most; 1 MB is generous for any legitimate
	// event. Truncated bodies will fail json.Unmarshal → 400.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var evt ZoomWebhookEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// The URL validation handshake is the bootstrap event and is NOT signed
	// by Zoom — it's the proof that we know the secret in the first place.
	if evt.Event == "endpoint.url_validation" {
		s.handleValidation(w, evt.Payload)
		return
	}

	// All other events must carry a valid signature.
	timestamp := r.Header.Get("x-zm-request-timestamp")
	signature := r.Header.Get("x-zm-signature")
	if !verifyZoomSignature(s.cfg.ZoomWebhookSecret, timestamp, signature, body) {
		log.Printf("signature verification failed event=%s", evt.Event)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch evt.Event {
	case "recording.completed", "recording.transcript_completed":
		var payload ZoomRecordingPayload
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			log.Printf("invalid %s payload: %v", evt.Event, err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}

		if evt.DownloadToken == "" {
			log.Printf("%s missing download_token meetingID=%d", evt.Event, payload.Object.ID)
			w.WriteHeader(http.StatusOK)
			return
		}

		// Enqueue a Cloud Tasks task that will trigger /process-event
		// in a separate inbound request. This lets the slow Zoom →
		// Drive upload run under an active request lifecycle (full CPU,
		// no instance reaping) instead of a background goroutine that
		// Cloud Run can't see. See issue #8.
		taskPayload := TaskPayload{
			EventName:     evt.Event,
			Meeting:       payload.Object,
			DownloadToken: evt.DownloadToken,
			// Only recording.completed writes meeting-metadata.json;
			// recording.transcript_completed fires separately for the
			// same meeting and must not write a second metadata file.
			WriteMetadata: evt.Event == "recording.completed",
		}
		if err := s.taskEnqueuer.Enqueue(r.Context(), taskPayload); err != nil {
			log.Printf("enqueue task event=%s meetingID=%d: %v", evt.Event, payload.Object.ID, err)
			// 5xx so Zoom retries — our downstream can't be reached yet.
			http.Error(w, "enqueue failed", http.StatusInternalServerError)
			return
		}
		log.Printf("enqueued task event=%s meetingID=%d files=%d",
			evt.Event, payload.Object.ID, len(payload.Object.RecordingFiles))

		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
		return

	default:
		log.Printf("unhandled event: %s", evt.Event)
		w.WriteHeader(http.StatusOK)
	}
}

// ----------------------------------------------------------------------------
// Zoom URL validation handshake
// ----------------------------------------------------------------------------

func (s *Server) handleValidation(w http.ResponseWriter, raw json.RawMessage) {
	var p ZoomValidationPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		http.Error(w, "invalid validation payload", http.StatusBadRequest)
		return
	}

	mac := hmac.New(sha256.New, []byte(s.cfg.ZoomWebhookSecret))
	mac.Write([]byte(p.PlainToken))
	encrypted := hex.EncodeToString(mac.Sum(nil))

	resp := map[string]string{
		"plainToken":     p.PlainToken,
		"encryptedToken": encrypted,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)

	log.Println("validation challenge handled")
}

// ----------------------------------------------------------------------------
// /process-event: Cloud Tasks callback
// ----------------------------------------------------------------------------

// extractBearerToken pulls the token out of an Authorization: Bearer <token>
// header. Returns an error if the header is missing or malformed.
func extractBearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", errors.New("missing Authorization header")
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", errors.New("malformed Authorization header (expected 'Bearer <token>')")
	}
	return parts[1], nil
}

// handleProcessEvent is invoked by Cloud Tasks with a TaskPayload body
// and an OIDC bearer token in the Authorization header. It verifies the
// token, parses the payload, and runs the per-event work synchronously
// inside the request lifecycle (so Cloud Run keeps full CPU allocated
// for the duration — the whole point of the rearchitecture).
//
// Response status codes communicate retry semantics back to Cloud Tasks:
//   - 200 — task succeeded, no retry
//   - 400 — malformed request body (will not retry on 4xx by default)
//   - 401 — missing / invalid OIDC (will not retry on 4xx by default)
//   - 410 — Zoom rejected the download_token (permanent; do not retry)
//   - 500 — other processing error (Cloud Tasks retries per queue config)
//
// Cloud Tasks default retry policy retries 5xx and network errors; 4xx
// responses (except 408/429) are treated as permanent failures.
func (s *Server) handleProcessEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token, err := extractBearerToken(r)
	if err != nil {
		log.Printf("process-event: %v", err)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if err := s.tokenValidator.Validate(r.Context(), token, s.cfg.ProcessEventURL); err != nil {
		log.Printf("process-event token validation failed: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Cap the body at 1 MB. Real task payloads are a few KB; 1 MB is
	// generous and matches the cap on /webhook.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	var payload TaskPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("process-event: invalid task payload: %v", err)
		http.Error(w, "invalid task payload", http.StatusBadRequest)
		return
	}

	log.Printf("process-event: starting event=%s meetingID=%d files=%d writeMetadata=%v",
		payload.EventName, payload.Meeting.ID, len(payload.Meeting.RecordingFiles), payload.WriteMetadata)

	err = s.processEventFn(r.Context(), payload.Meeting, payload.DownloadToken, payload.WriteMetadata)
	if err != nil {
		if errors.Is(err, errZoomUnauthorized) {
			// Permanent failure — the download_token is no longer
			// valid and retries can't recover it.
			log.Printf("process-event: permanent failure event=%s meetingID=%d: %v",
				payload.EventName, payload.Meeting.ID, err)
			http.Error(w, "zoom download_token rejected (permanent)", http.StatusGone)
			return
		}
		log.Printf("process-event: transient failure event=%s meetingID=%d: %v",
			payload.EventName, payload.Meeting.ID, err)
		http.Error(w, "processing failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "OK")
}

// ----------------------------------------------------------------------------
// Signature verification
// ----------------------------------------------------------------------------

// verifyZoomSignature checks the x-zm-signature header against an HMAC-SHA256
// of "v0:<timestamp>:<body>" using the webhook secret. Returns true on match.
//
// It also rejects timestamps more than 5 minutes from now to prevent replay
// of intercepted requests.
func verifyZoomSignature(secret, timestamp, signatureHeader string, body []byte) bool {
	if secret == "" || timestamp == "" || signatureHeader == "" {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	now := time.Now().Unix()
	if diff := now - ts; diff > 300 || diff < -300 {
		return false
	}
	msg := "v0:" + timestamp + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signatureHeader))
}

// ----------------------------------------------------------------------------
// Recording download → Google Drive upload (streaming)
// ----------------------------------------------------------------------------

func (s *Server) processRecording(ctx context.Context, meeting ZoomMeeting, downloadToken string, writeMetadata bool) error {
	// Serialize concurrent processRecording calls for the same meeting.
	// This closes the race window between recording.completed and
	// recording.transcript_completed, which Zoom may deliver within
	// milliseconds of each other (or even out of order).
	lock := s.meetingLock(meeting.ID)
	lock.Lock()
	defer lock.Unlock()

	log.Printf("processing recording: topic=%q meetingID=%d host=%s files=%d writeMetadata=%v",
		meeting.Topic, meeting.ID, meeting.HostEmail, len(meeting.RecordingFiles), writeMetadata)

	// Initialize Drive client (uses Application Default Credentials in Cloud Run)
	driveSvc, err := drive.NewService(ctx, option.WithScopes(drive.DriveScope))
	if err != nil {
		return fmt.Errorf("create drive client: %w", err)
	}

	// Folder structure: <root>/<host>/<YYYY-MM-DDThh-mm>-<topic>/raw/
	hostFolder := hostUsername(meeting.HostEmail)
	mfn := buildMeetingFolderName(meeting)

	hostFolderID, err := getOrCreateFolder(driveSvc, s.cfg.DriveRootFolderID, hostFolder)
	if err != nil {
		return fmt.Errorf("create host folder: %w", err)
	}
	meetingFolderID, err := getOrCreateFolder(driveSvc, hostFolderID, mfn)
	if err != nil {
		return fmt.Errorf("create meeting folder: %w", err)
	}
	rawFolderID, err := getOrCreateFolder(driveSvc, meetingFolderID, "raw")
	if err != nil {
		return fmt.Errorf("create raw folder: %w", err)
	}

	// Stream each recording file from Zoom directly into Drive
	uploaded := 0
	for _, file := range meeting.RecordingFiles {
		if file.Status != "completed" {
			log.Printf("skipping file %s (status=%s)", file.ID, file.Status)
			continue
		}
		filename := buildFilename(meeting.Topic, file)
		if err := s.streamFileToDrive(ctx, driveSvc, file, downloadToken, rawFolderID, filename); err != nil {
			// Zoom 401 means the download_token is invalid (expired,
			// revoked, or wrong). All remaining files in this event
			// will fail the same way, and retries will fail the same
			// way. Return the sentinel so handleProcessEvent can
			// translate it to a permanent-failure response to Cloud
			// Tasks.
			if errors.Is(err, errZoomUnauthorized) {
				return fmt.Errorf("stream %s: %w", filename, err)
			}
			log.Printf("stream %s failed: %v", filename, err)
			continue
		}
		uploaded++
		log.Printf("uploaded: %s", filename)
	}

	// Write metadata file only on the initial recording.completed event.
	// The transcript event fires separately for the same meeting; writing
	// metadata twice would produce a second file with misleading counts.
	if writeMetadata {
		metadata := map[string]any{
			"topic":                            meeting.Topic,
			"start_time":                       meeting.StartTime,
			"host_email":                       meeting.HostEmail,
			"meeting_id":                       meeting.ID,
			"duration":                         meeting.Duration,
			"files_uploaded":                   uploaded,
			"total_files":                      len(meeting.RecordingFiles),
			"processed_at":                     time.Now().UTC().Format(time.RFC3339),
			"transcript_may_arrive_separately": true,
		}
		metaJSON, _ := json.MarshalIndent(metadata, "", "  ")
		_, err = driveSvc.Files.Create(&drive.File{
			Name:     "meeting-metadata.json",
			Parents:  []string{meetingFolderID},
			MimeType: "application/json",
		}).
			Media(bytes.NewReader(metaJSON)).
			SupportsAllDrives(true).
			Do()
		if err != nil {
			log.Printf("write metadata: %v", err)
		}
	}

	log.Printf("done: %d/%d files uploaded to %s/%s", uploaded, len(meeting.RecordingFiles), hostFolder, mfn)
	return nil
}

// streamFileToDrive downloads a single recording file from Zoom and streams it
// directly into a Google Drive upload, without buffering the whole file.
//
// Authentication is via the per-event download_token from the webhook payload,
// passed as the access_token query parameter on the download URL. This is the
// auth model Zoom requires for webhook-delivered recording files; it is NOT the
// same as the Server-to-Server OAuth access token used for the Zoom REST API.
func (s *Server) streamFileToDrive(
	ctx context.Context,
	driveSvc *drive.Service,
	file RecordingFile,
	downloadToken string,
	parentFolderID string,
	filename string,
) error {
	signedURL, err := buildSignedDownloadURL(file.DownloadURL, downloadToken)
	if err != nil {
		return fmt.Errorf("build signed url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	// No Authorization header — auth is via the access_token query param.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%s: %w", filename, errZoomUnauthorized)
		}
		return fmt.Errorf("download status %d", resp.StatusCode)
	}

	// Stream the response body directly into Drive's resumable upload.
	// drive.Files.Create().Media(reader) handles chunking internally.
	driveFile := &drive.File{
		Name:    filename,
		Parents: []string{parentFolderID},
	}

	_, err = driveSvc.Files.Create(driveFile).
		Media(resp.Body).
		Context(ctx).
		SupportsAllDrives(true).
		Do()
	if err != nil {
		return fmt.Errorf("drive upload: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// Google Drive helpers
// ----------------------------------------------------------------------------

func getOrCreateFolder(svc *drive.Service, parentID, name string) (string, error) {
	// Search for an existing folder with this name under parentID.
	//
	// SupportsAllDrives + IncludeItemsFromAllDrives are required for the
	// query to see items inside Shared Drives (Workspace shared drives).
	// Without them, the API silently returns no results for Shared Drive
	// items, which presents as a 404 on subsequent operations — a real
	// production bug we caught via the synthetic test driver before our
	// first deploy. See docs/design-decisions.md "Lesson 2" for the full
	// story.
	query := fmt.Sprintf("mimeType='application/vnd.google-apps.folder' and name='%s' and '%s' in parents and trashed=false",
		escapeQuery(name), parentID)

	list, err := svc.Files.List().
		Q(query).
		Fields("files(id, name)").
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true).
		Do()
	if err != nil {
		return "", err
	}
	if len(list.Files) > 0 {
		return list.Files[0].Id, nil
	}

	// Create it
	folder := &drive.File{
		Name:     name,
		MimeType: "application/vnd.google-apps.folder",
		Parents:  []string{parentID},
	}
	created, err := svc.Files.Create(folder).
		Fields("id").
		SupportsAllDrives(true).
		Do()
	if err != nil {
		return "", err
	}
	return created.Id, nil
}

// ----------------------------------------------------------------------------
// Utility functions
// ----------------------------------------------------------------------------

func parseStartDate(startTime string) string {
	if startTime == "" {
		return "unknown-date"
	}
	t, err := time.Parse(time.RFC3339, startTime)
	if err != nil {
		return "unknown-date"
	}
	return t.Format("2006-01-02")
}

// buildMeetingFolderName produces a folder name that includes both the
// date and the UTC start time of the meeting, so two meetings with the
// same topic on the same day get distinct folders. Format:
//
//	2026-04-11T18-00-Weekly Standup
//
// The time is always UTC (Zoom's start_time is always UTC regardless of
// the host's timezone). Using UTC avoids timezone conversion code and
// keeps the name deterministic. If the start_time is missing or
// malformed, falls back to date-only (which is still better than nothing).
//
// NOTE: cmd/synthetic-test/main.go has a mirror of this function called
// buildExpectedMeetingFolderName. Keep them in sync.
func buildMeetingFolderName(meeting ZoomMeeting) string {
	topic := sanitizeFilename(meeting.Topic)
	if topic == "" {
		topic = "Untitled Meeting"
	}
	if meeting.StartTime == "" {
		return "unknown-date-" + topic
	}
	t, err := time.Parse(time.RFC3339, meeting.StartTime)
	if err != nil {
		return "unknown-date-" + topic
	}
	t = t.UTC()
	return fmt.Sprintf("%sT%s-%s", t.Format("2006-01-02"), t.Format("15-04"), topic)
}

func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", "?", "-", "%", "-", "*", "-",
		":", "-", "|", "-", "\"", "-", "<", "-", ">", "-",
	)
	out := replacer.Replace(name)
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// hostUsername extracts a folder-safe username from a Zoom host email.
// Returns the lowercased local part of the email (everything before the
// last @), sanitized to remove characters that would be problematic in
// a folder name. Returns "unknown-host" if the email is empty, has no
// @ sign, or sanitizes to an empty string.
//
// Lowercasing matters because Drive folder names are case-sensitive —
// without it, "Foo@bar.com" and "foo@bar.com" would create two folders
// for the same human.
//
// NOTE: cmd/synthetic-test/main.go has a mirror of this function called
// localPartFromEmail. Keep them in sync.
func hostUsername(hostEmail string) string {
	hostEmail = strings.TrimSpace(hostEmail)
	if hostEmail == "" {
		return "unknown-host"
	}
	at := strings.LastIndex(hostEmail, "@")
	if at <= 0 {
		return "unknown-host"
	}
	local := strings.ToLower(hostEmail[:at])
	sanitized := sanitizeFilename(local)
	if sanitized == "" {
		return "unknown-host"
	}
	return sanitized
}

func escapeQuery(s string) string {
	// Escape single quotes for Drive API query
	return strings.ReplaceAll(s, "'", "\\'")
}

func buildFilename(topic string, file RecordingFile) string {
	ext := strings.ToLower(file.FileExtension)
	if ext == "" {
		ext = guessExtension(file.FileType)
	}
	name := fmt.Sprintf("%s-%s.%s", topic, file.RecordingType, ext)
	return sanitizeFilename(name)
}

// buildSignedDownloadURL appends the Zoom webhook download_token as the
// access_token query parameter on the download URL. If access_token is
// already present in the URL, it is replaced.
func buildSignedDownloadURL(downloadURL, downloadToken string) (string, error) {
	u, err := url.Parse(downloadURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("access_token", downloadToken)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func guessExtension(fileType string) string {
	switch strings.ToUpper(fileType) {
	case "MP4":
		return "mp4"
	case "M4A":
		return "m4a"
	case "TRANSCRIPT", "CC":
		return "vtt"
	case "CHAT":
		return "txt"
	case "CSV":
		return "csv"
	case "TIMELINE":
		return "json"
	case "SUMMARY":
		return "vtt"
	default:
		return strings.ToLower(fileType)
	}
}
