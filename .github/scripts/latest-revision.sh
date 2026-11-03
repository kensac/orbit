#!/usr/bin/env bash
# latest-revision.sh IMAGE: the commit IMAGE:latest was built from (its
# org.opencontainers.image.revision label), or nothing when there is no
# latest or no label.
set -uo pipefail
docker buildx imagetools inspect "$1:latest" \
  --format '{{ index (index .Image "linux/amd64").Config.Labels "org.opencontainers.image.revision" }}' 2>/dev/null || true
