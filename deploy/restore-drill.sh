#!/usr/bin/env bash
# restore-drill.sh DUMP IMAGE: prove a nightly orbit-db dump restores and
# that orbit can read it, touching nothing live. DUMP is a pg_dump custom
# format file (hetzner's db-dump.timer writes /root/db-dumps/orbit-db.dump);
# IMAGE is the orbit image to read it with (the running one's, normally).
#
# On a throwaway Postgres on an internal network (nothing in it can reach
# anything), it: counts every table's rows in the dump itself; restores it
# with --exit-on-error; checks every table's restored count equals the
# dump's; runs `orbit preflight` (when IMAGE has it: the next rollout's
# migrations, dry-run on real rows) and `orbit migrate` on the copy; makes
# the database read only; and runs `orbit restore-check`, which reads it
# through orbit's own store code and requires at least the dump's rows.
# Everything it started is removed on exit. docs/agents/deployment.md,
# Restore drill.
set -euo pipefail

dump=${1:?usage: deploy/restore-drill.sh DUMP IMAGE}
image=${2:?usage: deploy/restore-drill.sh DUMP IMAGE}
# The Postgres hetzner runs (compose.orbit.yml), as in the Makefile and CI.
PG_IMAGE=${PG_IMAGE:-postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5}

[ -s "$dump" ] || { echo "restore-drill: $dump is missing or empty"; exit 1; }
dump=$(cd "$(dirname "$dump")" && pwd)/$(basename "$dump")
id=orbit-drill-$$
work=$(mktemp -d)
log() { echo "restore-drill: $*"; }
fail() {
  log "FAILED: $*"
  exit 1
}
cleanup() {
  docker rm -fv "$id-db" >/dev/null 2>&1 || true
  docker network rm "$id" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

log "dump $dump ($(du -h "$dump" | cut -f1), written $(date -u -r "$dump" '+%Y-%m-%d %H:%MZ'))"
log "image $image ($(docker run --rm --network none "$image" version))"

docker network create --internal "$id" >/dev/null
pw=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
docker run -d --name "$id-db" --network "$id" -e POSTGRES_USER=orbit -e "POSTGRES_PASSWORD=$pw" -e POSTGRES_DB=orbit \
  -v "$dump:/drill/orbit.dump:ro" "$PG_IMAGE" >/dev/null
# Over TCP: the image's first-start init runs a temporary server on the unix
# socket only, which answers pg_isready and then restarts.
ready=
for _ in $(seq 60); do
  if docker exec "$id-db" pg_isready -h 127.0.0.1 -U orbit -d orbit >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ -n "$ready" ] || fail "postgres never became ready"
psql() { docker exec -i "$id-db" psql -U orbit -d orbit -v ON_ERROR_STOP=1 -qAt "$@"; }

# The dump's own row counts: each table's COPY block, one line per row
# (COPY escapes newlines inside values), ended by \.
docker exec "$id-db" pg_restore --data-only -f - /drill/orbit.dump |
  awk '/^COPY / { t = $2; sub(/^public\./, "", t); gsub(/"/, "", t); n = 0; copy = 1; next }
       copy && /^\\\.$/ { print t "\t" n; copy = 0; next }
       copy { n++ }' | sort >"$work/dump-counts.tsv"
[ -s "$work/dump-counts.tsv" ] || fail "the dump holds no table data"
log "dump holds $(wc -l <"$work/dump-counts.tsv" | tr -d ' ') tables, $(awk -F'\t' '{ s += $2 } END { print s }' "$work/dump-counts.tsv") rows"

docker exec "$id-db" pg_restore -U orbit -d orbit --no-owner --no-acl --exit-on-error /drill/orbit.dump ||
  fail "pg_restore"
log "restored"

: >"$work/restored-counts.tsv"
while IFS=$'\t' read -r t _; do
  printf '%s\t%s\n' "$t" "$(psql -c "SELECT count(*) FROM public.\"$t\"")" >>"$work/restored-counts.tsv"
done <"$work/dump-counts.tsv"
if ! diff -u "$work/dump-counts.tsv" "$work/restored-counts.tsv"; then
  fail "restored row counts differ from the dump's"
fi
log "every table's restored rows match the dump"

url="postgres://orbit:$pw@$id-db:5432/orbit?sslmode=disable"
run() { docker run --rm --network "$id" -e "ORBIT_DATABASE_URL=$url" "$@"; }
usage=$(docker run --rm --network none --entrypoint orbit "$image" 2>&1 || true)
if [[ $usage == *"| preflight "* ]]; then
  run "$image" preflight || fail "orbit preflight on the restored copy"
fi
run "$image" migrate || fail "orbit migrate on the restored copy"

psql -c "ALTER DATABASE orbit SET default_transaction_read_only = on"
chmod 0755 "$work" && chmod 0644 "$work/dump-counts.tsv"
run -v "$work/dump-counts.tsv:/drill/counts.tsv:ro" "$image" restore-check --expect /drill/counts.tsv ||
  fail "orbit restore-check"
log "ok"
