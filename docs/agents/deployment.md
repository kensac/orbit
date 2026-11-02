# Deployment

orbit's control instance runs on hetzner, from `hetzner-vps-config/compose.orbit.yml`: the `orbit` service (`orbit serve`) and its own `orbit-db` (Postgres). Its configuration is `hetzner-vps-config/orbit/`, mounted read-only at `/config`, so changing an agent is a commit there, not here. Workers (`orbit worker`) on other hosts read that configuration from the database (Worker fleet, below).

## Image

`build-push-ghcr.yml` pushes `ghcr.io/kensac/orbit:{sha-<commit>,latest}` on
every push to main, for amd64 and arm64. hetzner pins a digest; a release
reaches it when that pin moves.

## Rolling out a new image

Never recreate the container in place: traefik routes nothing until the new
one is healthy, and runs in progress are cut. Instead, two instances run
side by side for the switch (docs/ARCHITECTURE.md, Upgrades):

1. **Start the new container** beside the old one, same image config,
   environment, networks, `/config` mount and **the same `/data` volume**
   (sessions live there; a new instance on its own volume starts every
   interrupted or continued session fresh). Give it its own container name
   (compose: a second service or `--scale orbit=2 --no-recreate`).
2. **Wait until it is ready:** `docker inspect -f
   '{{.State.Health.Status}}'` is `healthy` (traefik's docker provider adds
   a container only then; the image probes every 2 s while starting) and
   `docker exec <new> orbit healthcheck --ready` exits 0. If it never gets
   there, remove it and stop: the old one is untouched.
3. **Drain the old one:** `docker exec <old> orbit drain`. It answers 503 on
   `/readyz` from then on, claims nothing, keeps its streams and API, and
   exits by itself once its runs finish.
4. **Wait for it to exit** (`docker wait <old>`), up to `ORBIT_DRAIN_MAX`
   plus a minute. Do not `docker stop` it: that sends SIGTERM (the same
   drain) but then SIGKILLs after `stop_grace_period`, and a killed run's
   spend since its last save is lost. If something must be cut short, a
   second `docker kill -s TERM <old>` hands its runs back cleanly first.
5. **Remove it** (`docker rm`).

traefik must route by readiness: the service's
`traefik.http.services.orbit.loadbalancer.healthcheck.path=/readyz` with a
short `interval` (2 s), so the draining instance leaves the pool within
seconds. Both containers carry the same router labels; nothing routes to
the internal listener. Set compose `stop_grace_period` to at least
`ORBIT_DRAIN_MAX` so a plain stop (a host reboot) also drains. Remove any
`ORBIT_DRAIN` from the environment; `ORBIT_DRAIN_MAX` (a Go duration)
overrides the drain's cap, which defaults to the longest agent timeout plus
5 minutes.

A request that reaches the old instance in the seconds before traefik drops
it is still served (the API is DB-backed); a run it enqueues is claimed by
the new one within its 5 s poll.

## Claiming it is deployed

Check the running container's revision, not the pin:
`docker exec orbit orbit version` must print the commit, `/healthz` and
`/readyz` must answer, and exactly one orbit container must be running once
the rollout is over. A config change is in force when the log says `config reloaded` and
`orbit_config_valid` is 1.

## Data volume

`ORBIT_DATA_DIR` (`/data`) holds run scratch and, for agents with `session`,
their Claude Code sessions, and every run's session while it runs (so an
interrupted run can be continued). It must be a named volume, owned by uid
10001, shared by the old and new container during a rollout; on the
container's own layer every restart starts those sessions fresh.

## Credentials

Rendered from Infisical (`HETZNER_ORBIT_*`). The Claude Code subscription
token comes from `claude setup-token` and lasts a year; when it expires every
run fails with `Not logged in`, which pages through the failure notification.

## Seeding memory from Claude Code

