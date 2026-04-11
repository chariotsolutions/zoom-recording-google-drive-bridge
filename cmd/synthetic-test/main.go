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
//	export DRIVE_ROOT_FOLDER_ID=<YOUR_FOLDER_ID>
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

func main() {
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
	meetingID := startTime.Unix()

	// The two groups of files, mirroring how real Zoom delivers them.
	recordingFiles := []SyntheticFile{
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

	transcriptFiles := []SyntheticFile{
		{
			RecordingType: "audio_transcript",
			FileType:      "TRANSCRIPT",
			FileExtension: "VTT",
			FilePath:      "/synth-transcript.vtt",
			Content:       []byte("WEBVTT\n\n00:00:00.000 --> 00:00:05.000\nSynthetic transcript line.\n"),
		},
	}

	// Start fake Zoom file server with all files from both groups.
	fakeFiles := make(map[string][]byte)
	for _, f := range recordingFiles {
		fakeFiles[f.FilePath] = f.Content
	}
	for _, f := range transcriptFiles {
		fakeFiles[f.FilePath] = f.Content
	}
	fake, err := StartFakeZoomServer(fakeFiles)
	if err != nil {
		exitf("start fake server: %v", err)
	}
	defer fake.Close()
	fmt.Printf("[driver] fake Zoom file server: %s\n", fake.URL())
	fmt.Printf("[driver] meetingID=%d topic=%q\n", meetingID, *topic)

	downloadToken := fmt.Sprintf("synth-token-%d", meetingID)

	// Build both payloads
	recordingPayload, err := BuildPayload(
		"recording.completed",
		*topic, *hostEmail, fake.URL(), downloadToken,
		meetingID, recordingFiles, startTime,
	)
	if err != nil {
		exitf("build recording payload: %v", err)
	}
	transcriptPayload, err := BuildPayload(
		"recording.transcript_completed",
		*topic, *hostEmail, fake.URL(), downloadToken,
		meetingID, transcriptFiles, startTime,
	)
	if err != nil {
		exitf("build transcript payload: %v", err)
	}
	fmt.Printf("[driver] recording.completed payload: %d bytes, %d files\n",
		len(recordingPayload), len(recordingFiles))
	fmt.Printf("[driver] recording.transcript_completed payload: %d bytes, %d files\n",
		len(transcriptPayload), len(transcriptFiles))

	// Determine the order to send events.
	// Normal mode: recording.completed first, then transcript.
	// Reverse mode: transcript first, then recording (tests the
	// per-meeting lock's handling of out-of-order delivery).
	first := struct {
		name    string
		payload []byte
	}{"recording.completed", recordingPayload}
	second := struct {
		name    string
		payload []byte
	}{"recording.transcript_completed", transcriptPayload}
	if *reverseOrder {
		first, second = second, first
		fmt.Println("[driver] --reverse-order: sending transcript event first")
	}

	if err := postEvent(*bridgeURL, first.name, first.payload, secret); err != nil {
		exitf("%s: %v", first.name, err)
	}
	// Small gap so the second event arrives shortly after the first.
	// In real Zoom the two events can arrive within milliseconds of each
	// other; we use 200ms here to be realistic without waiting forever.
	time.Sleep(200 * time.Millisecond)
	if err := postEvent(*bridgeURL, second.name, second.payload, secret); err != nil {
		exitf("%s: %v", second.name, err)
	}

	if *skipVerify {
		fmt.Println("[driver] --skip-verify set; not checking Drive")
		fmt.Println("[driver] ✓ done (both events POSTed successfully)")
		return
	}

	// Poll Drive for the full expected structure.
	fmt.Printf("[driver] polling Drive for files (timeout %s)...\n", *pollTimeout)
	expectedFolderName := fmt.Sprintf("%s-%s",
		startTime.UTC().Format("2006-01-02"),
		sanitizeForFolder(*topic))

	// Union of all expected files from both events.
	expected := []ExpectedFile{
		{NameContains: "shared_screen_with_speaker_view", Content: recordingFiles[0].Content},
		{NameContains: "audio_only", Content: recordingFiles[1].Content},
		{NameContains: "timeline", Content: recordingFiles[2].Content},
		{NameContains: "audio_transcript", Content: transcriptFiles[0].Content},
	}

	ctx := context.Background()
	if err := VerifyDrive(ctx, rootFolderID, expectedFolderName, expected, *pollTimeout); err != nil {
		exitf("Drive verification failed: %v", err)
	}

	// Verify the fake server received each file exactly once.
	allFiles := append(recordingFiles, transcriptFiles...)
	for _, f := range allFiles {
		hits := fake.Hits(f.FilePath)
		if hits == 0 {
			exitf("fake server never received request for %s — bridge may not have downloaded it", f.FilePath)
		}
		if hits > 1 {
			fmt.Fprintf(os.Stderr, "[driver] warning: fake server got %d requests for %s (expected 1)\n",
				hits, f.FilePath)
		}
	}

	fmt.Println("[driver] ✓ all checks passed")
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
