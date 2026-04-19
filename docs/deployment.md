# Deployment Guide

This document walks through deploying the `zoom-recording-google-drive-bridge`
service to Google Cloud Run from scratch, configuring the Zoom Marketplace
app to hit it, and verifying end-to-end with a real meeting.

Follow the steps in order. Each one is idempotent — re-running a completed
step is safe.

---

## Prerequisites

Before starting, you need:

- **`gcloud` CLI** installed and authenticated (`gcloud auth login`)
- **Go 1.26+** (only needed if running locally or building the binary yourself;
  Cloud Build handles compilation during deploy)
- **A Google Cloud project** for the bridge
- **A Google Drive folder** where recordings will land, accessible to the
  service account that Cloud Run will use. Workspace Shared Drives are
  supported. The folder ID is in the URL:
  `drive.google.com/drive/folders/<THIS_IS_THE_ID>`
- **A Google Cloud service account** with **Contributor** (writer) access to
  the Drive folder. The Cloud Run service will run as this account, so it
  needs Drive access to read/write the target folder. Create it via the
  Google Cloud Console, `gcloud iam service-accounts create`, or whatever
  internal provisioning tool your organization uses.
- **A Zoom Marketplace app** of type **Server-to-Server OAuth** in your
  organization, with Event Subscriptions configured but the endpoint URL not
  yet set. You'll need its **Secret Token** (Feature → Secret Token in the
  Zoom Marketplace app config).

## What gets deployed

- **Cloud Run service** named `zoom-recording-bridge` in region `us-east1`
- Built from source via Cloud Build using the repo's `Dockerfile`
- Runs as the configured service account (so it inherits the Drive folder
  permissions granted to that account)
- Reads `DRIVE_ROOT_FOLDER_ID` from a plain env var
- Reads `ZOOM_WEBHOOK_SECRET_TOKEN` from Google Secret Manager at runtime
- Public HTTPS endpoint (`--allow-unauthenticated`); security is enforced at
  the application layer via Zoom's HMAC signature verification

---

## Step 1: Set the active gcloud project

Makes all subsequent commands target the right project without needing
`--project=` on every line.

```bash
gcloud config set project <YOUR_PROJECT_ID>
```

Verify:

```bash
gcloud config get-value project
# should print: <YOUR_PROJECT_ID>
```

---

## Step 2: Enable required Google Cloud APIs

Cloud Run deployment needs four APIs enabled on the project. The command is
idempotent.

```bash
gcloud services enable \
  run.googleapis.com \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  secretmanager.googleapis.com
```

What each one does:

| API | Purpose |
|---|---|
| `run.googleapis.com` | Cloud Run itself |
| `cloudbuild.googleapis.com` | Builds the Docker image from source |
| `artifactregistry.googleapis.com` | Stores the built container images |
| `secretmanager.googleapis.com` | Holds the Zoom webhook secret |

Also make sure the Drive API is enabled (required for the running service to
talk to Google Drive):

```bash
gcloud services enable drive.googleapis.com
```

Verify everything is enabled:

```bash
gcloud services list --enabled --filter="config.name:(
  run.googleapis.com OR
  cloudbuild.googleapis.com OR
  artifactregistry.googleapis.com OR
  secretmanager.googleapis.com OR
  drive.googleapis.com
)"
```

You should see all five listed.

---

## Step 3: Store the Zoom webhook secret in Secret Manager

Storing the webhook secret in plain env vars (via `--set-env-vars`) would
expose it on the command line, in deploy logs, and in shell history. Secret
Manager avoids all of that: the value is stored encrypted at rest, mounted
into the running container as an env var, and never appears anywhere else.

**Run this in your terminal** (not through any automation) so you can paste
the secret at the prompt:

```bash
# zsh syntax — uses `name?prompt` for read prompts
read -s "ZOOM_SECRET?Webhook secret: " && echo \
  && printf '%s' "$ZOOM_SECRET" | gcloud secrets create zoom-webhook-secret --data-file=- \
  && unset ZOOM_SECRET
```

Breakdown:

