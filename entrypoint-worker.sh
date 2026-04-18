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

# Set up a loop-backed ext4 volume at /var/lib/buildkit so that buildkitd can use the
# native overlayfs snapshotter. The Kata CLH guest rootfs is virtiofs, which does not
# support overlayfs upper directories (EINVAL). ext4 does support them.
#
# The Kata guest kernel has CONFIG_BLK_DEV_LOOP=y (built-in) but udev does not run
# inside the VM, so the loop device nodes must be created manually before use.
echo "setting up loop-backed ext4 for /var/lib/buildkit..." >&2
mknod /dev/loop-control c 10 237 2>/dev/null || true
for i in $(seq 0 7); do
  mknod /dev/loop$i b 7 $i 2>/dev/null || true
done
# Sparse file: truncate creates a 20G hole without writing bytes (fast, disk-backed via
# virtiofs so no RAM pressure). mkfs.ext4 with lazy_itable_init completes in <1s.
truncate -s 20G /var/lib/buildkit.img
mkfs.ext4 -F -q -E lazy_itable_init=1,lazy_journal_init=1 /var/lib/buildkit.img
LOOP=$(losetup -f --show /var/lib/buildkit.img)
mkdir -p /var/lib/buildkit
mount "$LOOP" /var/lib/buildkit
echo "loop-ext4 mounted at /var/lib/buildkit on $LOOP" >&2

# Start buildkitd in background (standard, non-rootless — runs natively inside Kata VM).
# --oci-worker-net=host ensures build containers share the host network namespace so they
# can reach dockerd at tcp://localhost:2375.
# overlayfs snapshotter now works because /var/lib/buildkit is ext4 (supports upper dirs).
buildkitd --addr tcp://localhost:1234 --oci-worker-net=host --oci-worker-snapshotter=overlayfs &

# Start dockerd in background.
# -H tcp://127.0.0.1:2375 exposes dockerd over TCP so build containers running with
# host network can use DOCKER_HOST=tcp://localhost:2375.
dockerd --userland-proxy=false -H unix:///var/run/docker.sock -H tcp://127.0.0.1:2375 &

# Wait for dockerd to be ready on TCP port 2375.
echo "waiting for dockerd..." >&2
i=0
while ! curl -sf --max-time 1 http://127.0.0.1:2375/_ping >/dev/null 2>&1; do
  i=$((i+1))
  if [ "$i" -ge 60 ]; then
    echo "ERROR: dockerd not ready after 60s" >&2
    exit 1
  fi
  sleep 1
done
echo "dockerd ready" >&2

exec ci-worker
