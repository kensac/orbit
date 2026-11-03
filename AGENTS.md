# Working on this project

Instructions for any coding agent working in this repository. Read this file
first, then the topic document covering what you are about to do.

**[docs/WORKBOARD.md](docs/WORKBOARD.md) says what is built, who is working on what right now, and what is free to take.** Read it before starting, and update it in your PR.

These are instructions, not history. [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
is the design; the topic documents below are the rules for changing it.

## Non-negotiable

1. **Code first, model last.** Anything deterministic is code: fetching,
   filtering, joining, totals, diffs, thresholds, deciding whether anything
   changed. The model gets the smallest input that still needs judgment, and
   a run whose answer code can give does not call a model at all. Every
   number in a report should be one code computed. This is what keeps runs
   fast, cheap in tokens, and reproducible.
2. **The model never holds a credential it does not need to reach its own
   backend.** Sources are fetched and actions executed by the harness; the
   CLI's environment is an allowlist (`runner.childEnv`). A change that hands
   a model a source, channel or endpoint secret is wrong however convenient.
3. **A write an agent wants is an action with a rule.** Never give an agent a
   tool that writes outside its run's workdir. New capability means a new
   action type in `internal/actions`, denied unless an agent's spec names it.
4. **Never delete stored data.** Runs, events, artifacts, memory versions,
   messages and actions are kept. An interrupted attempt keeps its rows; the
   next attempt gets new ones.
5. **Never attribute an AI anywhere.** No tool or model name, no "generated
   with", no `Co-Authored-By` trailer for one: not in commits, PR titles or
   bodies, code, comments or docs.
6. **No em dashes** in the repository. `make prose` fails on one.

## Topic documents

| Read this | Before you |
|---|---|
| [working-agreement.md](docs/agents/working-agreement.md) | Take a task, open a PR, or write a doc |
| [engineering-standards.md](docs/agents/engineering-standards.md) | Write or change any code |
| [security.md](docs/agents/security.md) | Touch a runner, a tool grant, an action, a channel, or the API |
| [configuration.md](docs/agents/configuration.md), [spec-reference.md](docs/agents/spec-reference.md) | Change the spec, add a source, channel, runner or action kind |
| [store.md](docs/agents/store.md) | Touch `internal/store` or add a migration |
| [testing.md](docs/agents/testing.md) | Write a test or trust a green run |
| [deployment.md](docs/agents/deployment.md) | Claim anything is deployed |

## Repository map

- `cmd/orbit/`: the binary: `serve` (control: scheduler, executors, action loop, outbox delivery, drain, reaper, config reload and sync, HTTP), `worker` (executors only, specs from the database; `worker.go`), and operator commands.
- `cmd/orbitctl/`: the terminal client (chat, conversations, approvals) with authentik device-code sign-in.
- `cmd/orbiteval/`: checks saved run results against reference properties and reports regressions and cost.
- `internal/spec/`: configuration loading and validation (`orbit.yaml`,
  `agents/*.yaml`).
- `internal/engine/`: one run end to end, and the output contract
  (`contract.go`: the schema and the prompts).
- `internal/runner/`: model backends: `claude-code`, `codex`, `anthropic`.
- `internal/claudeproto/`: Claude Code's stream-json protocol (turns, control requests both ways, messages written mid-turn with their uuid and lifecycle, withdrawing a queued one, interrupt receipts, init capabilities, permission answers recorded before they are sent, the transport's flags, the child's credential scrub), written against the Agent SDK's wire format, not the SDK.
- `internal/journal/`: a run's typed record (prose, thinking, tool calls, diffs, plans, approvals, questions, turns) that clients render.
- `internal/harness/`: code the model's process runs: the `orbit mcp` tool server and the `orbit hook` command.
- `internal/sources/`: inputs the harness fetches (`http`, `prometheus`, `loki`, `github`, `file`).
- `internal/connectors/`: orbit's MCP client for connectors (streamable HTTP, header and OAuth credentials, sealed tokens); the engine's gateway is `internal/engine/connectors.go`.
- `internal/transforms/`: coded reducers over a source before the model sees it.
- `internal/actions/`: action types, their parameter schemas and executors.
- `internal/notify/`: channels (`ntfy`, `smtp`, `webhook`, `log`).
- `internal/mail/`: the mailbox: reply addresses that name a conversation, and the poller that turns mail into messages.
- `internal/push/`: Web Push of attention rows to registered devices (title and link only, bursts folded, skipped for whoever is looking at the conversation; signed single-use action links on approvals and permission requests; close pushes when a row closes); `internal/webpush/`: the RFC 8291 encryption and VAPID signing it sends with.
- `internal/scheduler/`: schedule triggers to queued runs, one leader.
- `internal/rrule/`: the RFC 5545 recurrence subset schedules use beyond cron.
- `internal/store/`: every database read and write; `migrations/` is the schema.
- `internal/testdb/`: a freshly migrated database per test from `ORBIT_TEST_DATABASE_URL`.
- `internal/secret/`: every signing and sealing key, derived from the operator token by purpose (security.md).
- `internal/api/`: JSON API, the run and approval pages, `/healthz`, `/readyz`, `/metrics`.
- `internal/auth/`: verification of the authentik JWTs `/api/v1` accepts.
- `internal/client/`: the API client `orbitctl` uses, including device-code sign-in.
- `internal/metrics/`: the Prometheus surface.
- `internal/github/`: orbit's only access to GitHub, as one GitHub App with per-operation installation tokens.
- `internal/claudeimport/`, `internal/frontmatter/`: reading Claude Code's on-disk memory and skills (and their YAML headers) into orbit's shapes for `orbit import`.
- `internal/eval/`: deterministic checks over saved run results, used by `orbiteval`.
- `examples/`: a complete configuration, loaded by tests and `make validate`.
- `internal/build/`: the build runner (tool policy, stdio permission
  protocol, diff policy, commit, push, PR); `internal/tickets/`: the ticket
  loop that carries a ticket from approved to done.
  `internal/build/buildtest/` holds the fake sandboxd and fake CLI build tests run against.
- `internal/sandboxd/`, `internal/egress/`, `internal/credproxy/`, `internal/sandbox/`: the build
  sandbox service (the only holder of a docker socket), its egress proxy, the credential proxy that keeps the backend credential out of every sandbox,
  and orbit's client for it.
- `internal/workspace/`: packs and unpacks a chat conversation's workspace as a tar, for its sandbox and the snapshot kept after each run.
- `deploy/`: the image (`Dockerfile`), the build sandbox's image
  (`build.Dockerfile`), an agent's workstation box image (`workstation.Dockerfile`, its files in `workstation/`) and the pinned agent CLIs (`cli/`).

## Commands

- `make testdb-up`, then `make check`: exactly CI's gates (format, vet,
  staticcheck, prose, the example config, the tests against Postgres).
  Without `make testdb-up` (and no `ORBIT_TEST_DATABASE_URL`), database tests skip.
- `make check-sandbox`: what a build sandbox can pass (no Postgres, no cgo): the same gates, tests without `-race`, database tests skipping.
- `make vuln`: govulncheck.
- `go run ./cmd/orbit validate <dir>`: check a configuration directory and
  list the environment it references.
