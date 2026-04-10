# Synthetic Webhook Test Driver — Plan

## Context

Zoom does **not** provide a "send test event" button for synthetic webhook events. The only built-in test is the URL validation handshake that fires automatically when you save the endpoint URL in the Zoom Marketplace app config. For everything else (`recording.completed` in particular), the community options are:

1. Use ngrok + a real test meeting on a real Zoom account every time you want to test
2. Capture a real payload once and replay it via curl forever
3. Build your own synthetic driver that constructs payloads, signs them, and POSTs to your endpoint

This document plans option 3 for the `zoom-recording-google-drive-bridge` project.

## Why build it

The unit tests in `main_test.go` cover:
- Pure functions (signature verification, URL building, sanitization, etc.)
- HTTP handler routing and response codes
- Validation handshake correctness

They do **not** cover:
- Real Drive auth (scopes, service account permissions, folder access)
- Real network behavior (TLS, large bodies, slow downloads)
- The actual streaming pipe end-to-end (Zoom download → Drive upload, no buffering)
- The folder structure as it materializes in a real Drive account
- The metadata file as it lands in real Drive

A synthetic driver lets us exercise all of the above on demand, locally, without creating real Zoom meetings every time.

## Goals

- One-command end-to-end test against real Google Drive using a test folder
- No dependency on Zoom — we generate the payload and signature ourselves
- Realistic enough to catch real bugs (correct schema, valid signature, real bytes flowing through the streaming pipe)
- Repeatable as a regression check after code changes

## Architecture

```
+----------------+      +------------------------+      +----------------+
| test driver    |      | bridge service         |      | real Google    |
| (Go program)   |      | (go run . / deployed)  |      | Drive          |
|                |      |                        |      |                |
| 1. start fake  |      |                        |      |                |
|    file server |      |                        |      |                |
|                |      |                        |      |                |
| 2. construct   |      |                        |      |                |
|    payload w/  |      |                        |      |                |
|    download_url|      |                        |      |                |
|    pointing to |      |                        |      |                |
|    fake server |      |                        |      |                |
|                |      |                        |      |                |
| 3. sign w/     |      |                        |      |                |
|    webhook     |      |                        |      |                |
|    secret      |      |                        |      |                |
|                |      |                        |      |                |
| 4. POST to     |----->| /webhook               |      |                |
|    /webhook    |      |   verify sig (200)     |      |                |
|                |      |   parse, dispatch      |      |                |
|                |      |   goroutine starts     |      |                |
|                |      |                        |      |                |
|                |<---- |   GET download_url     |      |                |
| 5. serve fake  | ---->|   (with access_token)  |      |                |
|    bytes       |      |                        |      |                |
|                |      |                        ----->| Files.Create   |
|                |      |   stream → Drive       |      | (real upload)  |
|                |      |                        |      |                |
| 6. verify file |      |                        |<-----| folder + file  |
|    appeared in |      |                        |      | created        |
|    Drive       |      |                        |      |                |
+----------------+      +------------------------+      +----------------+
```

## Components

### Component 1: Fake Zoom file server

A tiny `http.Server` that serves predetermined bytes for any path. Started before the test, listens on a random port.

```go
type fakeZoomServer struct {
    server *httptest.Server
    files  map[string][]byte  // path → bytes to serve
}
```

- Verifies the request includes `?access_token=...` in the query (the bridge MUST be using the download_token, not a bearer header)
- Returns 200 with the bytes for the requested path
- Returns 404 if the path is unknown
- Returns 401 if `access_token` is missing

### Component 2: Payload builder

A Go function that constructs a realistic `recording.completed` event:

```go
func buildSyntheticEvent(
    fakeServerURL string,
    downloadToken string,
    files []syntheticFile,
) []byte
```

Each `syntheticFile` describes:
- Recording type (`audio_only`, `shared_screen_with_speaker_view`, etc.)
- File type (`MP4`, `M4A`, `TRANSCRIPT`)
- Bytes to serve from the fake server

The builder produces JSON matching Zoom's exact schema (verified against the docs and used by the production code), with the `download_url` field pointing at the fake server.

### Component 3: Signer

Uses the webhook secret (from env var or flag) to compute `x-zm-request-timestamp` and `x-zm-signature` exactly the way Zoom does. Reuses logic compatible with the production `verifyZoomSignature` function. (We can either expose `signFor` from `main_test.go` or duplicate the small HMAC computation — duplication is fine for a separate binary.)

### Component 4: HTTP poster

POSTs the signed payload to the bridge's `/webhook` endpoint with the right headers. Asserts on the response status (200 expected).

### Component 5: Drive verifier (optional but recommended)

