#!/bin/sh
set -e

# Start buildkitd in background (standard, non-rootless — runs natively inside Kata VM).
buildkitd --addr tcp://localhost:1234 &

# Start dockerd in background.
dockerd &

# Wait for buildkitd to be ready.
until nc -z localhost 1234 2>/dev/null; do sleep 0.1; done
echo "buildkitd ready" >&2

exec ci-worker
