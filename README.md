# zoom-recording-google-drive-bridge

A small Go HTTP service that receives Zoom `recording.completed` webhooks and
streams the recording files directly into a Google Drive folder.

Designed to run on Google Cloud Run. Memory usage stays constant regardless of
file size because it streams the download from Zoom into the Drive upload
without buffering the whole file.

## What it does

1. Listens for Zoom webhook POSTs at `/webhook`
2. Handles the `endpoint.url_validation` handshake (HMAC-SHA256 with the webhook secret)
3. On `recording.completed`:
   - Fetches a Zoom Server-to-Server OAuth access token
   - Creates a Drive folder structure: `<root>/<YYYY-MM-DD>-<topic>/raw/`
   - Streams each recording file (MP4, M4A, transcript, etc.) from Zoom into Drive
   - Writes a `meeting-metadata.json` file with meeting info

## Required environment variables

| Variable | Description |
|---|---|
| `ZOOM_WEBHOOK_SECRET_TOKEN` | From your Zoom app → Feature → Secret Token |
| `ZOOM_CLIENT_ID` | From your Zoom app → App Credentials |
| `ZOOM_CLIENT_SECRET` | From your Zoom app → App Credentials |
| `ZOOM_ACCOUNT_ID` | From your Zoom app → App Credentials |
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
  --set-env-vars="ZOOM_WEBHOOK_SECRET_TOKEN=...,ZOOM_CLIENT_ID=...,ZOOM_CLIENT_SECRET=...,ZOOM_ACCOUNT_ID=...,DRIVE_ROOT_FOLDER_ID=..."
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
