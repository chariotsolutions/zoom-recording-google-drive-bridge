# zoom-recording-google-drive-bridge

[![CI](https://github.com/chariotsolutions/zoom-recording-google-drive-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/chariotsolutions/zoom-recording-google-drive-bridge/actions/workflows/ci.yml)
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
   - Enqueues a Cloud Tasks task for the event and ACKs Zoom immediately
   - The queue (`max-concurrent-dispatches=1`) serializes execution and
     provides durable retry
5. Cloud Tasks dispatches each task to `/process-event` (OIDC-authenticated)
   which does the actual Zoom → Drive work synchronously:
   - Creates a Drive folder structure:
     `<root>/<host_username>/<YYYY-MM-DDThh-mm>-<topic>/raw/`
     where `host_username` is the lowercased local part of the meeting
     host's email (e.g., `skapadia` from `skapadia@chariotsolutions.com`)
   - Streams each recording file (MP4, M4A, timeline.json, transcript VTT)
     from Zoom into Drive using the per-event `download_token` for auth
   - Writes a `meeting-metadata.json` file (only on the initial
     `recording.completed` event to avoid overwriting)

The two-endpoint design exists because Cloud Run throttles CPU
between inbound requests and can reap idle instances — background
goroutines spawned from `/webhook` would get killed mid-upload. See
[`docs/design-decisions.md`](./docs/design-decisions.md) "Decision 6"
and [issue #8](https://github.com/chariotsolutions/zoom-recording-google-drive-bridge/issues/8)
for the full story.

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
| `PROCESS_EVENT_URL` | Full URL of this service's `/process-event` endpoint (e.g. `https://…run.app/process-event`) |
| `CLOUD_TASKS_QUEUE` | Queue resource name, `projects/.../locations/.../queues/…` |
| `TASKS_INVOKER_SA` | Service account email Cloud Tasks impersonates for OIDC tokens |
| `PORT` | (Optional, defaults to 8080) |
| `BRIDGE_IN_PROCESS_FAKE_TASKS` | (Test only) Set to `1` for local-dev/synthetic runs — short-circuits Cloud Tasks and OIDC verification. **Never set in production.** |

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
| `/` | GET | Liveness check (returns plain text) |
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