- `read -s` reads input silently (no echo to the terminal)
- The value is stored in `$ZOOM_SECRET` for one command only (not exported)
- `printf '%s'` writes it to stdout with no trailing newline
- Pipes into `gcloud secrets create zoom-webhook-secret --data-file=-`
- `unset` clears the variable from the shell immediately after

**Note for bash users:** replace `read -s "ZOOM_SECRET?Webhook secret: "` with
`read -s -p "Webhook secret: " ZOOM_SECRET`. Zsh uses the `"var?prompt"` form
because in zsh, `-p` means "read from coprocess."

Expected output on success:

```
Created version [1] of the secret [zoom-webhook-secret].
```

If the secret already exists (re-running this step), you'll get an error.
To add a new version instead of creating a new secret, use `versions add`:

```bash
read -s "ZOOM_SECRET?Webhook secret: " && echo \
  && printf '%s' "$ZOOM_SECRET" | gcloud secrets versions add zoom-webhook-secret --data-file=- \
  && unset ZOOM_SECRET
```

---

## Step 4: Grant the service account access to the secret

The Cloud Run runtime service account needs the
`roles/secretmanager.secretAccessor` role on this specific secret (principle of
least privilege — no broader Secret Manager access).

```bash
gcloud secrets add-iam-policy-binding zoom-webhook-secret \
  --member="serviceAccount:<YOUR_SERVICE_ACCOUNT>" \
  --role="roles/secretmanager.secretAccessor"
```

Expected output: an updated IAM policy with the binding shown.

---

## Step 5: Provision Cloud Tasks infra

The bridge uses Cloud Tasks to move long-running download/upload work
off the Zoom webhook request path (see `docs/design-decisions.md`
"Decision 6" for why). Before the first deploy, provision the queue
and IAM bindings:

```bash
PROJECT_ID=<YOUR_PROJECT_ID> \
  REGION=us-east1 \
  SERVICE_NAME=zoom-recording-bridge \
  SERVICE_ACCOUNT=<YOUR_SERVICE_ACCOUNT> \
  ./scripts/provision-cloud-tasks-infra.sh
```

This creates:

- A Cloud Tasks queue (`zoom-recording-jobs` by default) with
  `max-concurrent-dispatches=1` (serializes task execution, same
  invariant the old in-process mutex gave us) and
  `max-retry-duration=14400s` (4 h, comfortably under Zoom's 24 h
  download_token expiry).
- IAM binding: service account gets `roles/cloudtasks.enqueuer` (the
  bridge creates tasks from `/webhook`).
- IAM binding: service account gets `roles/run.invoker` on the Cloud
  Run service (Cloud Tasks, signing as this SA, invokes
  `/process-event`).
- Cloud Run timeout bumped to **1800s** so the service request window
  matches the 30-min dispatch deadline we set on each task. Without
  this, Cloud Run would kill the `/process-event` handler at the
  default 5 min regardless of what Cloud Tasks is willing to wait for.

The script prints the `CLOUD_TASKS_QUEUE` resource name you'll need
for the deploy step below.

## Step 6: Deploy to Cloud Run

This is the main event. Cloud Build compiles the Go code in the repo's
`Dockerfile`, pushes the image to Artifact Registry, and deploys it to Cloud
Run.

From the **repo root**:

```bash
gcloud run deploy zoom-recording-bridge \
  --source . \
  --region us-east1 \
  --allow-unauthenticated \
  --service-account <YOUR_SERVICE_ACCOUNT> \
  --set-env-vars DRIVE_ROOT_FOLDER_ID=<YOUR_FOLDER_ID> \
  --set-env-vars PROCESS_EVENT_URL=<YOUR_SERVICE_URL>/process-event \
  --set-env-vars CLOUD_TASKS_QUEUE=projects/<YOUR_PROJECT_ID>/locations/us-east1/queues/zoom-recording-jobs \
  --set-env-vars TASKS_INVOKER_SA=<YOUR_SERVICE_ACCOUNT> \
  --update-secrets ZOOM_WEBHOOK_SECRET_TOKEN=zoom-webhook-secret:latest \
  --max-instances=1 \
  --timeout=1800
```

