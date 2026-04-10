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
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// ----------------------------------------------------------------------------
// Configuration
// ----------------------------------------------------------------------------

type Config struct {
	ZoomWebhookSecret string
	DriveRootFolderID string
	Port              string
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		ZoomWebhookSecret: os.Getenv("ZOOM_WEBHOOK_SECRET_TOKEN"),
		DriveRootFolderID: os.Getenv("DRIVE_ROOT_FOLDER_ID"),
		Port:              getEnvDefault("PORT", "8080"),
	}

	missing := []string{}
	if cfg.ZoomWebhookSecret == "" {
		missing = append(missing, "ZOOM_WEBHOOK_SECRET_TOKEN")
	}
	if cfg.DriveRootFolderID == "" {
		missing = append(missing, "DRIVE_ROOT_FOLDER_ID")
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
	AccountID string         `json:"account_id"`
	Object    ZoomMeeting    `json:"object"`
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
// Server
// ----------------------------------------------------------------------------

type Server struct {
	cfg *Config
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	srv := &Server{cfg: cfg}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleRoot)
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/webhook", srv.handleWebhook)

	log.Printf("listening on :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Zoom recording → Google Drive bridge is running.")
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "ok")
}

// handleWebhook receives Zoom webhook POSTs.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
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
	case "recording.completed":
		var payload ZoomRecordingPayload
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			log.Printf("invalid recording payload: %v", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}

		if evt.DownloadToken == "" {
			log.Printf("recording.completed missing download_token meetingID=%d", payload.Object.ID)
			w.WriteHeader(http.StatusOK)
			return
		}

		// Capture by value before launching goroutine.
		downloadToken := evt.DownloadToken
		meeting := payload.Object

		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")

		go func() {
			ctx := context.Background()
			if err := s.processRecording(ctx, meeting, downloadToken); err != nil {
				log.Printf("processRecording error meetingID=%d: %v", meeting.ID, err)
			}
		}()
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

func (s *Server) processRecording(ctx context.Context, meeting ZoomMeeting, downloadToken string) error {
	log.Printf("processing recording: topic=%q meetingID=%d host=%s files=%d",
		meeting.Topic, meeting.ID, meeting.HostEmail, len(meeting.RecordingFiles))

	// Initialize Drive client (uses Application Default Credentials in Cloud Run)
	driveSvc, err := drive.NewService(ctx, option.WithScopes(drive.DriveScope))
	if err != nil {
		return fmt.Errorf("create drive client: %w", err)
	}

	// Folder structure: <root>/<YYYY-MM-DD>-<topic>/raw/
	dateStr := parseStartDate(meeting.StartTime)
	folderName := fmt.Sprintf("%s-%s", dateStr, sanitizeFilename(meeting.Topic))

	meetingFolderID, err := getOrCreateFolder(driveSvc, s.cfg.DriveRootFolderID, folderName)
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
			log.Printf("stream %s failed: %v", filename, err)
			continue
		}
		uploaded++
		log.Printf("uploaded: %s", filename)
	}

	// Write metadata file
	metadata := map[string]any{
		"topic":           meeting.Topic,
		"start_time":      meeting.StartTime,
		"host_email":      meeting.HostEmail,
		"meeting_id":      meeting.ID,
		"duration":        meeting.Duration,
		"files_uploaded":  uploaded,
		"total_files":     len(meeting.RecordingFiles),
		"processed_at":    time.Now().UTC().Format(time.RFC3339),
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

	log.Printf("done: %d/%d files uploaded to %s", uploaded, len(meeting.RecordingFiles), folderName)
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
	// items, which presents as a 404 on subsequent operations.
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
