// synthetic-test sends synthetic Zoom webhook events to a running
// zoom-recording-google-drive-bridge instance and verifies that the recording
// files land in Google Drive.
//
// The driver emulates Zoom's real two-event delivery model:
//
//  1. recording.completed → MP4 video, M4A audio, timeline.json
//  2. recording.transcript_completed → VTT transcript
//
// Both events use the same meeting ID so they exercise the bridge's
// per-meeting serialization lock.
//
// Usage:
//
//	export ZOOM_WEBHOOK_SECRET_TOKEN=test_secret_local
//	export DRIVE_ROOT_FOLDER_ID=<your_drive_folder_id>
//	export GOOGLE_APPLICATION_CREDENTIALS=/path/to/service-account.json
//
//	# Terminal 1: start the bridge
//	go run .
//
//	# Terminal 2: run the synthetic test
//	go run ./cmd/synthetic-test
//
// Flags:
//
//	--bridge-url        URL of the bridge's webhook endpoint
//	--topic             Meeting topic (default: "Synthetic Test <unix-time>")
//	--reverse-order     Send transcript event BEFORE recording event (tests
//	                    the per-meeting lock's handling of out-of-order
//	                    delivery; Zoom does this ~7ms before recording.completed
//	                    in some real observations)
//	--skip-verify       Just POST both events, skip Drive verification
//	--poll-timeout      How long to poll Drive before giving up (default 60s)
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// runConfig holds the parsed flags and resolved env vars for a single
// driver run.
type runConfig struct {
	bridgeURL    string
	topic        string
	hostEmail    string
	reverseOrder bool
	skipVerify   bool
	pollTimeout  time.Duration
	secret       string
	rootFolderID string
	startTime    time.Time
	meetingID    int64
}

func main() {
	cfg := parseFlagsAndEnv()

	recordingFiles, transcriptFiles := defaultSyntheticFiles()
	allFiles := append(append([]SyntheticFile{}, recordingFiles...), transcriptFiles...)

	fake, err := startFakeServer(allFiles)
	if err != nil {
		exitf("start fake server: %v", err)
	}
	defer fake.Close()
	fmt.Printf("[driver] fake Zoom file server: %s\n", fake.URL())
	fmt.Printf("[driver] meetingID=%d topic=%q\n", cfg.meetingID, cfg.topic)

	if err := sendEvents(cfg, fake.URL(), recordingFiles, transcriptFiles); err != nil {
		exitf("%v", err)
	}

	if cfg.skipVerify {
		fmt.Println("[driver] --skip-verify set; not checking Drive")
		fmt.Println("[driver] ✓ done (both events POSTed successfully)")
		return
	}

	if err := verifyResults(cfg, fake, recordingFiles, transcriptFiles); err != nil {
		exitf("%v", err)
	}
	fmt.Println("[driver] ✓ all checks passed")
}

