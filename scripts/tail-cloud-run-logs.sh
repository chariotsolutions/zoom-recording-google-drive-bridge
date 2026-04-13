#!/usr/bin/env bash
#
# Tail logs from the deployed Cloud Run service.
#
# Shows the last N historical log entries (default 50), then switches to
# live tailing. This lets you see recent context before watching for new
# events.
#
# Usage:
#   ./scripts/tail-cloud-run-logs.sh
#
# Press Ctrl+C to stop.
#
# Requires:
#   - gcloud CLI authenticated
#   - active project set (gcloud config set project <YOUR_PROJECT_ID>),
#     or PROJECT env var passed to this script
#   - Cloud Run service already deployed
#
# Override defaults via env vars:
#   SERVICE=my-service REGION=us-central1 PROJECT=my-proj HISTORY=20 \
#     ./scripts/tail-cloud-run-logs.sh

set -euo pipefail

SERVICE="${SERVICE:-zoom-recording-bridge}"
REGION="${REGION:-us-east1}"
PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
HISTORY="${HISTORY:-50}"

if [[ -z "$PROJECT" ]]; then
  echo "error: no project set. Run 'gcloud config set project <YOUR_PROJECT_ID>' or pass PROJECT=<id>" >&2
  exit 1
fi

echo "Tailing logs for $SERVICE in $REGION (project: $PROJECT)"
echo ""

# Show the last N log entries in chronological order (oldest first).
echo "=== Last $HISTORY log entries ==="
echo ""
gcloud run services logs read "$SERVICE" \
  --region="$REGION" \
  --project="$PROJECT" \
  --limit="$HISTORY" 2>/dev/null || true
echo ""
echo "=== Live tail (Ctrl+C to stop) ==="
echo ""

exec gcloud beta run services logs tail "$SERVICE" \
  --region="$REGION" \
  --project="$PROJECT"
