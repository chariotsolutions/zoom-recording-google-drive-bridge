#!/usr/bin/env bash
#
# Provision the Cloud Tasks queue + IAM + Cloud Run timeout for the
# zoom-recording-google-drive-bridge.
#
# This script is idempotent-ish: creating a queue that already exists
# errors out, so delete first if you need to recreate. IAM bindings and
# service updates are fine to re-apply.
#
# Why this lives in the repo (this is an OSS repo):
#   The gcloud commands are parameterized via env vars, not hardcoded to
#   Chariot's specific project / SA / region. Anyone forking the repo
#   provides their own values.
#
# Required env vars:
#   PROJECT_ID        Google Cloud project id (e.g. my-bridge-project)
#   REGION            Cloud Run region hosting the bridge (e.g. us-east1)
#   SERVICE_NAME      Cloud Run service name (e.g. zoom-recording-bridge)
#   SERVICE_ACCOUNT   Email of the SA the bridge runs as and that Cloud
#                     Tasks will impersonate for OIDC (e.g.
#                     my-bot@my-project.iam.gserviceaccount.com)
#
# Optional env vars:
#   QUEUE_NAME        Cloud Tasks queue name (default: zoom-recording-jobs)
#
# Usage:
#   PROJECT_ID=my-proj REGION=us-east1 \
#     SERVICE_NAME=zoom-recording-bridge \
#     SERVICE_ACCOUNT=my-bot@my-proj.iam.gserviceaccount.com \
#     ./scripts/provision-cloud-tasks-infra.sh

set -euo pipefail

: "${PROJECT_ID:?PROJECT_ID is required}"
: "${REGION:?REGION is required}"
: "${SERVICE_NAME:?SERVICE_NAME is required}"
: "${SERVICE_ACCOUNT:?SERVICE_ACCOUNT is required (email of the bridge runtime service account)}"
QUEUE_NAME="${QUEUE_NAME:-zoom-recording-jobs}"

echo "== provisioning Cloud Tasks infra =="
echo "project:      $PROJECT_ID"
echo "region:       $REGION"
echo "service:      $SERVICE_NAME"
echo "SA:           $SERVICE_ACCOUNT"
echo "queue:        $QUEUE_NAME"
echo ""

echo "-- creating Cloud Tasks queue --"
# max-concurrent-dispatches=1 serializes task execution, which replaces
# the in-process per-meeting mutex the bridge used to carry.
# max-retry-duration=14400s (4h) is comfortably under Zoom's 24h
# download_token validity window.
gcloud tasks queues create "$QUEUE_NAME" \
  --project="$PROJECT_ID" \
  --location="$REGION" \
  --max-dispatches-per-second=10 \
  --max-concurrent-dispatches=1 \
  --max-attempts=10 \
  --max-retry-duration=14400s \
  || echo "(queue '$QUEUE_NAME' may already exist — continuing)"

echo ""
echo "-- granting roles/cloudtasks.enqueuer to $SERVICE_ACCOUNT --"
# The bridge creates tasks from its /webhook handler.
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:$SERVICE_ACCOUNT" \
  --role=roles/cloudtasks.enqueuer

echo ""
echo "-- granting roles/run.invoker on $SERVICE_NAME to $SERVICE_ACCOUNT --"
# Cloud Tasks, signing as this SA, invokes the /process-event endpoint
# on the Cloud Run service. The SA needs permission to invoke its own
# service (this is a common pattern, not as circular as it looks).
gcloud run services add-iam-policy-binding "$SERVICE_NAME" \
  --region="$REGION" \
  --project="$PROJECT_ID" \
  --member="serviceAccount:$SERVICE_ACCOUNT" \
  --role=roles/run.invoker

echo ""
echo "-- setting Cloud Run request timeout to 1800s (30 min) --"
# Cloud Run's default request timeout is 5 min. Cloud Tasks can hold a
# dispatch open for up to 30 min. The effective budget for the
# /process-event handler is min(DispatchDeadline, Cloud Run --timeout),
# so Cloud Run must be at least as large as the deadline we want.
gcloud run services update "$SERVICE_NAME" \
  --region="$REGION" \
  --project="$PROJECT_ID" \
  --timeout=1800

echo ""
echo "== done =="
echo ""
echo "Set these env vars in your Cloud Run deploy:"
echo "  CLOUD_TASKS_QUEUE=projects/$PROJECT_ID/locations/$REGION/queues/$QUEUE_NAME"
echo "  TASKS_INVOKER_SA=$SERVICE_ACCOUNT"
echo "  PROCESS_EVENT_URL=https://<your-service-url>/process-event"
