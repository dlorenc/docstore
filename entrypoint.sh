#!/bin/sh
set -e

# Fetch workload identity token and write Docker credentials for
# buildkitd to authenticate with Artifact Registry.
TOKEN=$(wget -qO- --header "Metadata-Flavor: Google" \
  "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" \
  | grep -o '"access_token":"[^"]*"' | cut -d'"' -f4 || true)
if [ -n "$TOKEN" ]; then
  AUTH=$(printf "oauth2accesstoken:%s" "$TOKEN" | base64 | tr -d '\n')
  mkdir -p /root/.docker
  cat > /root/.docker/config.json <<JSON
{
  "auths": {
    "us-central1-docker.pkg.dev": {"auth": "$AUTH"},
    "mirror.gcr.io":               {"auth": "$AUTH"},
    "gcr.io":                      {"auth": "$AUTH"}
  }
}
JSON
fi

buildkitd --oci-worker-snapshotter=native &
until [ -S /run/buildkit/buildkitd.sock ]; do sleep 0.1; done

if [ -n "${DEV_IDENTITY}" ]; then
  set -- "$@" --dev-identity "${DEV_IDENTITY}"
fi

exec ci-runner \
  --buildkit-addr unix:///run/buildkit/buildkitd.sock \
  --docstore-url "${DOCSTORE_URL}" \
  --port "${PORT:-8080}" \
  "$@"