**First-deploy chicken-and-egg:** `PROCESS_EVENT_URL` needs the service
URL, which you don't know until the first deploy completes. Two options:

- **Deploy once without `PROCESS_EVENT_URL`** (it's required by
  `loadConfig`, so the service won't start — that's expected). Run
  `gcloud run services describe zoom-recording-bridge --region=us-east1
  --format="value(status.url)"` to get the URL, then redeploy with the
  full command above.
- **Or use a custom domain / reserved URL** you know in advance.

Replace placeholders:

- `<YOUR_FOLDER_ID>` — Drive folder ID from the prerequisites (the URL
  segment: `drive.google.com/drive/folders/<THIS_IS_THE_ID>`)
- `<YOUR_PROJECT_ID>` — the GCP project
- `<YOUR_SERVICE_URL>` — `https://zoom-recording-bridge-<hash>.us-east1.run.app`
- `<YOUR_SERVICE_ACCOUNT>` — runtime service account email

What each flag does:

| Flag | Purpose |
|---|---|
| `--source .` | Build from the Dockerfile in the current directory |
| `--region us-east1` | South Carolina — lowest latency for East Coast users |
| `--allow-unauthenticated` | Public endpoint; Zoom needs to reach `/webhook` from the internet |
| `--service-account ...` | Runtime identity for the container (inherits Drive + enqueue access) |
| `--set-env-vars DRIVE_ROOT_FOLDER_ID=...` | Root Drive folder for uploads |
| `--set-env-vars PROCESS_EVENT_URL=...` | Full URL for `/process-event`; used as the OIDC audience and the Cloud Tasks target URL |
| `--set-env-vars CLOUD_TASKS_QUEUE=...` | Full resource name of the queue created in Step 5 |
| `--set-env-vars TASKS_INVOKER_SA=...` | Service account Cloud Tasks impersonates when signing OIDC tokens for `/process-event` |
| `--update-secrets ZOOM_WEBHOOK_SECRET_TOKEN=...` | Mount the Secret Manager secret as that env var |
| `--max-instances=1` | Cap horizontal scaling at one instance |
| `--timeout=1800` | Cloud Run request timeout 30 min, matches Cloud Tasks' max dispatch deadline |

**First deploy takes 3-5 minutes.** You'll see progress for:

1. Validating configuration
2. Creating the container repository (first time only)
3. Uploading sources
4. Building the container (the longest step)
5. Setting IAM policy
6. Creating the revision
7. Routing traffic

**Subsequent deploys are faster** (~1-2 minutes) thanks to the Cloud Build
cache.

When it succeeds, the last line of output is:

```
Service URL: https://zoom-recording-bridge-<hash>.us-east1.run.app
```

**Save this URL** — you'll use it in step 8 (Zoom Marketplace config).

---

## Step 7: Sanity check the deployed service

Before touching Zoom, verify the deployment is healthy.

### 6a: Root path (trivial reachability check)

```bash
curl https://zoom-recording-bridge-<hash>.us-east1.run.app/
```

Expected:

```
Zoom recording → Google Drive bridge is running.
```

### 6b: Validation handshake endpoint

Send a synthetic `endpoint.url_validation` event to `/webhook` and verify the
response shape:

```bash
curl -s -X POST \
  -H "Content-Type: application/json" \
  -d '{"event":"endpoint.url_validation","payload":{"plainToken":"abc123"}}' \
  https://zoom-recording-bridge-<hash>.us-east1.run.app/webhook
```

Expected:

```json
{"encryptedToken":"<64-char hex string>","plainToken":"abc123"}
```

This proves:

- The endpoint is reachable over HTTPS
- Request routing works
- The handler reads `ZOOM_WEBHOOK_SECRET_TOKEN` from Secret Manager at runtime
- HMAC computation executes
- Response JSON is formatted correctly

You can't verify the `encryptedToken` value is *correct* without knowing the
real secret — but Zoom will verify it against the same secret in step 8.

### Cloud Run reserves `/healthz`

