#!/usr/bin/env bash
# smoke.sh IMAGE [REVISION]: start IMAGE the way hetzner runs it, against a
# throwaway Postgres, and fail unless it migrates an empty database, loads
# the example configuration, becomes healthy and ready, and (with REVISION)
# names that commit. build-push-ghcr.yml runs it on each architecture
# before any tag points at the image, so an image that cannot boot is never
# published. Needs docker; run from the repository root (it mounts
# examples/ as /config).
set -euo pipefail

image=${1:?usage: deploy/smoke.sh IMAGE [REVISION]}
want=${2:-}
# The Postgres hetzner runs (compose.orbit.yml), as in the Makefile and CI.
PG_IMAGE=${PG_IMAGE:-postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5}
READY_TIMEOUT=${SMOKE_READY_TIMEOUT:-120}

id=orbit-smoke-$$
log() { echo "smoke: $*"; }
fail() {
  log "FAILED: $*"
  docker logs "$id-orbit" 2>&1 | tail -n 50 || true
  exit 1
}
cleanup() {
  docker rm -f "$id-orbit" "$id-db" >/dev/null 2>&1 || true
  docker network rm "$id" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker network create "$id" >/dev/null
docker run -d --name "$id-db" --network "$id" -e POSTGRES_USER=orbit -e POSTGRES_PASSWORD=smoke -e POSTGRES_DB=orbit "$PG_IMAGE" >/dev/null
for _ in $(seq 60); do
  docker exec "$id-db" pg_isready -U orbit -d orbit >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$id-db" pg_isready -U orbit -d orbit >/dev/null || fail "postgres never became ready"

common=(--network "$id" -e "ORBIT_DATABASE_URL=postgres://orbit:smoke@$id-db:5432/orbit?sslmode=disable" -v "$PWD/examples:/config:ro")

rev=$(docker run --rm "$image" version)
log "revision $rev"
if [ -n "$want" ] && [ "$rev" != "$want" ]; then fail "image says revision $rev, want $want"; fi

docker run --rm "${common[@]}" "$image" migrate || fail "orbit migrate on an empty database"
log "migrated"

# Every variable the example configuration references gets a dummy value,
# so serve starts as it would with its real environment; nothing it reaches
# with them exists (no executors, example.com hosts).
out=$(docker run --rm "${common[@]}" "$image" validate /config) || fail "orbit validate /config"
echo "$out"
envs=()
for v in $(sed -n 's/^environment referenced: //p' <<<"$out" | tr -d ','); do
  # Only variable names: an entry can read "A or B".
  [[ $v =~ ^[A-Z_][A-Z0-9_]*$ ]] && envs+=(-e "$v=smoke")
done

docker run -d --name "$id-orbit" "${common[@]}" "${envs[@]}" \
  -e "ORBIT_API_TOKEN=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
  -e ORBIT_WORKERS=0 -e ORBIT_WORKER_NAME=smoke "$image" >/dev/null

# What orbit-rollout.sh waits for before it drains the old instance: docker
# healthy and `orbit healthcheck --ready`.
deadline=$((SECONDS + READY_TIMEOUT))
while :; do
  state=$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "$id-orbit")
  case $state in running*) ;; *) fail "orbit serve exited ($state)" ;; esac
  if [ "$state" = "running healthy" ] && docker exec "$id-orbit" orbit healthcheck --ready >/dev/null 2>&1; then break; fi
  [ "$SECONDS" -lt "$deadline" ] || fail "not healthy and ready within ${READY_TIMEOUT}s ($state)"
  sleep 2
done
log "ok: $image is healthy and ready"
