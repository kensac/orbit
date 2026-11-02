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
- `internal/spec/`: configuration loading and validation (`orbit.yaml`,
  `agents/*.yaml`).
- `internal/engine/`: one run end to end, and the output contract
  (`contract.go`: the schema and the prompts).
- `internal/runner/`: model backends: `claude-code`, `codex`, `anthropic`.
- `internal/claudeproto/`: Claude Code's stream-json protocol (turns, control requests both ways, permission answers recorded before they are sent, the transport's flags, the child's credential scrub), written against the Agent SDK's wire format, not the SDK.
- `internal/sources/`: inputs the harness fetches (`http`, `prometheus`, `file`).
- `internal/transforms/`: coded reducers over a source before the model sees it.
- `internal/actions/`: action types, their parameter schemas and executors.
- `internal/notify/`: channels (`ntfy`, `smtp`, `webhook`, `log`).
- `internal/push/`: Web Push of attention rows to registered devices (title and link only, bursts folded); `internal/webpush/`: the RFC 8291 encryption and VAPID signing it sends with.
- `internal/scheduler/`: schedule triggers to queued runs, one leader.
- `internal/store/`: every database read and write; `migrations/` is the schema.
- `internal/api/`: JSON API, the run and approval pages, `/healthz`, `/readyz`, `/metrics`.
- `examples/`: a complete configuration, loaded by tests and `make validate`.
- `internal/build/`: the build runner (tool policy, stdio permission
  protocol, diff policy, commit, push, PR); `internal/tickets/`: the ticket
  loop that carries a ticket from approved to done.
- `internal/sandboxd/`, `internal/egress/`, `internal/sandbox/`: the build
  sandbox service (the only holder of a docker socket), its egress proxy,
  and orbit's client for it.
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
