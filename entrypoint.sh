#!/bin/sh
set -e

# Configure docker-credential-gcr so buildkitd can pull from
# Artifact Registry using the instance's workload identity.
docker-credential-gcr configure-docker \
  --registries=us-central1-docker.pkg.dev,mirror.gcr.io,gcr.io 2>/dev/null || true

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