`orbit import claude-memory <dir> --scope <profile|topic:NAME|agent:NAME>` reads a Claude Code memory directory (`~/.claude/projects/<project>/memory`) and sends it to `POST /api/v1/memory/import`. Entries of the types in `--profile-types` (default `user`) go to the profile whatever `--scope` says. It is a dry run unless `--apply` is given: the server reports what it would create, update or leave alone. `--print` shows what was read from disk without calling a server. A file it cannot map (no frontmatter, an unknown type) is skipped with a warning. Repeating it is safe: it matches by key and changes only what changed.

The API is `--api` (default `ORBIT_API_URL`, else the internal listener) with `--token` (default `ORBIT_API_TOKEN`).

- **From the Mac**, against the public API with an authentik access token for a person in `ORBIT_OIDC_GROUP` (the entries are then authored by that person): `orbit import claude-memory <dir> --scope topic:homelab --api https://orbit.kanishksachdev.com --token <jwt>`, then again with `--apply`.
- **On hetzner**, with the operator token on the internal listener: copy the directory into the container and run it there, where `ORBIT_API_TOKEN` and the internal listener are the defaults:

  ```bash
  docker cp ~/memory orbit:/tmp/memory
  docker exec orbit orbit import claude-memory /tmp/memory --scope topic:homelab
  docker exec orbit orbit import claude-memory /tmp/memory --scope topic:homelab --apply
  docker exec orbit rm -r /tmp/memory
  ```

## sandboxd and egress on oci

The build sandbox runs on oci (tailnet `100.123.116.85`), not hetzner: a
container escape there reaches CI runners, not every secret in the fleet.
Two services from orbit's own image, in `oci-vps-config` as
`compose.orbit-sandbox.yml` (included from `docker-compose.yml`, whitelisted
in `.gitignore`):

```yaml
services:
  orbit-sandboxd:
    image: ghcr.io/kensac/orbit@sha256:<digest>
    container_name: orbit-sandboxd
    command: ["sandboxd"]
    # The docker socket is root:docker 0660, and holding it is root on the
    # host anyway; everything else about the container is locked down.
    user: "0:0"
    # Binds the tailnet address itself (sandboxd refuses every interface);
    # it joins no docker network, so no build container can reach it.
    network_mode: host
    read_only: true
    tmpfs: ["/tmp:size=600m"]   # the readback spool (256 MiB cap)
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    # The image's HEALTHCHECK probes orbit serve's /healthz.
    healthcheck: {disable: true}
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    environment:
      - ORBIT_SANDBOX_TOKEN=${ORBIT_SANDBOX_TOKEN}
      - ORBIT_SANDBOX_LISTEN=100.123.116.85:8091
      - ORBIT_SANDBOX_IMAGE=ghcr.io/kensac/orbit-build@sha256:<digest>
      - ORBIT_SANDBOX_NETWORK=orbit-sandbox
      - ORBIT_SANDBOX_PROXY=orbit-egress:3128
    depends_on: [orbit-egress]

  orbit-egress:
    image: ghcr.io/kensac/orbit@sha256:<digest>
    container_name: orbit-egress
    command: ["egress"]
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    healthcheck: {disable: true}
    restart: unless-stopped
    environment:
      - ORBIT_EGRESS_ALLOW=api.anthropic.com,proxy.golang.org,sum.golang.org,registry.npmjs.org
      - ORBIT_EGRESS_LISTEN=:3128
    networks: [orbit-sandbox, orbit-egress]

networks:
  # Build containers' network: internal, so no route out. sandboxd refuses
  # to start if it is not.
  orbit-sandbox:
    name: orbit-sandbox
    internal: true
  # The proxy's way out; nothing else joins it.
  orbit-egress:
    name: orbit-egress
```

- **Token:** `ORBIT_SANDBOX_TOKEN`, at least 32 characters, in Infisical as
  `OCI_ORBIT_SANDBOX_TOKEN` and `HETZNER_ORBIT_SANDBOX_TOKEN` (the same
  value; rotated together). orbit reads it as `build.token_env`.