After waiting a few seconds for the goroutine to finish, queries the Drive API for the expected folder structure and file contents. Asserts:
- `<root>/<YYYY-MM-DD>-<topic>/raw/` exists
- Each expected recording file is present
- File contents match what the fake server served (proves the streaming pipe is intact)
- `meeting-metadata.json` exists with expected fields

This step requires a Google service account with Drive access — same as production. Can be skipped (just print "check Drive manually") if we want a lighter version.

## File layout

```
zoom-recording-google-drive-bridge/
  cmd/
    synthetic-test/
      main.go        # the driver
      payload.go     # payload builder
      fake_server.go # fake Zoom file server
      verify.go      # Drive verification (optional)
```

Reasoning: keeping it under `cmd/` follows Go conventions for secondary binaries in the same module. `go build ./cmd/synthetic-test` produces a separate binary.

## How it would be run

```bash
# Set up env (same as production, plus a test Drive folder)
export ZOOM_WEBHOOK_SECRET_TOKEN=...
export DRIVE_ROOT_FOLDER_ID=...   # a test folder, NOT prod
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/service-account.json

# Terminal 1: start the bridge
go run .

# Terminal 2: run the driver
go run ./cmd/synthetic-test \
  --bridge-url=http://localhost:8080/webhook \
  --topic="Test Meeting $(date +%s)" \
  --files=mp4,m4a,transcript
```

Expected output:

```
[driver] starting fake Zoom file server on :43891
[driver] constructing recording.completed payload
[driver] signing with webhook secret
[driver] POST http://localhost:8080/webhook
[driver] response: 200 OK
[driver] waiting 5s for goroutine processing...
[driver] verifying Drive folder structure
[driver] ✓ folder created: 2026-04-10-Test Meeting 1712785432/
[driver] ✓ folder created: 2026-04-10-Test Meeting 1712785432/raw/
[driver] ✓ uploaded: Test Meeting 1712785432-shared_screen_with_speaker_view.mp4 (1024 bytes)
[driver] ✓ uploaded: Test Meeting 1712785432-audio_only.m4a (512 bytes)
[driver] ✓ uploaded: Test Meeting 1712785432-audio_transcript.vtt (256 bytes)
[driver] ✓ metadata file: meeting-metadata.json
[driver] all checks passed
```

## What it catches that unit tests don't

- Drive service account auth misconfiguration (wrong scope, no folder access)
- Real network behavior (TLS, redirects, slow connections — though all on localhost)
- The streaming pipe actually streaming (bytes flow through without buffering)
- Folder creation against real Drive (case sensitivity, name conflicts, quota)
- Metadata file format as it lands in real Drive
- Schema drift (if we ever change the production code's struct definitions in a way that breaks unmarshal)

## What it doesn't catch

- Real Zoom payload quirks we didn't anticipate (only catchable by actually receiving a real event)
- The 24-hour download token expiry (we control the token in synthetic runs)
- Cloud Run-specific issues (cold starts, timeouts, IAM)

## Out of scope (for v1)

- CI integration — this is a manual on-demand tool, not a CI test. Adding it to CI would require encrypted Drive credentials in CI secrets, which is more setup than it's worth right now.
- Multi-meeting load testing
- Failure injection (already covered by unit tests)
- Replay of captured real payloads — could be a v2 feature; for now we generate fresh payloads

## Dependencies

- Go (already required for the project)
- A Google Cloud service account with Drive scope
- A test Drive folder ID (separate from any production folder)
- The Zoom webhook secret token (can be a test value, not the real production one)

No new Go dependencies — uses stdlib `net/http`, `net/http/httptest`, plus the `google.golang.org/api/drive/v3` client we already have.

## Estimated effort

- ~150 lines of Go across 4 files
- 1-2 hours including testing the test driver itself
- One commit when done

## When to build it

**After the bridge is deployed to Cloud Run and a real Zoom test webhook has been confirmed working at least once.**

Reasoning: building the driver before the first real-world test would mean we're testing our own code against our own assumptions — and the driver itself could have the same blind spots as the production code (e.g., wrong field names in the schema). Once a real Zoom event has been confirmed working, we have ground truth: any future drift can be detected against that baseline.

After deployment, the driver becomes a regression check that runs in seconds, locally, without needing to schedule a test meeting.

## Acceptance criteria

- [ ] `cmd/synthetic-test/` directory with the four components
- [ ] Driver runs against a local `go run .` instance and produces expected output
- [ ] Driver verifies real Drive folder structure and file contents
- [ ] Driver fails loudly with useful error messages if any step breaks
- [ ] README updated with a "Local end-to-end testing" section pointing at the driver
- [ ] One example invocation documented in the driver's own help text
