#!/usr/bin/env bash
# compat.sh BASE: the overlap window of a rollout, with binaries. The
# previous release (BASE, a commit) migrates a new database and queues a
# run; this tree's binary preflights and then migrates it; the previous
# binary then serves on the new schema: it becomes ready, claims and
# finishes the queued run, queues and finishes another, answers the API,
# and drains on SIGTERM. A migration that breaks the image still running
# during a deploy fails here, before it can merge. Needs Go, psql and a
# Postgres server (ORBIT_TEST_DATABASE_URL, as for the tests); CI runs it
# in the check job (make compat).
set -euo pipefail

base=${1:?usage: deploy/compat.sh BASE}
server=${ORBIT_TEST_DATABASE_URL:?ORBIT_TEST_DATABASE_URL names the Postgres server to use}
agent=${COMPAT_AGENT:-finance}
port=$((30000 + RANDOM % 20000))

work=$(mktemp -d)
db=orbit_test_compat_$$
log() { echo "compat: $*"; }
pid=
fail() {
  log "FAILED: $*"
  [ -f "$work/old.log" ] && tail -n 40 "$work/old.log"
  exit 1
}
cleanup() {
  [ -n "$pid" ] && kill -KILL "$pid" 2>/dev/null
  psql -q "$server" -c "DROP DATABASE IF EXISTS $db WITH (FORCE)" >/dev/null 2>&1 || true
  git worktree remove --force "$work/base" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

git worktree add --detach -q "$work/base" "$base"
log "previous: $(git -C "$work/base" log -1 --format='%h %s')"
(cd "$work/base" && CGO_ENABLED=0 go build -o "$work/old" ./cmd/orbit)
CGO_ENABLED=0 go build -o "$work/new" ./cmd/orbit

psql -q "$server" -c "CREATE DATABASE $db" >/dev/null
url=$(python3 -c 'import sys, urllib.parse as u; p = u.urlsplit(sys.argv[1]); print(u.urlunsplit(p._replace(path="/" + sys.argv[2])))' "$server" "$db")
export ORBIT_DATABASE_URL=$url

# The previous release's schema, with a run waiting in it.
ORBIT_CONFIG_DIR="$work/base/examples" "$work/old" migrate
first=$(ORBIT_CONFIG_DIR="$work/base/examples" "$work/old" run "$agent" compat-before)

"$work/new" preflight --config examples || fail "orbit preflight against the previous release's schema"
"$work/new" migrate || fail "orbit migrate over the previous release's schema"
log "migrated forward"

# The previous binary on the new schema, as during a rollout. Every
# variable its examples reference gets a dummy: nothing it reaches exists.
vars=$("$work/old" validate "$work/base/examples" | sed -n 's/^environment referenced: //p' | tr -d ',')
envs=()
for v in $vars; do
  # Only variable names: an entry can read "A or B".
  [[ $v =~ ^[A-Z_][A-Z0-9_]*$ ]] && envs+=("$v=compat")
done
token=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
mkdir -p "$work/data"
env "${envs[@]}" ORBIT_API_TOKEN="$token" ORBIT_CONFIG_DIR="$work/base/examples" ORBIT_DATA_DIR="$work/data" \
  ORBIT_LISTEN="127.0.0.1:$port" ORBIT_INTERNAL_LISTEN="127.0.0.1:$((port + 1))" ORBIT_WORKERS=1 ORBIT_WORKER_NAME=compat \
  "$work/old" serve >"$work/old.log" 2>&1 &
pid=$!

ready=
for _ in $(seq 60); do
  kill -0 "$pid" 2>/dev/null || fail "the previous binary exited on the new schema"
  if ORBIT_LISTEN="127.0.0.1:$port" "$work/old" healthcheck --ready >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ -n "$ready" ] || fail "the previous binary never became ready on the new schema"
log "previous binary ready on the new schema"

api() { curl -fsS -H "Authorization: Bearer $token" "http://127.0.0.1:$((port + 1))/api/v1/$1"; }
# A run ends in any final state: the examples' sources and backends do
# not exist, so it fails, through the same claim, attempt, event and
# finish writes as a real one.
finished() {
  local st=
  for _ in $(seq 60); do
    st=$(api "runs/$1" 2>/dev/null | python3 -c 'import json, sys; d = json.load(sys.stdin); print((d.get("run") or d).get("status", ""))' 2>/dev/null || true)
    case $st in failed | succeeded | skipped | cancelled) log "run $1 $st"; return 0 ;; esac
    sleep 1
  done
  fail "run $1 never finished on the new schema (last status '$st')"
}
finished "$first"
second=$(ORBIT_CONFIG_DIR="$work/base/examples" "$work/old" run "$agent" compat-after) || fail "the previous binary could not queue a run on the new schema"
finished "$second"
api agents >/dev/null || fail "the previous binary's API on the new schema"

kill -TERM "$pid"
for _ in $(seq 60); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
kill -0 "$pid" 2>/dev/null && fail "the previous binary did not drain and exit"
wait "$pid" || fail "the previous binary exited non-zero after its drain"
pid=
if grep -q '"level":"ERROR"' "$work/old.log"; then
  # The examples' GitHub key is a dummy, so the ticket loop's error is expected; anything else is not.
  if grep '"level":"ERROR"' "$work/old.log" | grep -v '"component":"tickets"' | grep -q .; then
    grep '"level":"ERROR"' "$work/old.log" | grep -v '"component":"tickets"' | head -n 20
    fail "the previous binary logged errors on the new schema"
  fi
fi
log "ok: $(git -C "$work/base" rev-parse --short HEAD) keeps working on this tree's schema"
