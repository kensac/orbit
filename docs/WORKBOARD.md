# Workboard

The shared map of orbit's work for every coding agent: Claude Code sessions and Codex. Read [AGENTS.md](../AGENTS.md) first; this file says what exists, who is working on what right now, and what is free to take. Update it in the same PR as your work (move your item, add what you learned). Last full refresh: 2026-11-02.

## What orbit is

A self-hosted Go service on Kanishk's server (hetzner) that runs Claude Code on his subscription as standing agents he can reach from anywhere: a chat in his portfolio (kanishksachdev.com/orbit), email, an MCP connector for the Claude apps. Agents run on schedules or when he writes, read data orbit fetches for them, keep memory and learnings across sessions, use skills and hooks, delegate to each other, and change orbit's own code through sandboxed builds that open and merge PRs. The goal, in his words: "a dedicated Claude Code session on steroids with all the skills and hooks and context built over sessions condensed into super agents." Design: [docs/ARCHITECTURE.md](ARCHITECTURE.md). API contract: [docs/api.md](api.md). Tickets and builds: [docs/tickets.md](tickets.md).

## Built and merged

| Area | What | PRs |
|---|---|---|
| Core | agents from YAML and the database (versioned), conversations (many per agent), runs as a queue, memory, questions, rule-gated actions, notifications (only for unattended runs); `agent:define` (agents change agents, definitions record their editor and revert) | #4 #6 #12 #22 #24 #47 |
| Claude Code | `internal/claudeproto`: the CLI's stream-json protocol in Go (turns, control requests, interrupt, permission prompts, credential scrubbing); session resume with fork; a live process per conversation | #33 #35 #36 #39 #6 |
| Journal | typed run journal (messages, thinking estimates, tool calls with inputs and bounded results, diffs, plans, approvals, questions, turn state vs outcome), read by the thread, the SSE stream, the run page and MCP | #34 #38 |
| Determinism | orbit's MCP tools (exact `query`, `calc`), hooks (path guard, figures check), transforms (`sure-monthly`, JSON reducers), gates that skip a scheduled run with no model call, the `skipped` status, RRULE triggers and a precheck gate | #8 #20 #23 #27 #43 |
| Memory | typed memory entries at profile, topic and agent scope with every version kept; agents write their own; a learning pass over quiet conversations; `orbit import claude-memory` | #28 #44 |
| Skills | a versioned skills library, real Claude Code skills per run, agents writing and retiring their own, `orbit import skills` | #49 |
| Search | search across messages, reports, narration, tool calls and memory | #42 |
| Tickets and builds | tickets on conversations, GitHub App tokens (App `orbit-kensac`, id 5571920), the build runner (sandboxed Claude Code, orbit commits and pushes), the ticket loop, `github:merge` gated on green checks, failed checks rerun once and compared with the base before a fix, review comments on a ticket's PR applied by a revise build, stacked tickets restacked by code and merged in order, a checkout pool (one clone per ticket, warm spares) and setup and cleanup scripts trusted by content hash | #9 #14 #17 #32 #58 #69 #77 |
| Sandbox | `orbit sandboxd` (one locked-down container per build, or a `workstation` per chat conversation), `orbit egress` (allowlist proxy, never GitHub; open egress for workstations), the build image | #15 #59 |
| Chat agents | the built-in `orbit` agent as a plain Claude Code session per conversation in a sandbox, with a workspace snapshotted after each run and restored on any host (`internal/workspace`), orbit's tools over the control channel, workspace file downloads, stopping a running reply | #59 |
| Operations | seamless upgrades (readiness, drain, resuming interrupted attempts), the worker fleet (capabilities, control vs worker, sessions across hosts), usage and health APIs | #11 #16 #25 |
| Surfaces | thread and SSE API with authentik JWT auth, MCP server with OAuth proxy, mail (replies continue a conversation), uploads, `orbitctl` terminal client with authentik device-code sign-in, attention events with an inbox API and Web Push to registered devices | #4 #21 #31 #45 #53 #41 |
| Evaluations | `orbiteval`: deterministic checks and exact costs over saved run artifacts (no replay yet) | #54 |
| Docs | the workboard, the user guide (docs/guide/), the extension design (docs/plugins.md) | #51 #52 #56 |
| Migrations | the duplicate version 14 fixed, out-of-order allowed, new ones named by timestamp | #50 |
| Orchestration | `delegate` to other agents or fresh workers, each task in its own conversation; task DAGs; parent and child mailboxes (`task:message`, questions to the parent first); decision gates only a person resolves; `delegation` wakeups when tasks settle (never polling); the built-in `orbit` agent delegates to every agent; tasks in the thread, `/tasks` API and a live tasks stream (ARCHITECTURE.md, Delegation) | #46 |
| Hygiene | API fixes, exact documented shapes, reserved agent names, migration uniqueness, the runners on claudeproto's protocol code (no copies in internal/runner), `make check` skipping DB tests without a database, `make check-sandbox` for build sandboxes, a spec field reference (docs/agents/spec-reference.md) held to the structs by `TestReferenceCoversSpec`; one strict request decoder (unknown fields are 400), integer query params checked, one limit clamp and one UUID check | #18 #19 #26 #29 #30 #40 #65 #73 #66 #74 |

