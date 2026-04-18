#!/bin/sh
set -e

# Configure registry auth for buildkitd using GCP workload identity token.
TOKEN=$(curl -sf -H "Metadata-Flavor: Google" \
  "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" \
  | tr ',' '\n' | grep '"access_token"' | cut -d'"' -f4)
if [ -n "$TOKEN" ]; then
  AUTH=$(printf "oauth2accesstoken:%s" "$TOKEN" | base64 | tr -d '\n')
  mkdir -p "$HOME/.docker"
  printf '{"auths":{"us-central1-docker.pkg.dev":{"auth":"%s"},"mirror.gcr.io":{"auth":"%s"},"gcr.io":{"auth":"%s"}}}' \
    "$AUTH" "$AUTH" "$AUTH" > "$HOME/.docker/config.json"
  echo "registry auth configured" >&2
else
  echo "WARNING: could not fetch workload identity token" >&2
fi
export DOCKER_CONFIG="$HOME/.docker"

# Start rootless buildkitd in background.
# rootlesskit wraps buildkitd in a user namespace so SYS_ADMIN is not required.
rootlesskit buildkitd \
  --oci-worker-snapshotter=native \
  --oci-worker-no-process-sandbox \
  --addr tcp://localhost:1234 &

until nc -z localhost 1234 2>/dev/null; do sleep 0.1; done
echo "buildkitd ready" >&2

# Wait for docker socket from DinD sidecar
until [ -S /var/shared/docker.sock ] 2>/dev/null; do sleep 0.2; done
echo "docker socket ready" >&2

if [ -n "${DEV_IDENTITY}" ]; then
  set -- "$@" --dev-identity "${DEV_IDENTITY}"
fi
if [ -n "${RUNNER_URL}" ]; then
  set -- "$@" --runner-url "${RUNNER_URL}"
fi
if [ -n "${WEBHOOK_SECRET}" ]; then
  set -- "$@" --webhook-secret "${WEBHOOK_SECRET}"
fi

exec ci-runner \
  --buildkit-addr tcp://localhost:1234 \
  --docker-socket /var/shared/docker.sock \
  --docstore-url "${DOCSTORE_URL}" \
  --port "${PORT:-8080}" \
  "$@"
