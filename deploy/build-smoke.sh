#!/usr/bin/env bash
# build-smoke.sh IMAGE: the build sandbox's image (deploy/build.Dockerfile)
# runs the way sandboxd runs it: its tools work as uid 10001 on a read-only
# root with no capabilities, and /work is that uid's. CI runs it on a pull
# request's image, and build-push-ghcr.yml before any tag points at one.
set -euo pipefail
image=${1:?usage: deploy/build-smoke.sh IMAGE}
docker run --rm --read-only --tmpfs /tmp --user 10001:10001 --cap-drop ALL \
  --security-opt no-new-privileges:true "$image" \
  sh -c 'claude --version && go version && git --version && node --version && rg --version | head -1 && make --version | head -1 && test "$(stat -c %u /work)" = 10001'
echo "build-smoke: ok: $image"
