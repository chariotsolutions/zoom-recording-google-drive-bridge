package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// ExpectedFile is one file we expect to find in the meeting's raw/ subfolder.
type ExpectedFile struct {
	NameContains string // substring match (the bridge sanitizes filenames)
	Content      []byte // expected bytes
}

// VerifyDrive polls Drive until the entire expected structure exists (host
// folder, meeting folder, raw subfolder, all expected files, and
// meeting-metadata.json) or the timeout elapses, then verifies that file
// contents match what the fake server served.
//
// hostFolder and meetingFolderName must be the exact folder names the bridge
// will create — not prefixes — so the verifier doesn't pick up stale folders
// from earlier runs.
func VerifyDrive(
	ctx context.Context,
	rootFolderID string,
	hostFolder, meetingFolderName string,
	expected []ExpectedFile,
	pollTimeout time.Duration,
) error {
	svc, err := drive.NewService(ctx, option.WithScopes(drive.DriveScope))
	if err != nil {
		return fmt.Errorf("create drive client: %w", err)
	}

	hostFolderID, meetingFolderID, rawFolderID, err := pollForExpectedStructure(ctx, svc, rootFolderID, hostFolder, meetingFolderName, expected, pollTimeout)
	if err != nil {
		return err
	}
	_ = hostFolderID // captured for logging only

	fmt.Printf("[verify] found host folder: %s\n", hostFolder)
	fmt.Printf("[verify] found meeting folder: %s\n", meetingFolderName)
	fmt.Println("[verify] found raw subfolder")

	if err := verifyFileContents(svc, rawFolderID, expected); err != nil {
		return err
	}
	return verifyMetadataFile(svc, meetingFolderID)
}