Cloud Run (via Google Frontend) intercepts the `/healthz` path before it
reaches your container and returns a 404 from Google's edge. If you ever
need an HTTP-based health check endpoint on a Cloud Run service, use any
path *other than* `/healthz` — e.g., `/health`, `/livez`, or `/ping`.

The bridge doesn't have a dedicated health endpoint. Cloud Run's default
startup check is a TCP probe to the configured port, which is satisfied
automatically by our `http.ListenAndServe`. If you need a trivial
liveness signal, `/` returns 200 with plain text.

---

## Step 8: Configure the Zoom Marketplace app

Now point Zoom at the Cloud Run URL.

1. Open [marketplace.zoom.us](https://marketplace.zoom.us/)
2. **Develop → Manage → \<your app\>**
3. Go to **Feature → Event Subscriptions** (toggle on if it isn't already)
4. In the **Event notification endpoint URL** field, paste:

   ```
   https://zoom-recording-bridge-<hash>.us-east1.run.app/webhook
   ```

5. Click **Validate**

   Zoom fires an `endpoint.url_validation` event to your URL. Your service
   computes the HMAC and responds. Zoom verifies the HMAC against its copy of
   the secret. Expected result: a green checkmark or "Validated" confirmation.

6. Under **Add Event**, subscribe to **both** of these events under the
   **Recording** category:

   - **All Recordings have completed** (`recording.completed`) — fires when
     the MP4, M4A, and timeline files are ready
   - **Transcript files for the recording have completed**
     (`recording.transcript_completed`) — fires separately when Zoom's
     transcript generation finishes, which may be seconds to minutes
     after `recording.completed` (and very rarely slightly *before*)

   Both events must be subscribed. The service handles them with the same
   code path, using a per-meeting lock to serialize folder creation when
   they arrive near-simultaneously.

7. **Save** the event subscription

8. **Activate the app** (if there's an activation step). Server-to-Server
   OAuth apps require activation before events fire.

---

## Step 9: End-to-end test with a real meeting

The ultimate verification: run a real Zoom recording through the whole
pipeline.

### Start tailing Cloud Run logs

In one terminal, start the log tailer:

```bash
./scripts/tail-cloud-run-logs.sh
```

(Or run `gcloud beta run services logs tail zoom-recording-bridge
--region=us-east1` directly.)

Keep this running.

### Record a short test meeting

1. Start a new Zoom meeting on the account tied to the Marketplace app
2. Click **Record** and choose **Record to the Cloud** (not local)
3. Talk for 30-60 seconds so there's real audio/video content
4. **End the meeting**

### Wait for Zoom to process the recording

Zoom does its own processing before firing `recording.completed`. Timing:

- **Short meetings (~1 min):** usually 1-5 minutes
- **Longer meetings:** 10+ minutes, occasionally longer under load
- You'll receive a "Cloud recording is now available" email from Zoom
  at roughly the same time the webhook fires

### What to look for in the logs

When the webhook fires, you should see:

```
processing recording: topic="Test Meeting" meetingID=... host=you@example.com files=3
uploaded: Test Meeting-shared_screen_with_speaker_view.mp4
uploaded: Test Meeting-audio_only.m4a
uploaded: Test Meeting-audio_transcript.vtt
done: 3/3 files uploaded to 2026-04-10-Test Meeting
```

### Verify the files in Drive

Open the Drive folder in your browser. You should see:

```
<root folder>/
  <your_username>/                           # e.g., "skapadia" — the local
                                             # part of the host's email
    2026-04-10-Test Meeting/
      raw/
        Test Meeting-shared_screen_with_speaker_view.mp4
        Test Meeting-audio_only.m4a
        Test Meeting-audio_transcript.vtt
      meeting-metadata.json
```

The bridge groups recordings by host so each consultant's meetings live in
their own folder. Multiple meetings from the same host accumulate as
sibling subfolders.

Open the files to confirm they play/display correctly.

If all of that works, **the bridge is fully operational**.

---

## Updating the deployment

Whenever you push code changes and want them live on Cloud Run:

```bash
cd /path/to/zoom-recording-google-drive-bridge
gcloud run deploy zoom-recording-bridge \
  --source . \
  --region us-east1 \
  --allow-unauthenticated \
  --service-account <YOUR_SERVICE_ACCOUNT> \
  --set-env-vars DRIVE_ROOT_FOLDER_ID=<YOUR_FOLDER_ID> \
  --set-env-vars PROCESS_EVENT_URL=<YOUR_SERVICE_URL>/process-event \
  --set-env-vars CLOUD_TASKS_QUEUE=projects/<YOUR_PROJECT_ID>/locations/us-east1/queues/zoom-recording-jobs \
  --set-env-vars TASKS_INVOKER_SA=<YOUR_SERVICE_ACCOUNT> \
  --update-secrets ZOOM_WEBHOOK_SECRET_TOKEN=zoom-webhook-secret:latest \
  --max-instances=1 \
  --timeout=1800
```

Same command as the initial deploy. Cloud Run creates a new revision and
routes 100% of traffic to it by default.

The URL **does not change** across revisions, so you never have to update
the Zoom app config.

---

## Rotating the webhook secret

If the secret is compromised or you just want to rotate it:

1. Generate a new secret in the Zoom Marketplace app (or have Zoom generate
   one) — save it somewhere safe temporarily
2. Add it as a new version in Secret Manager:

   ```bash
   read -s "ZOOM_SECRET?New webhook secret: " && echo \
     && printf '%s' "$ZOOM_SECRET" | gcloud secrets versions add zoom-webhook-secret --data-file=- \
     && unset ZOOM_SECRET
   ```

3. Re-deploy the service (picks up the new `:latest` version):

   ```bash
   gcloud run services update zoom-recording-bridge \
     --region us-east1 \
     --update-secrets ZOOM_WEBHOOK_SECRET_TOKEN=zoom-webhook-secret:latest
   ```

4. Update the secret value in the Zoom Marketplace app to match
5. Test validation by re-saving the endpoint URL in the Zoom app (triggers a
   fresh `endpoint.url_validation` event)
6. Once confirmed working, disable (or delete) the old version in Secret
   Manager:

   ```bash
   gcloud secrets versions list zoom-webhook-secret
   gcloud secrets versions disable <OLD_VERSION_NUMBER> --secret=zoom-webhook-secret
   ```

---

## Rolling back

To roll back to a previous revision:

```bash
# List revisions
gcloud run revisions list --service=zoom-recording-bridge --region=us-east1

# Route 100% traffic to a specific revision
gcloud run services update-traffic zoom-recording-bridge \
  --region us-east1 \
  --to-revisions=zoom-recording-bridge-<REVISION_ID>=100
```

---

## Monitoring and debugging

### Tail live logs

```bash
./scripts/tail-cloud-run-logs.sh
```

### Query historical logs

```bash
gcloud run services logs read zoom-recording-bridge \
  --region=us-east1 \
  --limit=100
```

### Filter for errors only

```bash
gcloud logging read \
  'resource.type=cloud_run_revision AND resource.labels.service_name=zoom-recording-bridge AND severity>=ERROR' \
  --limit=50 \
  --format=json
```

### Check the service status

```bash
gcloud run services describe zoom-recording-bridge --region=us-east1
```

---

## Cleanup (tearing everything down)

If you ever need to remove the whole deployment:

```bash
# Delete the Cloud Run service
gcloud run services delete zoom-recording-bridge --region=us-east1

# Delete the secret
gcloud secrets delete zoom-webhook-secret

# Remove the IAM binding (if the secret is being retained)
gcloud secrets remove-iam-policy-binding zoom-webhook-secret \
  --member="serviceAccount:<YOUR_SERVICE_ACCOUNT>" \
  --role="roles/secretmanager.secretAccessor"
```

The Artifact Registry images (in the `cloud-run-source-deploy` repo) can be
deleted from the Google Cloud Console if you want to free storage, but they
don't cost anything meaningful to keep.

---

## Related docs

- [`synthetic-test-driver.md`](./synthetic-test-driver.md) — plan for the
  synthetic webhook test driver used to verify the Drive write path locally
  before deployment
- [`../README.md`](../README.md) — short project overview and env var
  reference
