#!/bin/sh
set -e

buildkitd --oci-worker-snapshotter=native &
until [ -S /run/buildkit/buildkitd.sock ]; do sleep 0.1; done

exec ci-runner \
  --buildkit-addr unix:///run/buildkit/buildkitd.sock \
  --docstore-url "${DOCSTORE_URL}" \
  --port "${PORT:-8080}"
