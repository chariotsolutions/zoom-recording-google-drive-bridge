# zoom-recording-google-drive-bridge

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/chariotsolutions/zoom-recording-google-drive-bridge)](go.mod)
[![Go Report Card](https://goreportcard.com/badge/github.com/chariotsolutions/zoom-recording-google-drive-bridge)](https://goreportcard.com/report/github.com/chariotsolutions/zoom-recording-google-drive-bridge)

A small Go HTTP service that receives Zoom recording webhooks and streams
the files directly into a Google Drive folder.

Designed to run on Google Cloud Run. Memory usage stays constant regardless
of file size because it streams the download from Zoom into the Drive upload
without buffering the whole file.

## What it does

1. Listens for Zoom webhook POSTs at `/webhook`
2. Verifies the `x-zm-signature` header on every event (HMAC-SHA256 with the
   webhook secret, 5-minute replay window)
3. Handles the `endpoint.url_validation` handshake
4. On `recording.completed` and `recording.transcript_completed`:
   - Creates a Drive folder structure: `<root>/<YYYY-MM-DD>-<topic>/raw/`
   - Streams each recording file (MP4, M4A, timeline.json, transcript VTT)
     from Zoom into Drive using the per-event `download_token` for auth
   - Writes a `meeting-metadata.json` file (only on the initial
     `recording.completed` event to avoid overwriting)
5. Serializes concurrent events for the same meeting with a per-meeting
   mutex, so the two events can arrive in either order without creating
   duplicate folders

### Limitations

- Transcripts are not guaranteed to arrive. Zoom fires
  `recording.transcript_completed` only when transcript generation
  succeeds — short/silent/unsupported-language meetings may never produce
  one. In that case the other files land normally and the folder simply
  lacks a transcript.

## Required environment variables

| Variable | Description |
|---|---|
| `ZOOM_WEBHOOK_SECRET_TOKEN` | From your Zoom Marketplace app → Feature → Secret Token |
| `DRIVE_ROOT_FOLDER_ID` | Google Drive folder ID where recordings will land |
| `PORT` | (Optional, defaults to 8080) |

See `.env.example` for a template.

## Local development

```bash
# Copy the env template and fill in real values
cp .env.example .env

# Build and run
go build -o server .
./server

# Or run directly
go run .
```

## Deploying to Google Cloud Run

```bash
gcloud run deploy zoom-recording-bridge \
  --source . \
  --region us-east1 \
  --allow-unauthenticated \
  --service-account <YOUR_SERVICE_ACCOUNT> \
  --set-env-vars DRIVE_ROOT_FOLDER_ID=... \
  --update-secrets ZOOM_WEBHOOK_SECRET_TOKEN=zoom-webhook-secret:latest
```

After deployment, copy the service URL and paste it (with `/webhook` appended)
into your Zoom app's Event Subscription endpoint.

## Google Drive authentication

The service uses **Application Default Credentials** to authenticate with the
Drive API. On Cloud Run, attach a service account that has Drive access to the
target folder. Locally, run `gcloud auth application-default login` first.

## Endpoints

| Path | Method | Purpose |
|---|---|---|
| `/` | GET | Health check (returns plain text) |
| `/healthz` | GET | Health check for Cloud Run |
| `/webhook` | POST | Zoom webhook receiver |

## Documentation

- [`docs/deployment.md`](./docs/deployment.md) — full deployment guide
  (Cloud Run setup, Secret Manager, Zoom app config, end-to-end testing)
- [`docs/design-decisions.md`](./docs/design-decisions.md) — *why* the code
  looks the way it does: design tradeoffs, alternatives considered, and
  bugs caught during development
- [`docs/synthetic-test-driver.md`](./docs/synthetic-test-driver.md) —
  the synthetic webhook test driver's design rationale

## License

Licensed under the Apache License, Version 2.0. See [`LICENSE`](./LICENSE)
for the full text and [`NOTICE`](./NOTICE) for attribution.