Deployed on hetzner: everything up to the image pinned in `hetzner-vps-config/compose.orbit.yml` (at the time of writing 21ab048; newer images were blocked by a duplicate migration, fixed in #50). MCP is live; mail is prepared but not enabled (docs in the homelab repo). The worker fleet is built but not deployed.

## In flight: owned by Claude Code agents (do not touch these areas)

| Area | Branch / PR | Notes |
|---|---|---|
| A live usage poller and deferring low-priority runs | #55 usage-poll | search merged in #42 |
| Memory restore endpoint, per-agent hooks | #57 context-hooks-2 (hooks) | skills library merged in #49 |
| Hook-driven live agent status | #37 agent-status | |

The Claude side also plans: agent templates and more sources (Gmail, Calendar, Drive, GitHub, Prometheus/Loki, RSS, the DuckDB lake), a homelab-ops agent, presence-aware notifications across devices, the "safeguard later" pass (a credential proxy so the Claude token stays out of the sandbox, a review of self-edit rights, rate limits), and deploying the worker fleet. Ask before starting any of these.

## Integration prerequisites

| Area | Branch / PR | Notes |
|---|---|---|
| Run event ordering | Codex, `codex/event-sequence`, #72 (merged) | Serializes sequence allocation, message capture and complete event/journal persistence across concurrent callbacks; blocks integration checks until reviewed |

## Open for Codex

Pick one, put your name and branch in the Owner column in your first PR, keep each PR to one reviewable behaviour.

| # | Item | Owner | Notes |
|---|---|---|---|
| 1 | `orbit` CLI client for the laptop: chat with any agent, stream replies, list conversations, approve actions, from the terminal like Claude Code; sign-in with authentik's device-code flow (no orbit-local tokens) | Codex, `codex/terminal-client`, #53 (merged) | `cmd/orbitctl` implemented and unit-tested against local TLS endpoints; real provider sign-in smoke remains |
| 2 | Natural-language schedules ("every weekday at 8", "the 1st at 9am") parsed by code into cron/RRULE, quiet hours per agent, calendar-aware skipping | | coordinate with #43 (RRULE) once it merges |
| 3 | Per-agent budgets: API-equivalent cost and run counts per period, a gate that defers or refuses scheduled runs past a budget, alerts | | builds on /usage (#11) |
| 4 | Agent evaluations: saved reference runs (inputs + expected properties), a command that replays them against the current charter/skills and reports regressions and cost | Codex, `codex/saved-run-evaluations`, #54 (merged) | First slice: offline artifact regression checks and exact cost reporting. Current-charter replay and model grading remain open. |
| 5 | Observability: a Grafana dashboard for orbit's metrics, structured logs suitable for Loki, Prometheus alert rules for stuck queues, failing agents, config errors | | the homelab side (dashboards, rules) lives in kensac/hetzner-vps-config; open PRs there, do not push main |
| 6 | Backup and export: export/import agents, memory, skills and conversations as a portable bundle; a restore drill for orbit-db documented | | |
| 7 | Messaging channels: Telegram (or Slack) as a two-way surface like mail: replies continue a conversation, new messages start one | | mirror internal/mail's design |
| 8 | Voice: dictation in the chat (browser) and spoken replies | | portfolio repo, app/orbit/ |
| 9 | Plugin system for sources, actions and tools without editing orbit (a manifest plus an executable or WASM module, run in the sandbox) | Codex, `codex/plugin-design`, #56 (merged) | Proposed design: [docs/plugins.md](plugins.md); manifest/protocol, grants, sandbox boundary and acceptance tests; no runtime implementation |
| 10 | Docs and onboarding: a user guide (what orbit can do, creating an agent in a minute, the chat, email, MCP), kept short | Codex, `codex/onboarding-guide`, #52 (merged) | Built: [docs/guide/README.md](guide/README.md); API examples, approval results, email and MCP prerequisites; deployment availability stated explicitly |

## Standards (summary; AGENTS.md and docs/agents/ are authoritative)

- **Code first, model last.** Deterministic work (fetching, totals, diffs, deciding whether anything changed) is code; the model gets only what needs judgment.
- **The model never holds orbit's secrets or GitHub tokens.** Writes an agent wants are actions executed by orbit. Interactive messages from the owner count as his instructions; unattended runs follow the agent's rules.
- **Never delete stored data.** New versions, never overwrites. DB access only through `internal/store`, with guarded state transitions.
- **Migrations:** name new ones by UTC timestamp, `YYYYMMDDHHMMSS_name.sql` (`date -u +%Y%m%d%H%M%S`). Never the next small number: parallel agents reusing numbers broke main twice. Additive only; never edit a merged migration.
- **Tests against real Postgres** (`ORBIT_TEST_DATABASE_URL`, `make testdb-up`; without either, database tests skip); `make check` and golangci-lint must pass; CI must be green before merging. Inside a build sandbox (no Postgres, no cgo) run `make check-sandbox`; CI still runs the full gates.
- **Workflow:** your own git worktree or clone (never switch the branch of a shared checkout); small PRs; merge your own PR (squash) once CI is green; if main moved, merge main into your branch; never force-push. Keep docs/api.md exact when the API changes, because the portfolio chat builds against it.
- **Writing:** no AI attribution anywhere (no tool or model names, no "generated with", no Co-Authored-By) in commits, PR titles and bodies, code or docs. No em dashes. Do not hard-wrap prose at 80 columns: one line per paragraph or sentence in docs, comments, commit messages and PR bodies (code follows gofmt). Comments state the current rule and its reason, not history.
- **Deploying is not your job here.** A merged PR builds an image; the owner's main session pins it on hetzner and rolls it out. Never edit anything on a host.
