# Deployment

orbit runs on hetzner, from `hetzner-vps-config/compose.orbit.yml`: the
`orbit` service and its own `orbit-db` (Postgres). Its configuration is
`hetzner-vps-config/orbit/`, mounted read-only at `/config`, so changing an
agent is a commit there, not here.

## Image

`build-push-ghcr.yml` pushes `ghcr.io/kensac/orbit:{sha-<commit>,latest}` on
every push to main, for amd64 and arm64. hetzner pins a digest; a release
reaches it when that pin moves.

## Claiming it is deployed

Check the running container's revision, not the pin:
`docker exec orbit orbit version` must print the commit, and `/healthz` must
answer. A config change is in force when the log says `config reloaded` and
`orbit_config_valid` is 1.

## Data volume

`ORBIT_DATA_DIR` (`/data`) holds run scratch and, for agents with `session`,
their Claude Code sessions. It must be a named volume, owned by uid 10001, for
sessions to survive a container restart; on the container's own layer every
restart starts those agents fresh.

## Credentials

Rendered from Infisical (`HETZNER_ORBIT_*`). The Claude Code subscription
token comes from `claude setup-token` and lasts a year; when it expires every
run fails with `Not logged in`, which pages through the failure notification.