- **Images:** both pinned by digest; `ghcr.io/kensac/orbit-build` is pushed
  by `build-push-ghcr.yml` beside the orbit image. orbit's image is private,
  so oci logs in to ghcr on deploy as hetzner does (`ghcr_username` in its
  host_vars, `OCI_GHCR_TOKEN`). sandboxd pulls the build image through the
  Engine API without credentials: make the `orbit-build` package public
  (it is built from this public repository and holds nothing private), or
  `docker pull` it on oci before sandboxd starts, since sandboxd uses an
  image already present.
- **Claiming it is deployed:** `docker logs orbit-sandboxd` shows
  `sandboxd: serving` (it exits on a failed host check: no seccomp, a
  network that is not internal, an image it cannot get), `docker logs
  orbit-egress` shows `egress: serving` with the allowlist, and from
  hetzner `curl -s -o /dev/null -w '%{http_code}' -X DELETE
  http://100.123.116.85:8091/v1/sandboxes/$(printf 'a%.0s' {1..64})`
  answers 401 without the token and 404 with it.

## Building tickets: what hetzner and oci need

In order; each step is safe to take alone (without step 4 approved tickets
just wait).

1. **oci**: sandboxd and egress as above, at the image digest that carries
   this build runner, `ghcr.io/kensac/orbit-build` present (public package
   or pulled), and `OCI_ORBIT_SANDBOX_TOKEN` in Infisical. Check with the
   `curl` above.
2. **Infisical**: `HETZNER_ORBIT_SANDBOX_TOKEN` with the same value
   (`playbooks/secret-set.yml -e key=HETZNER_ORBIT_SANDBOX_TOKEN`), so it
   renders into `/root/.env` as `ORBIT_SANDBOX_TOKEN`.
3. **hetzner, `compose.orbit.yml`**: the `orbit` service's environment
   gains `- ORBIT_SANDBOX_TOKEN=${ORBIT_SANDBOX_TOKEN}`. orbit reaches
   `100.123.116.85:8091` over the tailnet, which the container already has
   (it reaches Infisical and GitHub the same way). `ORBIT_DATA_DIR` holds
   each build's clone under `builds/` while it runs; it is removed after.
   The image carries `git` for this (deploy/Dockerfile); check with
   `docker exec orbit git --version`.
4. **hetzner, `orbit/orbit.yaml`**: the `build:` section
   (docs/tickets.md, Configuration) with `sandboxd: 100.123.116.85:8091`
   and `agent:` an agent whose spec has `timeout:` at most 50m and
   `actions: {github:merge: approve}`. The config reloads on its own; check
   `config reloaded` in the log and `orbit_config_valid` 1.
5. **GitHub**: `main` of `kensac/orbit` protected (a PR and the `check`
   status required, the App off the bypass list), as for `github:merge`.

**Claiming it works:** file a small request ticket, approve its plan, and
watch the ticket go `building` (the build run's stream shows
`orbit.build` steps and `orbit.permission` decisions), `review` (a PR from
`orbit-kensac[bot]` on `orbit/<8 hex>`), then `merging` with an approval
notification once CI is green. After the merge and the deploy that moves
hetzner's pin, the ticket is `done` when `docker exec orbit orbit version`
is the merge commit or a descendant. Until that has happened once, a real
build in a real sandbox is unverified.

## Worker fleet

Design: docs/ARCHITECTURE.md, Worker fleet. One control instance (hetzner) and workers on oci and transmission, all on the same image digest (a worker on a newer image than control waits for control to migrate).

### Environment per role

