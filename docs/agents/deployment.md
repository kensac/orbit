# Deployment

orbit runs on hetzner, from `hetzner-vps-config/compose.orbit.yml`: the
`orbit` service and its own `orbit-db` (Postgres). Its configuration is
`hetzner-vps-config/orbit/`, mounted read-only at `/config`, so changing an
agent is a commit there, not here.

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
  value; rotated together). orbit's side reads it once build runs are wired.
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