// parseFlagsAndEnv parses CLI flags + reads required env vars and returns a
// fully populated runConfig. Exits the program with a useful message if any
// required input is missing.
func parseFlagsAndEnv() *runConfig {
	bridgeURL := flag.String("bridge-url", "http://localhost:8080/webhook",
		"URL of the bridge's webhook endpoint")
	topic := flag.String("topic", "",
		"Meeting topic (defaults to 'Synthetic Test <unix-time>')")
	hostEmail := flag.String("host-email", "synthetic@example.invalid",
		"Host email to put in the synthetic payload")
	reverseOrder := flag.Bool("reverse-order", false,
		"Send transcript event before recording event (tests out-of-order delivery)")
	skipVerify := flag.Bool("skip-verify", false,
		"Skip Drive verification — just POST and confirm 200")
	pollTimeout := flag.Duration("poll-timeout", 60*time.Second,
		"How long to poll Drive for the expected folder before giving up")
	flag.Parse()

	secret := os.Getenv("ZOOM_WEBHOOK_SECRET_TOKEN")
	rootFolderID := os.Getenv("DRIVE_ROOT_FOLDER_ID")
	if secret == "" {
		exitf("ZOOM_WEBHOOK_SECRET_TOKEN env var is required")
	}
	if !*skipVerify && rootFolderID == "" {
		exitf("DRIVE_ROOT_FOLDER_ID env var is required (or pass --skip-verify)")
	}
	if !*skipVerify && os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
		fmt.Fprintln(os.Stderr,
			"warning: GOOGLE_APPLICATION_CREDENTIALS not set; Drive verification "+
				"may fall back to other ADC sources or fail")
	}

	startTime := time.Now()
	if *topic == "" {
		*topic = fmt.Sprintf("Synthetic Test %d", startTime.Unix())
	}
	return &runConfig{
		bridgeURL:    *bridgeURL,
		topic:        *topic,
		hostEmail:    *hostEmail,
		reverseOrder: *reverseOrder,
		skipVerify:   *skipVerify,
		pollTimeout:  *pollTimeout,
		secret:       secret,
		rootFolderID: rootFolderID,
		startTime:    startTime,
		meetingID:    startTime.Unix(),
	}
}

// defaultSyntheticFiles returns the two groups of files this driver sends:
// the recording files (delivered in recording.completed) and the transcript
// file (delivered in recording.transcript_completed).
func defaultSyntheticFiles() (recording, transcript []SyntheticFile) {
	recording = []SyntheticFile{
		{
			RecordingType: "shared_screen_with_speaker_view",
			FileType:      "MP4",
			FileExtension: "MP4",
			FilePath:      "/synth-video.mp4",
			Content:       bytes.Repeat([]byte("VIDEO_DATA_"), 100), // 1100 bytes
		},
		{
			RecordingType: "audio_only",
			FileType:      "M4A",
			FileExtension: "M4A",
			FilePath:      "/synth-audio.m4a",
			Content:       bytes.Repeat([]byte("AUDIO_DATA_"), 50), // 550 bytes
		},
		{
			RecordingType: "timeline",
			FileType:      "TIMELINE",
			FileExtension: "JSON",
			FilePath:      "/synth-timeline.json",
			Content:       []byte(`[{"ts":"00:00:00","event":"meeting_started"}]`),
		},
	}
	transcript = []SyntheticFile{
		{
			RecordingType: "audio_transcript",
			FileType:      "TRANSCRIPT",
			FileExtension: "VTT",
			FilePath:      "/synth-transcript.vtt",
			Content:       []byte("WEBVTT\n\n00:00:00.000 --> 00:00:05.000\nSynthetic transcript line.\n"),
		},
	}
	return recording, transcript
}

// startFakeServer registers all file paths in a fresh fake Zoom file server.
func startFakeServer(allFiles []SyntheticFile) (*FakeZoomServer, error) {
	fakeFiles := make(map[string][]byte, len(allFiles))
	for _, f := range allFiles {
		fakeFiles[f.FilePath] = f.Content
	}
	return StartFakeZoomServer(fakeFiles)
}