// pollForExpectedStructure waits until the host folder, meeting folder, raw
// subfolder, all expected files, and the metadata file are all present in
// Drive, or the timeout elapses.
func pollForExpectedStructure(
	ctx context.Context,
	svc *drive.Service,
	rootFolderID, hostFolder, meetingFolderName string,
	expected []ExpectedFile,
	pollTimeout time.Duration,
) (hostFolderID, meetingFolderID, rawFolderID string, err error) {
	deadline := time.Now().Add(pollTimeout)
	var lastErr error

	for {
		hostFolderID, meetingFolderID, rawFolderID, lastErr = pollOnce(svc, rootFolderID, hostFolder, meetingFolderName, expected, hostFolderID, meetingFolderID, rawFolderID)
		if lastErr == nil {
			return hostFolderID, meetingFolderID, rawFolderID, nil
		}
		if time.Now().After(deadline) {
			return "", "", "", fmt.Errorf("polling timeout: %v", lastErr)
		}
		select {
		case <-ctx.Done():
			return "", "", "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// pollOnce performs one round of folder/file lookups. It accumulates state
// between rounds via the hostFolderID, meetingFolderID, and rawFolderID
// parameters (callers should pass back what was returned from the previous
// call). Returns nil error when everything expected is present.
func pollOnce(
	svc *drive.Service,
	rootFolderID, hostFolder, meetingFolderName string,
	expected []ExpectedFile,
	hostFolderID, meetingFolderID, rawFolderID string,
) (string, string, string, error) {
	if hostFolderID == "" {
		id, err := findChildFolder(svc, rootFolderID, hostFolder)
		if err != nil {
			return "", "", "", fmt.Errorf("host folder %q not yet present: %w", hostFolder, err)
		}
		hostFolderID = id
	}
	if meetingFolderID == "" {
		id, err := findChildFolder(svc, hostFolderID, meetingFolderName)
		if err != nil {
			return hostFolderID, "", "", fmt.Errorf("meeting folder %q not yet present: %w", meetingFolderName, err)
		}
		meetingFolderID = id
	}
	if rawFolderID == "" {
		id, err := findChildFolder(svc, meetingFolderID, "raw")
		if err != nil {
			return hostFolderID, meetingFolderID, "", fmt.Errorf("raw subfolder not yet present: %w", err)
		}
		rawFolderID = id
	}
	var missing []string
	for _, ef := range expected {
		if _, _, err := findChildFileContaining(svc, rawFolderID, ef.NameContains); err != nil {
			missing = append(missing, ef.NameContains)
		}
	}
	if len(missing) > 0 {
		return hostFolderID, meetingFolderID, rawFolderID, fmt.Errorf("waiting for files: %v", missing)
	}
	if _, _, err := findChildFileContaining(svc, meetingFolderID, "meeting-metadata.json"); err != nil {
		return hostFolderID, meetingFolderID, rawFolderID, fmt.Errorf("waiting for meeting-metadata.json")
	}
	return hostFolderID, meetingFolderID, rawFolderID, nil
}

// verifyFileContents downloads each expected file from Drive and asserts
// that its bytes exactly match what the fake server served. This is the
// proof that the streaming pipe (Zoom download → Drive upload) is intact.
func verifyFileContents(svc *drive.Service, rawFolderID string, expected []ExpectedFile) error {
	for _, ef := range expected {
		fileID, fileName, err := findChildFileContaining(svc, rawFolderID, ef.NameContains)
		if err != nil {
			return fmt.Errorf("file %q vanished after poll: %w", ef.NameContains, err)
		}
		got, err := downloadFile(svc, fileID)
		if err != nil {
			return fmt.Errorf("download %s: %w", fileName, err)
		}
		if !bytes.Equal(got, ef.Content) {
			return fmt.Errorf("file %s content mismatch: got %d bytes, want %d bytes",
				fileName, len(got), len(ef.Content))
		}
		fmt.Printf("[verify] ✓ %s (%d bytes match)\n", fileName, len(got))
	}
	return nil
}

// verifyMetadataFile downloads the meeting-metadata.json file from the
// meeting folder and confirms it parses as JSON and contains the required
// fields the bridge writes.
func verifyMetadataFile(svc *drive.Service, meetingFolderID string) error {
	metaID, _, err := findChildFileContaining(svc, meetingFolderID, "meeting-metadata.json")
	if err != nil {
		return fmt.Errorf("metadata vanished after poll: %w", err)
	}
	metaBytes, err := downloadFile(svc, metaID)
	if err != nil {
		return fmt.Errorf("download metadata: %w", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return fmt.Errorf("metadata not valid JSON: %w", err)
	}
	requiredFields := []string{"topic", "start_time", "host_email", "meeting_id", "files_uploaded", "processed_at"}
	for _, k := range requiredFields {
		if _, ok := meta[k]; !ok {
			return fmt.Errorf("metadata missing field %q", k)
		}
	}
	fmt.Printf("[verify] ✓ meeting-metadata.json present with %d fields\n", len(meta))
	return nil
}

func findChildFolder(svc *drive.Service, parentID, name string) (string, error) {
	query := fmt.Sprintf(
		"mimeType='application/vnd.google-apps.folder' and name='%s' and '%s' in parents and trashed=false",
		name, parentID,
	)
	list, err := svc.Files.List().
		Q(query).
		Fields("files(id, name)").
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true).
		Do()
	if err != nil {
		return "", err
	}
	if len(list.Files) == 0 {
		return "", fmt.Errorf("folder %q not found under parent %s", name, parentID)
	}
	return list.Files[0].Id, nil
}

func findChildFileContaining(svc *drive.Service, parentID, substring string) (id, name string, err error) {
	query := fmt.Sprintf(
		"'%s' in parents and trashed=false and name contains '%s'",
		parentID, substring,
	)
	list, err := svc.Files.List().
		Q(query).
		Fields("files(id, name)").
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true).
		Do()
	if err != nil {
		return "", "", err
	}
	if len(list.Files) == 0 {
		return "", "", fmt.Errorf("no file containing %q under parent %s", substring, parentID)
	}
	return list.Files[0].Id, list.Files[0].Name, nil
}

func downloadFile(svc *drive.Service, fileID string) ([]byte, error) {
	resp, err := svc.Files.Get(fileID).
		SupportsAllDrives(true).
		Download()
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
