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

# Mount a loop-backed ext4 filesystem for buildkitd's data directory.
# The Kata CLH guest rootfs is served via virtiofs which does not support
# overlayfs upper dirs (EINVAL). A loop-mounted ext4 volume gives buildkitd
# a real block-backed filesystem where overlayfs works natively, enabling
# the full overlay snapshotter instead of the slow native (no-CoW) fallback.
truncate -s 20G /tmp/buildkit-disk.img
mkfs.ext4 -F -q /tmp/buildkit-disk.img
mkdir -p /var/lib/buildkit
mount -o loop /tmp/buildkit-disk.img /var/lib/buildkit

# Start buildkitd in background (standard, non-rootless — runs natively inside Kata VM).
# --oci-worker-net=host ensures build containers share the host network namespace so they
# can reach dockerd at tcp://localhost:2375.
# Snapshotter defaults to auto which will now select overlayfs on the ext4 volume above.
buildkitd --addr tcp://localhost:1234 --oci-worker-net=host &

# Start dockerd in background.
# -H tcp://127.0.0.1:2375 exposes dockerd over TCP so build containers running with
# host network can use DOCKER_HOST=tcp://localhost:2375.
dockerd --userland-proxy=false -H unix:///var/run/docker.sock -H tcp://127.0.0.1:2375 &

exec ci-worker
