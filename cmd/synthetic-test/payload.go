package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// SyntheticFile describes one fake recording file the test will produce.
type SyntheticFile struct {
	RecordingType string // e.g. "shared_screen_with_speaker_view", "audio_only"
	FileType      string // e.g. "MP4", "M4A", "TRANSCRIPT"
	FileExtension string // e.g. "MP4", "M4A", "VTT"
	FilePath      string // path on the fake server, e.g. "/file1.mp4"
	Content       []byte // bytes the fake server will serve for this path
}

// BuildPayload constructs a recording.completed webhook event matching the
// schema we verified against the Zoom Developer Docs. The download_url for
// each file points at the fake server.
func BuildPayload(
	topic, hostEmail, fakeServerURL, downloadToken string,
	files []SyntheticFile,
	startTime time.Time,
) ([]byte, error) {
	recordingFiles := make([]map[string]any, len(files))
	for i, f := range files {
		recordingFiles[i] = map[string]any{
			"id":              fmt.Sprintf("synth-file-%d-%d", startTime.Unix(), i),
			"meeting_id":      "WEz4RT2lSyKx2MD9Z+lYfA==", // dummy meeting UUID
			"recording_start": startTime.UTC().Format(time.RFC3339),
			"recording_end":   startTime.Add(5 * time.Minute).UTC().Format(time.RFC3339),
			"file_type":       f.FileType,
			"file_extension":  f.FileExtension,
			"file_size":       len(f.Content),
			"play_url":        "https://example.invalid/play",
			"download_url":    fakeServerURL + f.FilePath,
			"recording_type":  f.RecordingType,
			"status":          "completed",
		}
	}

	event := map[string]any{
		"event":    "recording.completed",
		"event_ts": time.Now().UnixMilli(),
		"payload": map[string]any{
			"account_id": "synth_account_id",
			"object": map[string]any{
				"id":              startTime.Unix(),
				"uuid":            "WEz4RT2lSyKx2MD9Z+lYfA==",
				"host_id":         "synth_host_id",
				"host_email":      hostEmail,
				"topic":           topic,
				"start_time":      startTime.UTC().Format(time.RFC3339),
				"duration":        5,
				"recording_files": recordingFiles,
			},
		},
		"download_token": downloadToken,
	}

	return json.Marshal(event)
}

// Sign computes the (timestamp, signature) pair Zoom would attach to a webhook
// request, using the same HMAC-SHA256 process the production verifier expects.
func Sign(secret string, body []byte) (timestamp, signature string) {
	timestamp = strconv.FormatInt(time.Now().Unix(), 10)
	msg := "v0:" + timestamp + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	signature = "v0=" + hex.EncodeToString(mac.Sum(nil))
	return timestamp, signature
}
