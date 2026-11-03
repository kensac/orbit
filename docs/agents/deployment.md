# Deployment

orbit's control instance runs on hetzner, from `hetzner-vps-config/compose.orbit.yml`: the `orbit` service (`orbit serve`) and its own `orbit-db` (Postgres). Its configuration is `hetzner-vps-config/orbit/`, mounted read-only at `/config`, so changing an agent is a commit there, not here. Workers (`orbit worker`) on other hosts read that configuration from the database (Worker fleet, below).

## Image

`build-push-ghcr.yml` publishes `ghcr.io/kensac/orbit:{sha-<commit>,latest}` and `ghcr.io/kensac/orbit-build:{sha-<commit>,latest}`, for amd64 and arm64, only for a main commit whose CI run succeeded (`workflow_run`, every job in ci.yml). It pushes each image by digest with no tag, smokes it natively on both architectures, and only then tags it, so nothing can pin an image that failed:

- `deploy/smoke.sh IMAGE [REVISION]`: a throwaway Postgres, `orbit migrate` on the empty database, `orbit validate` on `examples/`, `orbit serve` (no executors, a dummy for every variable the examples reference) until docker says `healthy` and `orbit healthcheck --ready` passes, and `orbit version` is the commit. CI runs it on every pull request's image too.
- `deploy/build-smoke.sh IMAGE`: the build image's tools as uid 10001 on a read-only root, as sandboxd runs it.
- `.github/scripts/publish-tags.sh`: points `sha-<short>` and then `latest` at exactly the smoked digest (it checks), and never moves `latest` back to an older commit when CI runs finish out of order. Each image's `org.opencontainers.image.revision` label is its commit.

A failed CI run or smoke publishes nothing; to publish that commit after a flake, re-run its CI run or the failed publish run. hetzner pins a digest; a release reaches it when that pin moves.

### The workstation image

`ghcr.io/kensac/orbit-workstation:{sha-<commit>,latest}` (`deploy/workstation.Dockerfile`, workflow `workstation.yml`) is an agent's persistent box: a container sandboxd keeps per agent under gVisor (`runsc`), where a person gets Claude Code in a browser terminal as on a laptop and the agent's own turns run beside it. It runs on hetzner (amd64) and oci (arm64), so it is built for both; a pull request that touches it builds it natively on each architecture and runs every tool as its user (`deploy/workstation-smoke.sh`). Publishing is gated the same way as orbit's: after a main commit's CI run succeeds, and only when its inputs (the Dockerfile, `deploy/workstation/`, `deploy/cli/`, the smoke and the workflow) changed since the commit `latest` was built from, it is pushed by digest, smoked natively on both architectures, and only then tagged. Like the build image it is pinned by digest where it runs, so a push reaches a box only when that pin moves.

