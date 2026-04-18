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

# Create /dev/fuse if it doesn't exist. The Kata CLH guest kernel has FUSE
# built-in (CONFIG_FUSE_FS=y, not a loadable module) so modprobe is a no-op,
# but udev doesn't run in the container so /dev/fuse is never created.
# Device numbers for fuse are always char 10:229.
[ -e /dev/fuse ] || mknod /dev/fuse -m 0666 c 10 229

# Start buildkitd in background (standard, non-rootless — runs natively inside Kata VM).
# --oci-worker-net=host ensures build containers share the host network namespace so they
# can reach dockerd at tcp://localhost:2375.
buildkitd --addr tcp://localhost:1234 --oci-worker-net=host --oci-worker-snapshotter=fuse-overlayfs &

# Start dockerd in background.
# -H tcp://127.0.0.1:2375 exposes dockerd over TCP so build containers running with
# host network can use DOCKER_HOST=tcp://localhost:2375.
dockerd --userland-proxy=false -H unix:///var/run/docker.sock -H tcp://127.0.0.1:2375 &

exec ci-worker
