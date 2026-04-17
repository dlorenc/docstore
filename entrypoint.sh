#!/bin/sh
set -e

# Write buildkitd config to authenticate with Artifact Registry using
# the instance's workload identity token.
mkdir -p /etc/buildkit
TOKEN=$(wget -qO- --header "Metadata-Flavor: Google" \
  "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" \
  | grep -o '"access_token":"[^"]*"' | cut -d'"' -f4 || true)
if [ -n "$TOKEN" ]; then
  cat > /etc/buildkit/buildkitd.toml <<TOML
[registry."us-central1-docker.pkg.dev"]
  [registry."us-central1-docker.pkg.dev".auth]
    token = "$TOKEN"
TOML
fi

buildkitd --oci-worker-snapshotter=native --config=/etc/buildkit/buildkitd.toml &
until [ -S /run/buildkit/buildkitd.sock ]; do sleep 0.1; done

if [ -n "${DEV_IDENTITY}" ]; then
  set -- "$@" --dev-identity "${DEV_IDENTITY}"
fi

exec ci-runner \
  --buildkit-addr unix:///run/buildkit/buildkitd.sock \
  --docstore-url "${DOCSTORE_URL}" \
  --port "${PORT:-8080}" \
  "$@"