- **Tools:** Debian trixie with git, jq, ripgrep, fd, fzf, curl, wget, make, build-essential, tmux, neovim, nano, sqlite3, psql, ssh, less, procps, iproute2, unzip and sudo; Go (go.mod's toolchain), Node 24 with npm and pnpm (corepack, `deploy/workstation/package.json`), python3 with uv, yq, gh, and Docker (engine, CLI, containerd, buildx, compose) for a dockerd inside the box; Claude Code at deploy/cli's lockfile with its auto-updater off.
- **User:** `agent`, uid 10001 (sandboxd's uid), `HOME=/home/agent`, passwordless sudo (which needs a container without `no-new-privileges`), in the `docker` group. `/work` is its own. Locale `en_US.UTF-8`, `TERM=xterm-256color`; `/etc/tmux.conf` sets `set-clipboard on`, `window-size latest`, `mouse on` and 10,000 lines of history.
- **GitHub:** the image holds no credential. When orbit writes a token to `/run/orbit/gh-token`, the `gh` wrapper (`/usr/local/bin/gh`) exports it as `GH_TOKEN` and the system git credential helper (`git-credential-orbit`) answers `https://github.com` with it; without the file both step aside, so `gh auth login` and `gh auth setup-git` work as usual.
- **Pins:** images by digest, yq by each architecture's sha256 (renovate.json's custom manager recomputes it for a new release), pnpm by its `packageManager` hash, the GitHub CLI and Docker apt keys by sha256. The apt packages themselves are whatever Debian's, GitHub's and Docker's repositories serve at build time.

## Rolling out a new image

Never recreate the container in place: traefik routes nothing until the new
one is healthy, and runs in progress are cut. Instead, two instances run
side by side for the switch (docs/ARCHITECTURE.md, Upgrades):

0. **Preflight the new image** against the live database, before anything starts: `orbit preflight --config /config` in a one-off container of the new image with the service's environment and networks (compose: `docker compose run --rm --no-deps -l traefik.enable=false --entrypoint orbit orbit preflight --config /config`). It exits non-zero, and the rollout stops with the old instance untouched, when a pending migration is not expand-only, fails on the live schema or rows (a dry run, rolled back), or the configuration does not load (docs/agents/store.md, Migrations). An image without the command (before it existed) is rolled out without it.
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

Routers: `/api/`, `/mcp` with the OAuth paths, and `/hooks/` (webhook deliveries, signed, docs/api.md) go past forward-auth, each with a priority above the host's catch-all; everything else (`/ui/`) is behind it. A webhook secret in orbit.yaml is in force only once its variable is in orbit's environment (a missing one answers `503`).

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

It also holds `mirrors/`, a bare mirror of each repository agents read (`repos`), and `checkouts/`, the builds' kept clones. Both are caches: a lost one is fetched again on the next run that needs it. The two instances of a rollout share them; a fetch takes a lock file beside its mirror.

## Restore drill

`deploy/restore-drill.sh DUMP IMAGE` proves a nightly orbit-db dump restores and that orbit can read it, without touching anything live. On hetzner, as root, with the dump `db-dump.timer` writes and the running image:

```bash
rev=$(docker exec orbit orbit version)
curl -fsSL "https://raw.githubusercontent.com/kensac/orbit/$rev/deploy/restore-drill.sh" -o /tmp/restore-drill.sh
bash /tmp/restore-drill.sh /root/db-dumps/orbit-db.dump "$(docker inspect -f '{{.Config.Image}}' orbit)"
```

It starts a throwaway Postgres (the pinned 18-alpine) on an internal docker network, so nothing in the drill can reach anything, and:

1. counts every table's rows in the dump itself (each `COPY` block of `pg_restore --data-only`);
2. restores with `pg_restore --exit-on-error --no-owner --no-acl`, and requires every table's restored count to equal the dump's;
3. runs `orbit preflight` (when the image has it: the next rollout's migrations, dry-run on real rows) and `orbit migrate` on the copy;
4. sets the database `default_transaction_read_only` and runs `orbit restore-check --expect <the dump's counts>`, which reads it on a read-only connection through the store's own queries (agents, recent runs with their attempts, events, artifacts and deliveries, conversations, messages, memory entries, skills, tickets, actions, attention) and fails if the schema is behind the image, a table is missing, or one holds fewer rows than the dump (migrations only add);
5. removes the database, its volume and the network, pass or fail.

The last line is `restore-drill: ok`; any failure names its step. It needs disk for one more copy of the database while it runs. Run it after a Postgres or orbit-db change, and as a periodic check that the dumps are usable.

## Credentials

Rendered from Infisical (`HETZNER_ORBIT_*`). The Claude Code subscription
token comes from `claude setup-token` and lasts a year; when it expires every
run fails with `Not logged in`, which pages through the failure notification.
Record the day it lapses in orbit.yaml's `expires:` (configuration.md, Secrets) whenever it is set, so `orbit_credential_expiry_timestamp_seconds` warns two weeks ahead (Alerts, below).

Web Push needs `ORBIT_VAPID_PUBLIC_KEY`, `ORBIT_VAPID_PRIVATE_KEY` (from
`orbit vapid`) and `ORBIT_VAPID_SUBJECT` on control (docs/api.md, Attention
and push); without them push is off and `serve` logs so.

## Seeding memory from Claude Code

`orbit import claude-memory <dir> --scope <profile|topic:NAME|agent:NAME>` reads a Claude Code memory directory (`~/.claude/projects/<project>/memory`) and sends it to `POST /api/v1/memory/import`. Entries of the types in `--profile-types` (default `user`) go to the profile whatever `--scope` says. It is a dry run unless `--apply` is given: the server reports what it would create, update or leave alone. `--print` shows what was read from disk without calling a server. A file it cannot map (no frontmatter, an unknown type) is skipped with a warning; an operating system's droppings (macOS AppleDouble `._*` files, `.DS_Store`, `__MACOSX`, `Thumbs.db` and the like: `claudeimport.Junk`) are left out silently. Repeating it is safe: it matches by key and changes only what changed.

The API is `--api` (default `ORBIT_API_URL`, else the internal listener) with `--token` (default `ORBIT_API_TOKEN`).

- **From the Mac**, against the public API with an authentik access token for a person in `ORBIT_OIDC_GROUP` (the entries are then authored by that person): `orbit import claude-memory <dir> --scope topic:homelab --api https://orbit.kanishksachdev.com --token <jwt>`, then again with `--apply`.
- **On hetzner**, with the operator token on the internal listener: copy the directory into the container and run it there, where `ORBIT_API_TOKEN` and the internal listener are the defaults:

  ```bash
  docker cp ~/memory orbit:/tmp/memory
  docker exec orbit orbit import claude-memory /tmp/memory --scope topic:homelab
  docker exec orbit orbit import claude-memory /tmp/memory --scope topic:homelab --apply
  docker exec orbit rm -r /tmp/memory
  ```

## Seeding skills from Claude Code

`orbit import skills <dir>` reads each directory under `dir` that holds a `SKILL.md` (`~/.claude/skills`, a project's `.claude/skills`) with every file beside it, and sends them to `POST /api/v1/skills/import`: a dry run unless `--apply`, `--only a,b` to pick some, `--attach <agent>` to attach them (with `--apply`), `--print` to list what was read without a server. A directory without a `SKILL.md` (a plugin, a sync bucket) or with a symlink is skipped with a warning. Operating system droppings are never a skill's files, so a directory unpacked from a tar made on macOS imports cleanly: they are left out as for memory, and importing again over a skill stored with them writes a version without them. The API and token are as for memory above; on hetzner, `docker cp` the directory into the container and run it there.

## Importing Claude Code sessions

`orbit import claude-sessions <dir>` reads the session transcripts under `dir` (Claude Code's projects directory, `~/.claude/projects`, or one project's directory under it: every top-level `<session id>.jsonl`; subagent transcripts under `<id>/` are not sessions of their own) and sends what orbit does not hold yet to `POST /api/v1/sessions/import` (docs/api.md, Imported sessions). Each line is normalized on the Mac before it is sent (credentials scrubbed, an oversized line cut to its skeleton) and again by the server. It is incremental: it reads every session's `offset` and `prefix_sha256` from `GET /api/v1/sessions` first and sends only the complete lines after that offset when the file still starts with those bytes, the whole file as a new generation when it does not, and nothing for a session that has not grown. Without `--apply` it prints that plan and sends no line; `--print` lists what was read from disk without calling a server. A line that is not a JSON object is skipped with a warning. The API and token are as for memory above; run it from the Mac against the public API, since the transcripts are there.

## Syncing back to Claude Code

`orbit sync claude` is the reverse of the imports: it writes orbit's skills into `--skills-dir` (default `~/.claude/skills`, one directory per skill, `SKILL.md` and its files) and, with `--memory-dir` (a project's `~/.claude/projects/<project>/memory`), the memory entries of `--scope` (a comma list, default `profile`; `topic:<name>`, `agent:<name>`) as Claude Code memory files (`<key>.md`, or the file that already holds that key: frontmatter `name` (the key), `description`, `metadata.type`, then the body and its `**Why:**` and `**How to apply:**` paragraphs, which `orbit import claude-memory` reads back into the same entry) with a line each in `MEMORY.md` (`- [Title](file.md) - description`; every other line is kept). `--only a,b` limits the skills; `--skills-dir ''` or no `--memory-dir` skips either.

It never overwrites a person's edit: a local copy is written over only when it is exactly some version orbit holds (a skill by its digest over every file, a memory file by its fields), and otherwise reported as `changed locally` and left alone. An active skill or entry is created or brought to its current version; a retired one's local copy is removed when it is one of its versions. Every run plans first and prints the plan and its digest (`plan <sha256>`) and writes nothing; `--apply <digest>` writes only if the plan computed again is that one, and each write checks the local copy is still what the plan saw. A skill is written beside its directory and swapped in, a file beside itself and renamed over. The API and token are as for the imports above.

## sandboxd and egress on hetzner

The build sandbox runs on hetzner today, beside orbit, in `hetzner-vps-config` as `compose.orbit-sandbox.yml` (included from `docker-compose.yml`): services from orbit's own image, all pinned by digest. Workstation sandboxes are planned for both hetzner and oci. A container escape here reaches every secret on hetzner (docs/agents/security.md).

- **orbit-sandboxd** runs as uid 10001 with the docker group (`group_add`) for the socket, read-only, no capabilities, `no-new-privileges`, and listens on `10.250.250.2:8091` on `orbit-sandboxd-api`, an internal network only orbit shares, so it has no host port (hetzner has no firewall). `ORBIT_SANDBOX_IMAGE` pins `ghcr.io/kensac/orbit-build` by digest, `ORBIT_SANDBOX_NETWORK=orbit-sandbox`, `ORBIT_SANDBOX_PROXY=orbit-egress:3128`.
- **orbit-egress** joins `orbit-sandbox` (internal: the build containers' only way out is this proxy) and `orbit-egress`, with `ORBIT_EGRESS_ALLOW=proxy.golang.org,sum.golang.org,registry.npmjs.org`. `api.anthropic.com` is not on it: the CLI reaches its backend through orbit-credproxy, and an open route there would carry data out under any key a model brings.
- **orbit-credproxy** (`orbit credproxy`, the same image and hardening as egress: uid 10001, read-only, no capabilities, `no-new-privileges`) joins `orbit-sandbox` and `orbit-workstation`, with the alias `orbit-credproxy` on both, and `orbit-egress` for its own route out, with `ORBIT_CREDPROXY_KEY` and `ORBIT_CREDPROXY_LISTEN=:3129`. It holds no credential: each sandbox's token carries one, sealed. No host port.
- **The credential proxy key:** `ORBIT_CREDPROXY_KEY`, at least 32 characters, `HETZNER_ORBIT_CREDPROXY_KEY` in Infisical, read by orbit-credproxy and orbit-sandboxd and nothing else. sandboxd also takes `ORBIT_SANDBOX_CREDPROXY` (default `orbit-credproxy:3129`) and `ORBIT_SANDBOX_WORKSTATION_CREDPROXY` (the proxy's address on the workstation profile's network when it is addressed by IP; default the same), and refuses to start without the key: there is no configuration in which a sandbox holds the backend credential. Rotating the key invalidates every live sandbox's token (their runs fail with `Failed to authenticate. API Error: 401`).
- **Token:** `ORBIT_SANDBOX_TOKEN`, at least 32 characters, `HETZNER_ORBIT_SANDBOX_TOKEN` in Infisical, read by both sandboxd and orbit (`build.token_env`).
- **Images:** `ghcr.io/kensac/orbit-build` is pushed by `build-push-ghcr.yml` beside the orbit image. sandboxd pulls it through the Engine API without credentials, so the package must be public (it is built from this public repository and holds nothing private) or pulled on the host before sandboxd starts.
- **Health:** sandboxd answers `GET /healthz` (up) and `GET /readyz` without the token: 200 or 503 with `{"ready", "checks"}`, where `checks` names `docker` (the daemon answers), `network` and, when set, `workstation_network` (still internal), `proxy:<host>` for `ORBIT_SANDBOX_PROXY` and `ORBIT_SANDBOX_WORKSTATION_PROXY`, and `credproxy:<host>` for `ORBIT_SANDBOX_CREDPROXY` and `ORBIT_SANDBOX_WORKSTATION_CREDPROXY` (the container named as the host is running, and healthy when it has a healthcheck; a host given as an IP is reported `unchecked`), each `ok` or the failure; answers are cached for 10 s. egress answers an origin-form `GET /healthz` on its listener (a proxied request is never answered by it). Neither image healthcheck fits them (it probes `ORBIT_LISTEN`), so compose gives each `test: ["CMD", "orbit", "healthcheck", "--url", "<its /healthz>"]`. orbit control asks the configured sandboxd's `/readyz` every 30 s (Alerts, below).
- **Claiming it is deployed:** `docker logs orbit-sandboxd` shows `sandboxd: serving` (it exits on a failed host check: no seccomp, a network that is not internal, an image it cannot get), `docker logs orbit-egress` shows `egress: serving` with the allowlist, and `docker logs orbit-credproxy` shows `credproxy: serving` and, once a run has used it, `credproxy: forwarded` lines with the run's id and `status=200`.

**Workstation profile** (sandboxed chats, `sandbox: workstation`, and agents' boxes): its own internal network, `orbit-workstation`, and a second `orbit egress` with `ORBIT_EGRESS_PROFILE=workstation` (the open web: any public host on any port but SMTP's, plain HTTP too; docs/agents/security.md, Workstation egress) on the `orbit-workstation` and `orbit-egress` networks, never on `orbit-sandbox`, named for example `orbit-egress-open`; sandboxd's environment gains `ORBIT_SANDBOX_WORKSTATION_PROXY=orbit-egress-open:3128` and `ORBIT_SANDBOX_WORKSTATION_NETWORK=orbit-workstation`. sandboxd refuses to start with the proxy and no network of its own, or with the build network, so a build can never reach the open proxy. Without the proxy sandboxd refuses that profile (400), and the built-in agent's runs fail naming the variable. Its deny lists, the same on every host that runs boxes:

- `ORBIT_EGRESS_DENY_HOSTS=.kanishksachdev.com`: the person's zone (orbit, authentik, Infisical and every other service).
- `ORBIT_EGRESS_DENY_NETS`: every fleet host's own public addresses: hetzner's `46.224.149.5/32` and its global IPv6 `/64` (`ip -6 addr show scope global` on hetzner), oci's public IPv4 (`curl -4s https://ifconfig.me` on oci) and its IPv6 if it has one. Tailnet, docker and metadata ranges are refused without listing them.

`ORBIT_EGRESS_ALLOW=*` (the build profile with any public host, 443 only) still loads, for an egress set up before the workstation profile existed; a workstation should use the profile. Claiming it is deployed: `egress: serving` with `profile=workstation` and both deny lists in `docker logs orbit-egress-open`, and from a workstation sandbox `curl -sS -o /dev/null -w '%{http_code}' https://orbit.kanishksachdev.com/healthz` failing to connect while `https://example.com` answers 200.

**Box profile** (agents' boxes and their terminals, docs/workspace.md): on top of the workstation profile, sandboxd's environment gains `ORBIT_SANDBOX_BOX_IMAGE=ghcr.io/kensac/orbit-workstation@sha256:<digest>` (sandboxd pulls it without credentials at start, so the package must be public or pulled on the host first) and, when the host has gVisor, `ORBIT_SANDBOX_BOX_RUNTIME=runsc` (unset is runc). Without the image sandboxd refuses boxes (400) and creating a terminal fails naming the variable. Each agent whose chats run in a workstation sandbox gets one container, `orbit-box-<agent>`, on `ORBIT_SANDBOX_WORKSTATION_NETWORK` with the workstation proxy (never the builds' network), and named volumes, `orbit-box-<agent>-home` (`/home/agent`), `orbit-box-<agent>-work` (`/work`), and, per Claude account, `orbit-box-<agent>-acct-<slot>` (`/home/agent/.claude-accounts/<slot>`). Docker creates them on the box's first start (account volumes are mounted when the box is recreated to add an account); they are state that exists nowhere else, carry the label `orbit.box=<agent>` (never `orbit.sandbox`, so the reaper never touches them), and must never be pruned. The container has restart policy `unless-stopped`, so it comes back after a reboot; a stopped one is recreated on the current image pin when a terminal next needs it (its volumes kept), and a running one keeps its image until it stops. On orbit, `ORBIT_WORKSPACE_ORIGINS` (comma list, default `https://kanishksachdev.com,https://www.kanishksachdev.com`) is the Origins a terminal socket accepts; traefik passes `/api/v1/terminals/{id}/ws` with its upgrade on the `/api/v1` router without forward-auth. **Claiming it works:** `docker logs orbit-sandboxd` shows `sandboxd: serving` with `box_image`; `GET /api/v1/agents/orbit/workspace` answers a `box` workspace; creating a terminal logs `sandboxd: box created` (the first time) and `box started`; an attach logs `terminal attached` on orbit and `tty attached` on sandboxd; `docker ps --filter label=orbit.box` lists the box.

## Box isolation on a host (planned, not applied)

Workstation sandboxes, agents' boxes and builds move to a second docker daemon on hetzner and oci, `docker-ws`, whose only runtime is gVisor. A container escape then lands in gVisor's sentry, not the host kernel, and nothing on that daemon shares a network, a bridge or an iptables chain with the fleet's own containers. Nothing here exists on a host yet; the homelab repository holds the roles once they are applied.

**runsc, pinned.** gVisor release `20261026.0`, the release tarball (`https://storage.googleapis.com/gvisor/releases/release/20261026.0/<arch>/gvisor.tar.bz2`, `x86_64` on hetzner, `aarch64` on oci) checked by SHA-512 against values pinned in the role, unpacked whole into `/opt/gvisor/20261026.0/` (`runsc`, `containerd-shim-runsc-v1`, `gvisor-bin/`). A bump is a new release directory and a daemon restart; the previous one stays for rollback.

**docker-ws.** A systemd unit running `dockerd --config-file /etc/docker-ws/daemon.json` against the host's containerd, with its own everything:

| | value | why |
|---|---|---|
| socket | `unix:///run/docker-ws/sock/docker.sock`, group `docker` | sandboxd's `ORBIT_SANDBOX_DOCKER_SOCKET`, its directory bind-mounted into sandboxd (a single-file mount keeps a socket's old inode once the daemon restarts); the fleet's daemon is untouched |
| data-root | hetzner `/mnt/volume-nbg1-1/docker-ws`, oci `/var/lib/docker-ws` | its own images, volumes (boxes' `/work` and `/home`) and containers; on hetzner borg and restic must add `docker-ws/volumes/` (boxes are state that exists nowhere else) and exclude the rest as for the fleet daemon |
| exec-root, pidfile | `/run/docker-ws/exec`, `/run/docker-ws/docker.pid` | two daemons never share runtime state |
| containerd namespaces | `docker-ws`, `docker-ws-plugins` | the fleet daemon's `moby` namespace is never seen |
| default runtime | `runsc` (`/opt/gvisor/20261026.0/runsc`) | every container on it is gVisor, the proxies too; sandboxd also sets `ORBIT_SANDBOX_RUNTIME=runsc` |
| `bridge` | `none` | no default bridge: only the networks below |
| `iptables`, `ip6tables`, `ip-masq`, `ip-forward`, `ipv6` | `false` | the fleet daemon owns iptables; docker-ws's filtering and NAT are the nftables tables below, so the two never rewrite each other's chains |
| `default-address-pools` | `10.251.0.0/16`, size 24 | apart from every fleet docker range and the tailnet |

**Networks on docker-ws**, each with a fixed bridge name so the nftables rules can name it, and proxies at fixed addresses so no sandbox needs DNS:

| network | bridge | subnet | internal | members |
|---|---|---|---|---|
| `orbit-ws-sandbox` | `orbws-sbx` | `10.251.0.0/24`, dynamic from `.128/25` | yes | builds; build egress `10.251.0.2`, credproxy `10.251.0.3` |
| `orbit-ws-box` | `orbws-box` | `10.251.1.0/24`, dynamic from `.128/25` | yes | workstation sandboxes and boxes; workstation egress `10.251.1.2`, credproxy `10.251.1.3` |
| `orbit-ws-out` | `orbws-out` | `10.251.2.0/24` | no | the three proxies' route out |

sandboxd (on the fleet daemon, as today, with the docker-ws socket mounted) takes `ORBIT_SANDBOX_DOCKER_SOCKET=/run/docker-ws/sock/docker.sock`, `ORBIT_SANDBOX_RUNTIME=runsc`, `ORBIT_SANDBOX_BOX_RUNTIME=runsc`, `ORBIT_SANDBOX_NETWORK=orbit-ws-sandbox`, `ORBIT_SANDBOX_PROXY=10.251.0.2:3128`, `ORBIT_SANDBOX_CREDPROXY=10.251.0.3:3129`, `ORBIT_SANDBOX_WORKSTATION_NETWORK=orbit-ws-box`, `ORBIT_SANDBOX_WORKSTATION_PROXY=10.251.1.2:3128`, `ORBIT_SANDBOX_WORKSTATION_CREDPROXY=10.251.1.3:3129`. The proxies run on docker-ws (a compose project of their own, `docker --host unix:///run/docker-ws/sock/docker.sock compose`), with a bind-mounted `resolv.conf` naming public resolvers, since docker-ws programs no DNS redirection; the workstation egress has `ORBIT_EGRESS_PROFILE=workstation` and the deny lists above. On oci, sandboxd listens on oci's tailnet address for orbit on hetzner.

**nftables**, one file loaded atomically by its own unit after docker (and again whenever docker restarts, `PartOf=docker.service`), the same ranges egress refuses (docs/agents/security.md, Workstation egress), enforced in the kernel for anything that does not go through a proxy:

- `table inet orbit_ws`, forward: anything entering or leaving `orbws-sbx` or `orbws-box` through another interface is dropped (their only peers are their proxies on the same bridge; the fleet daemon loads `br_netfilter`, so traffic within a bridge also passes this hook, and only a change of interface is refused); from `orbws-out`, drop the denied sets (`0.0.0.0/8`, `10.0.0.0/8`, `100.64.0.0/10`, `127.0.0.0/8`, `169.254.0.0/16`, `172.16.0.0/12`, `192.0.0.0/24`, `192.168.0.0/16`, `198.18.0.0/15`, `224.0.0.0/3`, every fleet host's public address; IPv6 `::/127`, `::ffff:0:0/96`, `64:ff9b::/96`, `2001:db8::/32`, `fc00::/7`, `fe80::/10`, `ff00::/8`, the fleet's public prefixes) and TCP 25, 465, 587, accept the rest; into `orbws-out`, only replies. Input: from any of the three bridges only replies, so nothing on docker-ws reaches the host's own ports (the bridges' gateway addresses, anything bound to every interface). NAT: `10.251.2.0/24` masqueraded out of any other interface.
- `table bridge orbit_ws_l2`, forward: on `orbws-sbx` and `orbws-box`, ARP and traffic to or from that bridge's two proxy addresses only, so a box never reaches another agent's box or a build.
- The fleet daemon's iptables `FORWARD` policy is `DROP`, and a packet must pass every table, so `DOCKER-USER` gets four rules (inserted only if absent): accept within `orbws-sbx` and within `orbws-box` (bridged traffic seen through `br_netfilter`), from `orbws-out` to any other interface, and established or related traffic back to it. These only let through what the nftables tables also pass. Hosts with firewalld add the three bridges to its `docker` zone.

**Claiming it is deployed**, by the verification playbook on each host: `runsc --version` names the pinned release; `docker -H unix:///run/docker-ws/sock/docker.sock info` lists `runsc` as the default runtime, the data-root and `name=seccomp`; a container run there reports gVisor's kernel (`dmesg` starts with gVisor's banner); `nft list table inet orbit_ws` and `nft list table bridge orbit_ws_l2` match the file; the fleet daemon's `docker ps` does not list docker-ws's containers. From a probe container on `orbit-ws-box`: the workstation egress tunnels to `example.com:443` and `example.com:22`; it refuses `orbit.kanishksachdev.com`, `169.254.169.254`, the host's public address, a tailnet address and port 25; a direct TCP connection to `1.1.1.1:443`, to the bridge's gateway on 22 and to a second probe container on the same bridge all fail; `orbit-ws-sandbox`'s build egress refuses `example.com`. sandboxd starts against it (`sandboxd: serving`), and a build and a workstation chat each run end to end.

## Alerts

Prometheus rules for these live in the homelab repository (`hetzner-vps-config/prometheus/rules/orbit.yml`); orbit exports what they need:

| metric | means | alert on |
|---|---|---|
| `orbit_runs_queued_oldest_age_seconds` | seconds the oldest due queued run has waited since it became due (0 when none) | a queue no executor drains (no worker with the capability, every executor wedged) |
| `orbit_runs_total{agent,status}` | finished runs, per instance | an agent failing with no success or skip in hours |
| `orbit_config_valid` | 0 while the files on disk do not load | a change that is not in force |
| `orbit_sandboxd_ready` | control's probe of the configured sandboxd's `/readyz`; absent without one | builds and sandboxed chats cannot start |
| `orbit_egress_ready{proxy}` | each egress proxy's container, as sandboxd reported it; absent when sandboxd did not answer | sandboxes cannot reach the model or package registries |
| `orbit_credential_expiry_timestamp_seconds{credential}` | `expires:` in orbit.yaml, midnight UTC | the Claude Code token lapsing within 14 days |

## Building tickets: what hetzner needs

In order; each step is safe to take alone (without step 3 approved tickets just wait).

1. **sandboxd, egress and credproxy** as above, `ghcr.io/kensac/orbit-build` present (public package or pulled), and `HETZNER_ORBIT_SANDBOX_TOKEN` and `HETZNER_ORBIT_CREDPROXY_KEY` in Infisical (`playbooks/secret-set.yml -e key=...`), so they render into `/root/.env` as `ORBIT_SANDBOX_TOKEN` and `ORBIT_CREDPROXY_KEY`.
2. **`compose.orbit.yml`**: the `orbit` service's environment has `- ORBIT_SANDBOX_TOKEN=${ORBIT_SANDBOX_TOKEN}` and it joins `orbit-sandboxd-api`. `ORBIT_DATA_DIR` holds each build's clone under `builds/` while it runs; it is removed after. The image carries `git` for this (deploy/Dockerfile); check with `docker exec orbit git --version`.
3. **`orbit/orbit.yaml`**: the `build:` section (docs/tickets.md, Configuration) with `sandboxd: 10.250.250.2:8091` and `agent:` an agent whose spec has `timeout:` at most 50m and `actions: {github:merge: approve}`. The config reloads on its own; check `config reloaded` in the log and `orbit_config_valid` 1.
4. **GitHub**: `main` of `kensac/orbit` protected (a PR and the `check` status required, the App off the bypass list), as for `github:merge`.

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
| `CLAUDE_CODE_OAUTH_TOKEN` (or `ANTHROPIC_API_KEY`) | yes (builds and sandboxed chats need the token itself) | yes: the backend credential, the only one the model sees; the token wins when both are set |
| source credentials | every agent's | only those of agents its capabilities cover |
| `ORBIT_API_TOKEN`, `ORBIT_OIDC_*` | yes | no (no API) |
| channel credentials (`ORBIT_NTFY_TOPIC`, `SMTP_*`) | yes | no (control delivers) |
| `ORBIT_GITHUB_APP_PRIVATE_KEY`, `ORBIT_SANDBOX_TOKEN` | yes | no (builds and actions are control's) |
| `ORBIT_CONNECTOR_KEY`, connector header credentials | yes (sign-ins, approved connector actions) | only on a host advertising `connectors` |
| `ORBIT_LISTEN` | public listener | `/healthz`, `/readyz`, `/metrics` only; publish it on the tailnet address |

A worker logs `agents this worker may claim reference unset variables` with the names when a covered agent's source needs a variable it lacks (such a run fails on that worker with the source's error), or when it has neither backend credential (`CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY`).

### Capabilities, per host

| capability | means | hosts |
|---|---|---|
| `docker-network:hetzner` | can reach containers on hetzner's `proxy` network by name | hetzner |
| `sure` | holds `ORBIT_SURE_API_KEY` (Sure's API) | hetzner |
| `sandbox` | holds `ORBIT_SANDBOX_TOKEN` and reaches sandboxd | hetzner |
| `github` | holds the GitHub App's key, so it can fetch the repositories agents read (`repos`) | hetzner |
| `browser` | has a browser for an agent to drive | none yet (the image has none) |
| `connectors` | holds `ORBIT_CONNECTOR_KEY` and the connectors' credentials, so an agent with `connectors` can call them | hetzner, once a connector is registered |

So today: hetzner `ORBIT_CAPABILITIES=docker-network:hetzner,sure,sandbox,github`; oci and transmission none (they run agents that need nothing). The finance agent needs `[docker-network:hetzner, sure]` and stays on hetzner. A new capability is a name agreed here, a host advertising it, and the credentials it implies on exactly those hosts.

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

The control `orbit` service keeps `sslmode=disable` over the docker network and gains `ORBIT_CAPABILITIES=docker-network:hetzner,sure,sandbox,github`. Check: from oci, `psql "postgres://orbit:...@100.79.4.41:5434/orbit?sslmode=require" -c 'select ssl from pg_stat_ssl where pid = pg_backend_pid()'` is `t`; `ss -lntp | grep 5434` on hetzner shows only `100.79.4.41`.

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
