#!/bin/sh
set -e

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
