#!/usr/bin/env bash
# scripts/setup-gke.sh — One-time GKE setup for the ci-runner service.
# Run this after scripts/setup.sh (which creates the GCP service account and secrets).
# Safe to re-run (idempotent).
#
# Usage:
#   bash scripts/setup-gke.sh
#   PROJECT=my-project CLUSTER=my-cluster bash scripts/setup-gke.sh

set -euo pipefail

PROJECT="${PROJECT:-dlorenc-chainguard}"
REGION="${REGION:-us-central1}"
CLUSTER="${CLUSTER:-chainguardener}"
NAMESPACE="docstore-ci"
KSA="ci-runner"
GSA="ci-runner@${PROJECT}.iam.gserviceaccount.com"
DEPLOYER_SA="docstore-deployer@${PROJECT}.iam.gserviceaccount.com"

echo "==> Granting deployer SA container.developer (needed for kubectl in CI)..."
gcloud projects add-iam-policy-binding "${PROJECT}" \
  --member="serviceAccount:${DEPLOYER_SA}" \
  --role=roles/container.developer \
  --condition=None \
  --quiet
echo "    Done."

echo "==> Granting deployer SA secretVersionAdder on ci-runner-url (needed to update LB IP after deploy)..."
gcloud secrets add-iam-policy-binding ci-runner-url \
  --project="${PROJECT}" \
  --member="serviceAccount:${DEPLOYER_SA}" \
  --role=roles/secretmanager.secretVersionAdder \
  --quiet
echo "    Done."

echo "==> Getting cluster credentials..."
gcloud container clusters get-credentials "${CLUSTER}" \
  --region="${REGION}" \
  --project="${PROJECT}"
echo "    Done."

echo "==> Creating namespace ${NAMESPACE} (if not exists)..."
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
echo "    Done."

echo "==> Binding GCP SA to k8s SA for Workload Identity..."
gcloud iam service-accounts add-iam-policy-binding "${GSA}" \
  --project="${PROJECT}" \
  --role=roles/iam.workloadIdentityUser \
  --member="serviceAccount:${PROJECT}.svc.id.goog[${NAMESPACE}/${KSA}]" \
  --quiet
echo "    Done."

echo "==> Populating k8s Secret from Secret Manager..."
WEBHOOK_SECRET=$(gcloud secrets versions access latest \
  --secret=ci-runner-webhook-secret \
  --project="${PROJECT}")
RUNNER_URL=$(gcloud secrets versions access latest \
  --secret=ci-runner-url \
  --project="${PROJECT}")

kubectl create secret generic "${KSA}" \
  --namespace="${NAMESPACE}" \
  --from-literal=webhook-secret="${WEBHOOK_SECRET}" \
  --from-literal=runner-url="${RUNNER_URL}" \
  --dry-run=client -o yaml | kubectl apply -f -
echo "    Done."

echo ""
echo "GKE setup complete."
echo ""
echo "NEXT STEPS:"
echo ""
echo "  1. Build and push the GKE image:"
echo "     docker build -f Dockerfile.ci-runner-gke \\"
echo "       -t us-central1-docker.pkg.dev/${PROJECT}/images/ci-runner-gke:latest ."
echo "     docker push us-central1-docker.pkg.dev/${PROJECT}/images/ci-runner-gke:latest"
echo ""
echo "  2. Deploy to GKE:"
echo "     kubectl apply -f deploy/k8s/ci-runner.yaml"
echo ""
echo "  3. Test with port-forward:"
echo "     kubectl port-forward svc/ci-runner 8080:8080 -n ${NAMESPACE}"
echo "     curl -s -X POST http://localhost:8080/run \\"
echo "       -H 'Content-Type: application/json' \\"
echo "       -d '{\"repo\":\"default/default\",\"branch\":\"main\",\"head_sequence\":1}'"