| variable | `orbit serve` (control) | `orbit worker` |
|---|---|---|
| `ORBIT_DATABASE_URL` | `...@orbit-db:5432/orbit?sslmode=disable` (docker network) | `...@100.79.4.41:5434/orbit?sslmode=require` (tailnet, TLS) |
| `ORBIT_WORKER_NAME` | `hetzner` | the host's name |
| `ORBIT_CAPABILITIES` | what hetzner has (unset is `*`, every agent) | what this host has (unset is none: agents that need nothing) |
| `ORBIT_WORKERS` | executors here (0 for control only) | executors here (default 2) |
| `ORBIT_DATA_DIR` | `/data`, a named volume | `/data`, a named volume of its own |
| `CLAUDE_CODE_OAUTH_TOKEN` | yes | yes: the backend credential, the only one the model sees |
| source credentials | every agent's | only those of agents its capabilities cover |
| `ORBIT_API_TOKEN`, `ORBIT_OIDC_*` | yes | no (no API) |
| channel credentials (`ORBIT_NTFY_TOPIC`, `SMTP_*`) | yes | no (control delivers) |
| `ORBIT_GITHUB_APP_PRIVATE_KEY`, `ORBIT_SANDBOX_TOKEN` | yes | no (builds and actions are control's) |
| `ORBIT_LISTEN` | public listener | `/healthz`, `/readyz`, `/metrics` only; publish it on the tailnet address |

A worker logs `agents this worker may claim reference unset variables` with the names when a covered agent's source needs a variable it lacks; such a run fails on that worker with the source's error.

### Capabilities, per host

| capability | means | hosts |
|---|---|---|
| `docker-network:hetzner` | can reach containers on hetzner's `proxy` network by name | hetzner |
| `sure` | holds `ORBIT_SURE_API_KEY` (Sure's API) | hetzner |
| `sandbox` | holds `ORBIT_SANDBOX_TOKEN` and reaches sandboxd | hetzner |
| `browser` | has a browser for an agent to drive | none yet (the image has none) |

So today: hetzner `ORBIT_CAPABILITIES=docker-network:hetzner,sure,sandbox`; oci and transmission none (they run agents that need nothing). The finance agent needs `[docker-network:hetzner, sure]` and stays on hetzner. A new capability is a name agreed here, a host advertising it, and the credentials it implies on exactly those hosts.

### orbit-db on the tailnet, with TLS

The postgres alpine image has no openssl, so a one-shot service from orbit's image writes a self-signed certificate once into a volume (`orbit tls-cert`, kept on later runs), and `orbit-db` serves TLS with it and publishes on hetzner's tailnet address only. 5432 is jobtracker-db's and 5433 housing-watch-db's, so orbit-db takes 5434. Workers connect with `sslmode=require` (encrypted, certificate not verified; the tailnet authenticates the host), as jobtracker's do. In `compose.orbit.yml`:

```yaml
  orbit-db-tls:
    image: ghcr.io/kensac/orbit@sha256:<digest>   # the orbit service's pin
    container_name: orbit-db-tls
    command: ["tls-cert", "--dir", "/tls", "--owner", "70:70", "--name", "orbit-db"]
    user: "0:0"            # to chown the key to postgres (uid 70 in alpine)
    healthcheck: {disable: true}
    restart: "no"
    volumes:
      - orbit-db-tls:/tls

  orbit-db:
    # ... as now, plus:
    command: ["postgres", "-c", "ssl=on",
              "-c", "ssl_cert_file=/tls/server.crt", "-c", "ssl_key_file=/tls/server.key"]
    depends_on:
      orbit-db-tls:
        condition: service_completed_successfully
    ports:
      # Tailnet only: orbit workers (5432 jobtracker-db, 5433 housing-watch-db).
      # hetzner has no firewall, so never a bare mapping.
      - "100.79.4.41:5434:5432"
    volumes:
      - orbit-db-data:/var/lib/postgresql
      - orbit-db-tls:/tls:ro

volumes:
  orbit-db-tls:
```

The control `orbit` service keeps `sslmode=disable` over the docker network and gains `ORBIT_CAPABILITIES=docker-network:hetzner,sure,sandbox`. Check: from oci, `psql "postgres://orbit:...@100.79.4.41:5434/orbit?sslmode=require" -c 'select ssl from pg_stat_ssl where pid = pg_backend_pid()'` is `t`; `ss -lntp | grep 5434` on hetzner shows only `100.79.4.41`.

### Worker compose (oci, transmission)

The same service on both, in `compose.orbit-worker.yml` (oci: included from `docker-compose.yml`; transmission: under `transmission/`), whitelisted in `.gitignore`:

```yaml
services:
  orbit-worker:
    image: ghcr.io/kensac/orbit@sha256:<digest>   # the same digest as hetzner's orbit
    container_name: orbit-worker
    command: ["worker"]
    restart: unless-stopped
    # A drain lets the runs held here finish (each under its agent's
    # timeout); a plain stop must not SIGKILL them first.
    stop_grace_period: 20m
    environment:
      - ORBIT_DATABASE_URL=postgres://orbit:${ORBIT_DB_PASSWORD}@100.79.4.41:5434/orbit?sslmode=require
      - ORBIT_WORKER_NAME=oci            # transmission: transmission
      - ORBIT_CAPABILITIES=
      - ORBIT_WORKERS=2
      - CLAUDE_CODE_OAUTH_TOKEN=${ORBIT_CLAUDE_CODE_OAUTH_TOKEN}
      - TZ=America/New_York
    ports:
      # /healthz, /readyz, /metrics (ORBIT_LISTEN :8080). Tailnet only:
      # oci 100.123.116.85, transmission 100.109.24.102.
      - "100.123.116.85:9095:8080"
    volumes:
      - orbit-worker-data:/data

volumes:
  orbit-worker-data:
```

- transmission's worker does not join gluetun (`network_mode: service:gluetun`): model traffic and the database go out directly.
- The image's HEALTHCHECK runs `orbit healthcheck`, which reads `ORBIT_LISTEN`, so it works unchanged.
- Prometheus: add `100.123.116.85:9095` and `100.109.24.102:9095` to orbit's scrape job (metrics are per instance).
- `/data` holds the worker's sessions and scratch; a lost volume costs nothing (sessions come back from the database).
- Draining a worker: `docker stop` (SIGTERM drains, within `stop_grace_period`); `docker kill -s TERM` twice hands runs back at once. There is no `orbit drain` on a worker (no internal listener).

### Infisical

| key | value |
|---|---|
| `OCI_ORBIT_DB_PASSWORD`, `TRANSMISSION_ORBIT_DB_PASSWORD` | copies of `HETZNER_ORBIT_DB_PASSWORD` (rotate together) |
| `OCI_ORBIT_CLAUDE_CODE_OAUTH_TOKEN`, `TRANSMISSION_ORBIT_CLAUDE_CODE_OAUTH_TOKEN` | copies of `HETZNER_ORBIT_CLAUDE_CODE_OAUTH_TOKEN` (one subscription; rotate together) |
| `TRANSMISSION_GHCR_TOKEN` | a copy of `HETZNER_GHCR_TOKEN`, plus `ghcr_username: kensac` in transmission's host_vars (the image is private; oci already logs in) |

No worker gets `ORBIT_API_TOKEN`, a channel credential, the GitHub App key, the sandbox token, or `ORBIT_SURE_API_KEY`.

### Rolling out

**Before the first worker starts**, every agent that can run only on hetzner says so: in `hetzner-vps-config/orbit/agents/`, finance gets `needs: [docker-network:hetzner, sure]` (its sources are `http://plaid:3000` on the proxy network and `ORBIT_SURE_API_KEY`). An agent without `needs` is claimed by any worker. Then hetzner's `orbit` gains its `ORBIT_CAPABILITIES`, then orbit-db's TLS and port, then the workers.

All hosts pin one digest; Renovate's PRs for the orbit image land in hetzner-vps-config, oci-vps-config and transmission-vps-config together (lockstep, as jobtracker's). Order: control first (it migrates; workers on the new image wait for that with `worker waiting`), then workers. A worker on the old image keeps running against the new schema (migrations are additive).

**Claiming it is deployed:** `docker logs orbit-worker` shows `orbit worker` with its capabilities, `docker exec orbit-worker orbit version` is the commit, and a run of a needs-free agent has `worker` `oci/0` (its `run_attempts` row) with its report delivered by control (a `deliveries` row, its `outbox` row `sent_at` set).
