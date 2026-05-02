#!/bin/sh
set -e

# Private registry authentication (e.g. GCP Artifact Registry) is a separate
# concern and is not handled here. Build images must be on public registries.
# Tracked in issue #391.

# Set up a loop-backed ext4 volume at /var/lib/buildkit so that buildkitd can use the
# native overlayfs snapshotter. The Kata CLH guest rootfs is virtiofs, which does not
# support overlayfs upper directories (EINVAL). ext4 does support them.
#
# The Kata guest kernel has CONFIG_BLK_DEV_LOOP=y (built-in) but udev does not run
# inside the VM, so the loop device nodes must be created manually before use.
#
# Guard: on container restart within the same Kata VM the loop device and mount from
# the previous container run are still active in the VM kernel. Skip setup if already
# mounted so we don't exhaust the 8 loop devices we create (loop0-loop7).
if grep -q ' /var/lib/buildkit ' /proc/mounts; then
  echo "loop-ext4 already mounted at /var/lib/buildkit, reusing" >&2
else
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
fi

# Write buildkitd config so the ci-registry service (plain HTTP) is treated as insecure.
# Without this, BuildKit attempts HTTPS for cache import/export auth challenges even though
# ci-registry.docstore-ci.svc.cluster.local runs on plain HTTP.
mkdir -p /etc/buildkit
cat > /etc/buildkit/buildkitd.toml << 'TOML'
[registry."ci-registry.docstore-ci.svc.cluster.local"]
  http = true
  insecure = true
TOML

# Start buildkitd in background (standard, non-rootless — runs natively inside Kata VM).
# --oci-worker-net=host ensures build containers share the host network namespace so they
# can reach dockerd at tcp://localhost:2375.
# overlayfs snapshotter now works because /var/lib/buildkit is ext4 (supports upper dirs).
buildkitd --addr tcp://localhost:1234 --oci-worker-net=host --oci-worker-snapshotter=overlayfs --config /etc/buildkit/buildkitd.toml &

# Start dockerd in background.
# -H tcp://127.0.0.1:2375 exposes dockerd over TCP so build containers running with
# host network can use DOCKER_HOST=tcp://localhost:2375.
dockerd --userland-proxy=false -H unix:///var/run/docker.sock -H tcp://127.0.0.1:2375 &

exec ci-worker
