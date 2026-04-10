// synthetic-test sends a fully synthetic Zoom recording.completed webhook to a
// running zoom-recording-google-drive-bridge instance and verifies that the
// recording files land in Google Drive.
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
// The driver:
//  1. Spins up a fake Zoom file server on a random localhost port
//  2. Builds a recording.completed payload with download_url pointing at the
//     fake server
//  3. Signs the payload with ZOOM_WEBHOOK_SECRET_TOKEN
//  4. POSTs to the bridge's /webhook endpoint
//  5. Polls Drive until the expected files appear (or times out)
//  6. Downloads each file from Drive and compares bytes to what the fake
//     server served (proves the streaming pipe is intact)
//  7. Verifies meeting-metadata.json exists with the expected fields
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
	skipVerify := flag.Bool("skip-verify", false,
		"Skip Drive verification — just POST and confirm 200")
	pollTimeout := flag.Duration("poll-timeout", 30*time.Second,
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

	// 1. Build the synthetic file content. Small fixed bytes per file.
	files := []SyntheticFile{
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
			RecordingType: "audio_transcript",
			FileType:      "TRANSCRIPT",
			FileExtension: "VTT",
			FilePath:      "/synth-transcript.vtt",
			Content:       []byte("WEBVTT\n\n00:00:00.000 --> 00:00:05.000\nSynthetic transcript line.\n"),
		},
	}

	// 2. Start fake Zoom file server
	fakeFiles := make(map[string][]byte, len(files))
	for _, f := range files {
		fakeFiles[f.FilePath] = f.Content
	}
	fake, err := StartFakeZoomServer(fakeFiles)
	if err != nil {
		exitf("start fake server: %v", err)
	}
	defer fake.Close()
	fmt.Printf("[driver] fake Zoom file server: %s\n", fake.URL())

	// 3. Build + sign payload
	downloadToken := fmt.Sprintf("synth-token-%d", startTime.Unix())
	payload, err := BuildPayload(*topic, *hostEmail, fake.URL(), downloadToken, files, startTime)
	if err != nil {
		exitf("build payload: %v", err)
	}
	timestamp, signature := Sign(secret, payload)
	fmt.Printf("[driver] payload built: %d bytes, %d files\n", len(payload), len(files))

	// 4. POST to bridge
	fmt.Printf("[driver] POST %s\n", *bridgeURL)
	req, err := http.NewRequest(http.MethodPost, *bridgeURL, bytes.NewReader(payload))
	if err != nil {
		exitf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-zm-request-timestamp", timestamp)
	req.Header.Set("x-zm-signature", signature)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		exitf("POST to bridge: %v (is the bridge running on %s?)", err, *bridgeURL)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		exitf("bridge returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	fmt.Printf("[driver] bridge responded 200 OK\n")

	if *skipVerify {
		fmt.Println("[driver] --skip-verify set; not checking Drive")
		fmt.Println("[driver] ✓ done (synthetic POST accepted)")
		return
	}

	// 5. Verify Drive
	fmt.Printf("[driver] polling Drive for files (timeout %s)...\n", *pollTimeout)
	// Match the exact folder name the bridge will create, not just the date
	// prefix — otherwise the verifier picks up stale folders from earlier runs.
	expectedFolderName := fmt.Sprintf("%s-%s", startTime.UTC().Format("2006-01-02"), sanitizeForFolder(*topic))
	expected := []ExpectedFile{
		{NameContains: "shared_screen_with_speaker_view", Content: files[0].Content},
		{NameContains: "audio_only", Content: files[1].Content},
		{NameContains: "audio_transcript", Content: files[2].Content},
	}

	ctx := context.Background()
	if err := VerifyDrive(ctx, rootFolderID, expectedFolderName, expected, *pollTimeout); err != nil {
		exitf("Drive verification failed: %v", err)
	}

	// Verify the fake server actually received the downloads (proves bridge
	// followed the download_url + access_token path correctly)
	for _, f := range files {
		if fake.Hits(f.FilePath) == 0 {
			exitf("fake server never received request for %s — bridge may not have downloaded it", f.FilePath)
		}
	}

	fmt.Println("[driver] ✓ all checks passed")
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
