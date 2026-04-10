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

// VerifyDrive polls Drive until the entire expected structure exists (meeting
// folder, raw subfolder, all expected files, and meeting-metadata.json) or the
// timeout elapses, then verifies that file contents match what the fake server
// served.
//
// folderName must be the exact folder name the bridge will create — not a
// prefix — so the verifier doesn't pick up stale folders from earlier runs.
func VerifyDrive(
	ctx context.Context,
	rootFolderID string,
	folderName string,
	expected []ExpectedFile,
	pollTimeout time.Duration,
) error {
	svc, err := drive.NewService(ctx, option.WithScopes(drive.DriveScope))
	if err != nil {
		return fmt.Errorf("create drive client: %w", err)
	}

	deadline := time.Now().Add(pollTimeout)
	var meetingFolderID, meetingFolderName, rawFolderID string
	var lastErr error

	for {
		lastErr = nil

		// Step A: meeting folder (exact name match)
		if meetingFolderID == "" {
			id, err := findChildFolder(svc, rootFolderID, folderName)
			if err == nil {
				meetingFolderID = id
				meetingFolderName = folderName
			} else {
				lastErr = fmt.Errorf("meeting folder %q not yet present: %w", folderName, err)
			}
		}

		// Step B: raw subfolder
		if meetingFolderID != "" && rawFolderID == "" {
			rawFolderID, err = findChildFolder(svc, meetingFolderID, "raw")
			if err != nil {
				lastErr = fmt.Errorf("raw subfolder not yet present: %w", err)
			}
		}

		// Step C: expected files in raw/
		var missingFiles []string
		if rawFolderID != "" {
			for _, ef := range expected {
				_, _, err := findChildFileContaining(svc, rawFolderID, ef.NameContains)
				if err != nil {
					missingFiles = append(missingFiles, ef.NameContains)
				}
			}
			if len(missingFiles) > 0 {
				lastErr = fmt.Errorf("waiting for files: %v", missingFiles)
			}
		}

		// Step D: metadata file
		var metaPresent bool
		if meetingFolderID != "" && len(missingFiles) == 0 && rawFolderID != "" {
			_, _, err := findChildFileContaining(svc, meetingFolderID, "meeting-metadata.json")
			metaPresent = (err == nil)
			if !metaPresent {
				lastErr = fmt.Errorf("waiting for meeting-metadata.json")
			}
		}

		// All present?
		if meetingFolderID != "" && rawFolderID != "" && len(missingFiles) == 0 && metaPresent {
			break
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("polling timeout: %v", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	fmt.Printf("[verify] found meeting folder: %s\n", meetingFolderName)
	fmt.Println("[verify] found raw subfolder")

	// Verify each file's contents match what the fake server served.
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

	// Verify metadata file structure
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
