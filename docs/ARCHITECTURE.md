# Architecture

orbit runs standing agents: each has a charter (what it is responsible for),
triggers (when it wakes), sources (what it reads), memory (what it keeps
between runs), an inbox (what its person tells it), and rules (what it may do
alone, what waits for approval, what it may not do at all).

## Code first, model last

The model is the expensive, slow, non-deterministic part, so it gets only the
part of a job that needs judgment. Everything else is code that runs before
it: the harness fetches and paginates sources today; the next pieces are
transforms (coded reducers over a source: per-category totals, transfers
removed, a diff against last run) and gates (skip the model when inputs are
unchanged or a check passes, so a quiet day costs no tokens). A report's
numbers should come from code, with the model explaining them.

## One run

```
trigger (schedule | manual | message)
  -> runs row, status queued
  -> executor claims it (FOR UPDATE SKIP LOCKED; at most one running per agent)
  -> StartRunContext: memory + unread messages (marked consumed) + open questions
  -> sources fetched by the harness, written to <workdir>/inputs/, stored as artifacts
  -> runner called with: system contract, prompt, workdir, tools, output schema
  -> output validated against the contract
  -> RecordOutcome, one transaction: run succeeded, memory version, questions, actions
  -> notifications: the report, then one per action waiting for approval
  -> action loop executes approved actions (auto ones arrive approved by rule)
```

Everything a run read and wrote is in Postgres: the prompt, system contract,
schema, every input, the raw output, the backend's full event stream, every
delivery attempt. The workdir is scratch and is removed after the run.

## The output contract

Every backend returns the same JSON (`internal/engine/contract.go`):
`summary`, `report` (Markdown), `notify` (send, priority, title), `memory`
(update, the complete new document), `questions`, `actions`. The backend is
given a strict schema in which each action type's parameters are spelled out;
the engine validates against a looser envelope and then judges each action on
its own, so one malformed proposal is denied without losing the report.

## Backends are interchangeable

A runner takes a fully built request and returns the output JSON. Nothing an
agent means is resolved inside a runner, so moving an agent from Claude Code
to Codex or to a plain API call is one line in its spec.

| kind | how | auth | tools |
|---|---|---|---|
| `claude-code` | `claude -p --output-format stream-json --json-schema` | `CLAUDE_CODE_OAUTH_TOKEN` (subscription, `claude setup-token`) or `ANTHROPIC_API_KEY` | Read/Glob/Grep in the workdir, plus the agent's `tools` |
| `codex` | `codex exec --json --output-schema -s read-only` | `CODEX_API_KEY` / `OPENAI_API_KEY` / `CODEX_HOME` | its read-only sandbox; agent `tools` refused |
| `anthropic` | Messages API, schema as a forced tool | `api_key_env` | none; inputs inlined (bounded) |

## Memory, questions, messages

- **Memory** is one Markdown document per agent, replaced whole by a run that
  sets `memory.update`. Every version is kept (`memory_versions`).
- **Questions** a run asks become `from_agent` messages and are delivered with
  the report. A question stays open until a reply names it or any later
  message reaches the agent; open questions are shown to the next run so it
  does not ask again.
- **Messages** to an agent are consumed by the first run that starts after
  them. A run that fails or is interrupted hands them back.

## Actions and rules

An action type is code (`internal/actions`): a name, a parameter schema shown
to the agent, a validator, and an executor. An agent's spec maps types to
`auto`, `approve` or `deny`; unnamed types are denied and not offered.
Today: `notify` (an extra message to one of the agent's channels) and
`webhook:<endpoint>` (a JSON body to an endpoint whose URL and credentials
are configuration, never chosen by the model).

Approval happens from a signed link (`/ui/actions/{id}?sig=`), the API, or
`orbit approve`. An action is executed at most once: one interrupted during
execution is failed for a person to check, never retried.

## Reliability

- **Scheduling:** one leader by advisory lock. A fire is enqueued at most once
  (`runs_one_per_fire`). After downtime, each trigger runs once for its newest
  missed fire; a trigger that has never fired is not backfilled.
- **Shutdown:** a run in progress gets the drain window (`ORBIT_DRAIN`), then
  goes back to the queue. Nothing a run does outside its own rows happens
  before it finishes, so another attempt is safe.
- **Lost workers:** a run without a heartbeat for `engine.StaleAfter` is
  requeued, and failed after `store.MaxAttempts` claims.
- **Configuration** reloads when the directory's content changes. An invalid
  change is refused and the previous configuration kept
  (`orbit_config_valid` = 0).

## Not built yet

The direction, so changes now do not close it off:

- **Transforms and gates** (above). A transform is a named, tested Go
  function from a source's bytes to smaller bytes, stored as its own
  artifact; a gate is a predicate over inputs and memory that ends the run
  without a backend call, recorded as its own status (`skipped`, naming the
  gate) so a run that judged nothing is distinguishable from one that did.

- **Tickets and a build agent**: [tickets.md](tickets.md). A request or
  question thread a person files and discusses with an agent; an approved
  plan becomes a build run in a short-lived container with a checkout and
  write tools, no orbit secrets, and no GitHub credential; orbit commits,
  pushes `orbit/*` and opens the PR itself, and merging is an action
  (`github:merge`) whose executor refuses unless every required check is
  green, because in the fleet a merge is a deploy.
- **Event triggers** from webhooks (a ticket assigned, a comment).
- **A UI** over `/api/v1`.