// sendEvents builds, signs, and POSTs the two webhook events in the order
// dictated by cfg.reverseOrder. There's a 200ms gap between events to mimic
// realistic spacing.
func sendEvents(cfg *runConfig, fakeServerURL string, recordingFiles, transcriptFiles []SyntheticFile) error {
	downloadToken := fmt.Sprintf("synth-token-%d", cfg.meetingID)

	recordingPayload, err := BuildPayload(
		"recording.completed",
		cfg.topic, cfg.hostEmail, fakeServerURL, downloadToken,
		cfg.meetingID, recordingFiles, cfg.startTime,
	)
	if err != nil {
		return fmt.Errorf("build recording payload: %w", err)
	}
	transcriptPayload, err := BuildPayload(
		"recording.transcript_completed",
		cfg.topic, cfg.hostEmail, fakeServerURL, downloadToken,
		cfg.meetingID, transcriptFiles, cfg.startTime,
	)
	if err != nil {
		return fmt.Errorf("build transcript payload: %w", err)
	}
	fmt.Printf("[driver] recording.completed payload: %d bytes, %d files\n",
		len(recordingPayload), len(recordingFiles))
	fmt.Printf("[driver] recording.transcript_completed payload: %d bytes, %d files\n",
		len(transcriptPayload), len(transcriptFiles))

	type namedPayload struct {
		name    string
		payload []byte
	}
	first := namedPayload{"recording.completed", recordingPayload}
	second := namedPayload{"recording.transcript_completed", transcriptPayload}
	if cfg.reverseOrder {
		first, second = second, first
		fmt.Println("[driver] --reverse-order: sending transcript event first")
	}

	if err := postEvent(cfg.bridgeURL, first.name, first.payload, cfg.secret); err != nil {
		return fmt.Errorf("%s: %w", first.name, err)
	}
	// Small gap so the second event arrives shortly after the first.
	// Real Zoom delivers them within milliseconds; 200ms is realistic
	// without making the test slow.
	time.Sleep(200 * time.Millisecond)
	if err := postEvent(cfg.bridgeURL, second.name, second.payload, cfg.secret); err != nil {
		return fmt.Errorf("%s: %w", second.name, err)
	}
	return nil
}

// verifyResults polls Drive for the expected structure and content, then
// confirms the fake server received the expected download requests.
func verifyResults(cfg *runConfig, fake *FakeZoomServer, recordingFiles, transcriptFiles []SyntheticFile) error {
	fmt.Printf("[driver] polling Drive for files (timeout %s)...\n", cfg.pollTimeout)
	expectedFolderName := fmt.Sprintf("%s-%s",
		cfg.startTime.UTC().Format("2006-01-02"),
		sanitizeForFolder(cfg.topic))

	expected := []ExpectedFile{
		{NameContains: "shared_screen_with_speaker_view", Content: recordingFiles[0].Content},
		{NameContains: "audio_only", Content: recordingFiles[1].Content},
		{NameContains: "timeline", Content: recordingFiles[2].Content},
		{NameContains: "audio_transcript", Content: transcriptFiles[0].Content},
	}

	ctx := context.Background()
	if err := VerifyDrive(ctx, cfg.rootFolderID, expectedFolderName, expected, cfg.pollTimeout); err != nil {
		return fmt.Errorf("Drive verification failed: %w", err)
	}

	allFiles := append(append([]SyntheticFile{}, recordingFiles...), transcriptFiles...)
	for _, f := range allFiles {
		hits := fake.Hits(f.FilePath)
		if hits == 0 {
			return fmt.Errorf("fake server never received request for %s — bridge may not have downloaded it", f.FilePath)
		}
		if hits > 1 {
			fmt.Fprintf(os.Stderr, "[driver] warning: fake server got %d requests for %s (expected 1)\n",
				hits, f.FilePath)
		}
	}
	return nil
}

// postEvent signs the payload and POSTs it to the bridge, asserting a 200
// response.
func postEvent(bridgeURL, eventName string, payload []byte, secret string) error {
	timestamp, signature := Sign(secret, payload)

	fmt.Printf("[driver] POST %s event=%s\n", bridgeURL, eventName)
	req, err := http.NewRequest(http.MethodPost, bridgeURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-zm-request-timestamp", timestamp)
	req.Header.Set("x-zm-signature", signature)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST: %w (is the bridge running on %s?)", err, bridgeURL)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	fmt.Printf("[driver]   → 200 OK\n")
	return nil
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[driver] error: "+format+"\n", args...)
	os.Exit(1)
}

// sanitizeForFolder mirrors the bridge's main.sanitizeFilename so the driver
// can predict the exact folder name the bridge will create. Duplicated here
// to keep the driver in its own cmd/ package without sharing internals.
func sanitizeForFolder(name string) string {
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
