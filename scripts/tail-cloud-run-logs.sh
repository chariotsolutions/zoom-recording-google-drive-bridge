#!/usr/bin/env bash
#
# Tail logs from the deployed Cloud Run service.
#
# Usage:
#   ./scripts/tail-cloud-run-logs.sh
#
# Press Ctrl+C to stop.
#
# Requires:
#   - gcloud CLI authenticated
#   - the active project set to <YOUR_PROJECT_ID> (or override below)
#   - Cloud Run service already deployed

set -euo pipefail

SERVICE="${SERVICE:-zoom-recording-bridge}"
REGION="${REGION:-us-east1}"
PROJECT="${PROJECT:-<YOUR_PROJECT_ID>}"

echo "Tailing logs for $SERVICE in $REGION (project: $PROJECT)"
echo "Ctrl+C to stop."
echo ""

exec gcloud beta run services logs tail "$SERVICE" \
  --region="$REGION" \
  --project="$PROJECT"
