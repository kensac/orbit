# Workboard

The shared map of orbit's work for every coding agent: Claude Code sessions and Codex. Read [AGENTS.md](../AGENTS.md) first; this file says what exists, who is working on what right now, and what is free to take. Update it in the same PR as your work (move your item, add what you learned). Last full refresh: 2026-11-02.

## What orbit is

A self-hosted Go service on Kanishk's server (hetzner) that runs Claude Code on his subscription as standing agents he can reach from anywhere: a chat in his portfolio (kanishksachdev.com/orbit), email, an MCP connector for the Claude apps. Agents run on schedules or when he writes, read data orbit fetches for them, keep memory and learnings across sessions, use skills and hooks, delegate to each other, and change orbit's own code through sandboxed builds that open and merge PRs. The goal, in his words: "a dedicated Claude Code session on steroids with all the skills and hooks and context built over sessions condensed into super agents." Design: [docs/ARCHITECTURE.md](ARCHITECTURE.md). API contract: [docs/api.md](api.md). Tickets and builds: [docs/tickets.md](tickets.md).

## Built and merged

| Area | What | PRs |
|---|---|---|
| Core | agents from YAML and the database (versioned), conversations (many per agent), runs as a queue, memory, questions, rule-gated actions, notifications (only for unattended runs) | #4 #6 #12 #22 #24 |
| Message reads | every attempt's read of a person's message kept across hand-backs and failures (`message_reads`); the next-run selection and the thread read from it | #75 |
| Claude Code | `internal/claudeproto`: the CLI's stream-json protocol in Go (turns, control requests, interrupt, permission prompts, credential scrubbing); session resume with fork; a live process per conversation | #33 #35 #36 #39 #6 |
| Journal | typed run journal (messages, thinking estimates, tool calls with inputs and bounded results, diffs, plans, approvals, questions, turn state vs outcome), read by the thread, the SSE stream, the run page and MCP | #34 #38 |
| Determinism | orbit's MCP tools (exact `query`, `calc`), hooks (path guard, figures check), transforms (`sure-monthly`, JSON reducers), gates that skip a scheduled run with no model call, the `skipped` status | #8 #20 #23 #27 |
| Memory | typed memory entries at profile, topic and agent scope with every version kept; agents write their own; a learning pass over quiet conversations; memory document write and restore; `orbit import claude-memory` | #28 #44 |
| Skills and hooks | a versioned skills library given to each Claude Code run as real skills (stubs plus `mcp__orbit__skill`), agents writing and retiring their own, `orbit import skills`; per-agent hooks (`say`, `deny`, `run` at a Claude Code event) | #49 #57 |
| Tickets and builds | tickets on conversations, GitHub App tokens (App `orbit-kensac`, id 5571920), the build runner (sandboxed Claude Code, orbit commits and pushes), the ticket loop, `github:merge` gated on green checks | #9 #14 #17 |
| Define limits | database and built-in agents held to orbit.yaml: no `file` source, `${VAR}` only from `credentials:` (each bound to a URL prefix, checked on every request and redirect), without a sandbox, tools only from `spec.UnsandboxedTools`; an `agent:define` that widens an agent waits for a person | #71 |
| Repositories | agents read git repositories (`repos:`, checked against `github.read_repositories`): a bare mirror per repository fetched with a per-fetch read token, the tree at a commit in the run's inputs without `.git`, `_repos.json` in the `unchanged` gate, read-only `git_log`/`git_show`/`git_diff`/`git_blame`/`pr_list`/`pr_view` run by orbit, builds seeded from the mirror (ARCHITECTURE.md, Repositories) | #84 |
| Sandbox | `orbit sandboxd` (one locked-down container per build or workspace), `orbit egress` (allowlist proxy, never GitHub), the build image | #15 |
| Workstation image | `ghcr.io/kensac/orbit-workstation` (`deploy/workstation.Dockerfile`), amd64 and arm64: an agent's persistent box with a laptop's toolset, Docker for an inner dockerd, gh and git reading `/run/orbit/gh-token`, tmux for browser terminals, the pinned Claude Code; PRs build both arches and smoke every tool. Not yet used by sandboxd (its `box` profile is W9 in #78) | #81 |
| Operations | seamless upgrades (readiness, drain, resuming interrupted attempts), the worker fleet (capabilities, control vs worker, sessions across hosts), usage and health APIs | #11 #16 #25 |
| Surfaces | thread and SSE API with authentik JWT auth, MCP server with OAuth proxy, mail (replies continue a conversation), uploads | #4 #21 #31 #45 |
| MCP parity | MCP tools that mirror an HTTP list or read share its page shape, store call and error wording (`mcp_mirror_test.go` pins them) | #76 |
| Orchestration | `delegate` to other agents or fresh workers, each task in its own conversation; task DAGs; parent and child mailboxes (`task:message`, questions to the parent first); decision gates only a person resolves; `delegation` wakeups when tasks settle (never polling); the built-in `orbit` agent delegates to every agent; tasks in the thread, `/tasks` API and a live tasks stream (ARCHITECTURE.md, Delegation) | #46 |
| Hygiene | API fixes, exact documented shapes, reserved agent names, migration uniqueness, the runners on claudeproto's protocol code (no copies in internal/runner), `make check` skipping DB tests without a database, `make check-sandbox` for build sandboxes, a spec field reference (docs/agents/spec-reference.md) held to the structs by `TestReferenceCoversSpec`; one strict request decoder (unknown fields are 400), integer query params checked, one limit clamp and one UUID check | #18 #19 #26 #29 #30 #40 #65 #73 #66 #74 |

Deployed on hetzner: everything up to the image pinned in `hetzner-vps-config/compose.orbit.yml` (at the time of writing 21ab048; newer images were blocked by a duplicate migration, fixed in #50). MCP is live; mail is prepared but not enabled (docs in the homelab repo). The worker fleet is built but not deployed.

## In flight: owned by Claude Code agents (do not touch these areas)

| Area | Branch / PR | Notes |
|---|---|---|
| General `orbit` agent: a plain Claude Code session per conversation in a sandboxed persistent workspace, owner messages as instructions, agent self-edit (`agent:define`), uploads/files endpoints for the chat, stopping a running reply | #47, branch general-agent work | the chat UI already calls the endpoints described in docs/api.md |
| Search across everything stored, plus a live usage poller and deferring low-priority runs | #42 search | |
| Attention events, inbox API, Web Push (portfolio PWA) | #41 orbit-push-attention | the portfolio is already a PWA |
| Hook-driven live agent status | #37 agent-status | |
| Worktrees, review comments on PR diffs, stacked PRs, check reruns, RRULE schedules with a precheck | #32 #43 | |
| Migration fix (duplicate version 14) | #50 | |
| Workspace: an IDE in the portfolio chat (explorer, editor, terminals with Claude Code (direct), diffs with line comments), one shell with the chat | #78 docs/workspace-contract (design); items below | design [docs/workspace.md](workspace.md), contract docs/api.md, Workspace (planned) |

### Workspace: implementation items, in order

Each is one PR. A portfolio item (kensac/personal-portfolio, `app/orbit`) meets workspace.md, One shell, and says how in its description. An orbit item drops "planned" from the api.md routes it builds.

| # | Repo | Item | Needs |
|---|---|---|---|
| W1 | orbit | Workspace resolver and snapshot reads: `GET /conversations/{id}/workspace`, `/agents/{name}/workspace`, `workspace_id` on conversations, tree, stat, files (2 MiB, binary detection), download; `workspace_events`; the workspace group and the rate and size limits | #59 (workspaces) |
| W2 | portfolio | The shell: layout, rail and tabs, one command palette, tokens for every panel, the shared store and `open(ref)` with deep links | |
| W3 | orbit | Snapshot writes: `PUT` with `If-Match`, mkdir, rename, trash and restore, upload and signed upload, `busy`, pushing into a live sandbox, the next turn told what the person changed | W1 |
| W4 | portfolio | Explorer: lazy tree, create, rename, trash and restore, upload by drop, download | W2, W1 (W3 for writes) |
| W5 | portfolio | Editor on CodeMirror 6 (below): tabs, save with `If-Match` and a merge on `412`, binary and too-large states, image preview | W2, W3 |
| W6 | orbit | Change stream (snapshot backend), history, `changes` on run items and `GET /runs/{id}/changes` | W3 |
| W7 | orbit | Review comments and `POST /conversations/{id}/review` | W6 |
| W8 | portfolio | Diff view (`@codemirror/merge`, unified and side by side) for a reply's changes, line comments and sending the review; the explorer and editor live from the change stream | W5, W6, W7 |
| W9 | orbit | sandboxd `box` profile (persistent, runsc required, volumes, no lifetime) and TTY attach with resize; the multi-arch box image with the toolset, gh, tmux, the pinned CLI and the `orbit box` helper | |
| W10 | orbit | Box backend: file routes through `orbit box`, the inotify watcher behind the change stream, per-turn checkpoints, the agent's chat turns in its box, box snapshots and restore on either host | W9, W6 |
| W11 | orbit | Terminals: tmux lifecycle, attach tickets, the socket with Origin check, the SSE and POST fallback, scrollback, the `claude` preset, Claude sessions list, gh credential in terminal environments | W10, the WebSocket spike's answer |
| W12 | portfolio | Terminals on xterm.js (fit, WebGL with a canvas fallback, web links, search), several per workspace, a mobile key bar (Esc, Ctrl, Tab, arrows), direct socket with the proxied fallback | W2, W11 |
| W13 | orbit | Git routes in the box (repos, status, diff, log, show, branches, commit and push as the person) and the `workspace:push` action | W10 |

Editor: CodeMirror 6, not Monaco. Monaco's editor and its language workers are megabytes before compression, it is not supported on mobile browsers, and it brings its own theming; CodeMirror 6 is modular (a basic setup with a few languages is low hundreds of KB), works with touch, native selection and IME on phones, themes from CSS variables (the portfolio's tokens), and `@codemirror/merge` gives the diff view the same engine. Measure both bundles in W5's PR and record the numbers there.

The Claude side also plans: agent templates and more sources (Gmail, Calendar, Drive, GitHub, Prometheus/Loki, RSS, the DuckDB lake), a homelab-ops agent, presence-aware notifications across devices, the "safeguard later" pass (a credential proxy so the Claude token stays out of the sandbox, a review of self-edit rights, rate limits), and deploying the worker fleet. Ask before starting any of these.

## Integration prerequisites

| Area | Branch / PR | Notes |
|---|---|---|
| Run event ordering | `codex/event-sequence`, #72 | Serializes sequence allocation, message capture and complete event/journal persistence across concurrent callbacks; blocks integration checks until reviewed |

## Open for Codex

Pick one, put your name and branch in the Owner column in your first PR, keep each PR to one reviewable behaviour.

| # | Item | Owner | Notes |
|---|---|---|---|
| 1 | `orbit` CLI client for the laptop: chat with any agent, stream replies, list conversations, approve actions, from the terminal like Claude Code; sign-in with authentik's device-code flow (no orbit-local tokens) | `codex/terminal-client`, #53 | `cmd/orbitctl` implemented and unit-tested against local TLS endpoints; real provider sign-in smoke remains |
| 2 | Natural-language schedules ("every weekday at 8", "the 1st at 9am") parsed by code into cron/RRULE, quiet hours per agent, calendar-aware skipping | #64 | Strict recurring phrase compiler and standalone preview; quiet hours and calendar-aware skipping remain open. |
| 3 | Per-agent budgets: API-equivalent cost and run counts per period, a gate that defers or refuses scheduled runs past a budget, alerts | | builds on /usage (#11) |
| 4 | Agent evaluations: saved reference runs (inputs + expected properties), a command that replays them against the current charter/skills and reports regressions and cost | #54 | First slice: offline artifact regression checks and exact cost reporting. Current-charter replay and model grading remain open. |
| 5 | Observability: a Grafana dashboard for orbit's metrics, structured logs suitable for Loki, Prometheus alert rules for stuck queues, failing agents, config errors | | the homelab side (dashboards, rules) lives in kensac/hetzner-vps-config; open PRs there, do not push main |
| 6 | Backup and export: export/import agents, memory, skills and conversations as a portable bundle; a restore drill for orbit-db documented | `codex/portable-export`, #63 | First slice: read-only selected-agent API export with explicit coverage, hashes and verification. Restore drill of the nightly orbit-db dump: `deploy/restore-drill.sh` and `orbit restore-check` (docs/agents/deployment.md, Restore drill). Import remains open. |
| 7 | Messaging channels: Telegram (or Slack) as a two-way surface like mail: replies continue a conversation, new messages start one | | mirror internal/mail's design |
| 8 | Voice: dictation in the chat (browser) and spoken replies | | portfolio repo, app/orbit/ |
| 9 | Plugin system for sources, actions and tools without editing orbit (a manifest plus an executable or WASM module, run in the sandbox) | plugin-design #56; codex/plugin-manifest #62 | Manifest/package validation and standalone `orbitextension validate` command; no installation, execution or grants; design: [docs/plugins.md](plugins.md) |
| 10 | Docs and onboarding: a user guide (what orbit can do, creating an agent in a minute, the chat, email, MCP), kept short | onboarding-guide, #52 | Built: [docs/guide/README.md](guide/README.md); API examples, approval results, email and MCP prerequisites; deployment availability stated explicitly |

## Standards (summary; AGENTS.md and docs/agents/ are authoritative)

- **Code first, model last.** Deterministic work (fetching, totals, diffs, deciding whether anything changed) is code; the model gets only what needs judgment.
- **The model never holds orbit's secrets or GitHub tokens.** Writes an agent wants are actions executed by orbit. Interactive messages from the owner count as his instructions; unattended runs follow the agent's rules.
- **Never delete stored data.** New versions, never overwrites. DB access only through `internal/store`, with guarded state transitions.
- **Migrations:** name new ones by UTC timestamp, `YYYYMMDDHHMMSS_name.sql` (`date -u +%Y%m%d%H%M%S`). Never the next small number: parallel agents reusing numbers broke main twice. Additive only; never edit a merged migration.
- **Tests against real Postgres** (`ORBIT_TEST_DATABASE_URL`, `make testdb-up`; without either, database tests skip); `make check` and golangci-lint must pass; CI must be green before merging. Inside a build sandbox (no Postgres, no cgo) run `make check-sandbox`; CI still runs the full gates.
- **Workflow:** your own git worktree or clone (never switch the branch of a shared checkout); small PRs; merge your own PR (squash) once CI is green; if main moved, merge main into your branch; never force-push. Keep docs/api.md exact when the API changes, because the portfolio chat builds against it.
- **Writing:** no AI attribution anywhere (no tool or model names, no "generated with", no Co-Authored-By) in commits, PR titles and bodies, code or docs. No em dashes. Do not hard-wrap prose at 80 columns: one line per paragraph or sentence in docs, comments, commit messages and PR bodies (code follows gofmt). Comments state the current rule and its reason, not history.
- **Deploying is not your job here.** A merged PR builds an image; the owner's main session pins it on hetzner and rolls it out. Never edit anything on a host.
