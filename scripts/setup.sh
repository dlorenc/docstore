#!/usr/bin/env bash
# scripts/setup.sh — One-time infrastructure setup for the ci-runner service.
# Safe to re-run (idempotent).
#
# Usage:
#   bash scripts/setup.sh
#   PROJECT=my-project REGION=us-east1 bash scripts/setup.sh

set -euo pipefail

PROJECT="${PROJECT:-dlorenc-chainguard}"
REGION="${REGION:-us-central1}"

DEPLOYER_SA="docstore-deployer@${PROJECT}.iam.gserviceaccount.com"
CI_RUNNER_SA="ci-runner@${PROJECT}.iam.gserviceaccount.com"
LOG_BUCKET="docstore-ci-logs"
CI_RUNNER_SERVICE="ci-runner"

echo "==> Enabling required APIs..."
gcloud services enable \
  run.googleapis.com \
  secretmanager.googleapis.com \
  storage.googleapis.com \
  artifactregistry.googleapis.com \
  --project="${PROJECT}" --quiet
echo "    Done."

echo "==> Creating ci-runner service account (if not exists)..."
if ! gcloud iam service-accounts describe "${CI_RUNNER_SA}" \
     --project="${PROJECT}" --quiet 2>/dev/null; then
  gcloud iam service-accounts create ci-runner \
    --project="${PROJECT}" \
    --display-name="CI Runner" \
    --quiet
  echo "    Created ${CI_RUNNER_SA}"
else
  echo "    Already exists: ${CI_RUNNER_SA}"
fi

echo "==> Creating GCS log bucket (if not exists)..."
if ! gcloud storage buckets describe "gs://${LOG_BUCKET}" \
     --project="${PROJECT}" 2>/dev/null; then
  gcloud storage buckets create "gs://${LOG_BUCKET}" \
    --project="${PROJECT}" \
    --location="${REGION}" \
    --uniform-bucket-level-access \
    --quiet
  echo "    Created gs://${LOG_BUCKET}"
else
  echo "    Already exists: gs://${LOG_BUCKET}"
fi

echo "==> Granting ci-runner SA storage.objectCreator on log bucket..."
gcloud storage buckets add-iam-policy-binding "gs://${LOG_BUCKET}" \
  --member="serviceAccount:${CI_RUNNER_SA}" \
  --role=roles/storage.objectCreator \
  --project="${PROJECT}" \
  --quiet
echo "    Done."

echo "==> Granting deployer SA run.admin on project (needed to deploy ci-runner)..."
gcloud projects add-iam-policy-binding "${PROJECT}" \
  --member="serviceAccount:${DEPLOYER_SA}" \
  --role=roles/run.admin \
  --condition=None \
  --quiet
echo "    Done."

echo "==> Granting deployer SA iam.serviceAccountUser on ci-runner SA..."
gcloud iam service-accounts add-iam-policy-binding "${CI_RUNNER_SA}" \
  --project="${PROJECT}" \
  --member="serviceAccount:${DEPLOYER_SA}" \
  --role=roles/iam.serviceAccountUser \
  --quiet
echo "    Done."

echo "==> Creating Secret Manager secrets (if not exists)..."
for SECRET_NAME in ci-runner-webhook-secret ci-runner-url; do
  if ! gcloud secrets describe "${SECRET_NAME}" \
       --project="${PROJECT}" 2>/dev/null; then
    gcloud secrets create "${SECRET_NAME}" \
      --project="${PROJECT}" \
      --replication-policy=automatic \
      --quiet
    # Add a placeholder version so the secret is non-empty.
    echo -n "PLACEHOLDER" | gcloud secrets versions add "${SECRET_NAME}" \
      --project="${PROJECT}" \
      --data-file=- \
      --quiet
    echo "    Created ${SECRET_NAME} with placeholder value — update before deploying!"
  else
    echo "    Already exists: ${SECRET_NAME}"
  fi
done

echo "==> Granting ci-runner SA secretmanager.secretAccessor on ci-runner secrets..."
for SECRET_NAME in ci-runner-webhook-secret ci-runner-url; do
  gcloud secrets add-iam-policy-binding "${SECRET_NAME}" \
    --project="${PROJECT}" \
    --member="serviceAccount:${CI_RUNNER_SA}" \
    --role=roles/secretmanager.secretAccessor \
    --quiet
done
echo "    Done."

echo "==> Granting deployer SA secretmanager.viewer on ci-runner secrets..."
# Required so gcloud run deploy can validate the --update-secrets references.
for SECRET_NAME in ci-runner-webhook-secret ci-runner-url; do
  gcloud secrets add-iam-policy-binding "${SECRET_NAME}" \
    --project="${PROJECT}" \
    --member="serviceAccount:${DEPLOYER_SA}" \
    --role=roles/secretmanager.viewer \
    --quiet
done
echo "    Done."

echo ""
echo "Setup complete."
echo ""
echo "NEXT STEPS:"
echo ""
echo "  1. Set the real HMAC webhook secret (replace PLACEHOLDER):"
echo "     echo -n '<your-hmac-secret>' | gcloud secrets versions add ci-runner-webhook-secret \\"
echo "       --data-file=- --project=${PROJECT}"
echo ""
echo "  2. Push to main to trigger the first ci-runner deploy."
echo ""
echo "  3. After the first deploy, get the ci-runner URL and update the secret:"
echo "     URL=\$(gcloud run services describe ${CI_RUNNER_SERVICE} \\"
echo "       --region=${REGION} --project=${PROJECT} --format='value(status.url)')"
echo "     echo -n \"\${URL}\" | gcloud secrets versions add ci-runner-url \\"
echo "       --data-file=- --project=${PROJECT}"
