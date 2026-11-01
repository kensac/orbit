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
trigger (schedule | manual | message | continue)
  -> runs row, status queued
  -> executor claims it (FOR UPDATE SKIP LOCKED; at most one running per agent;
     a continuation not before its run_after)
  -> StartRunContext: memory + unread messages (marked consumed) + open questions
  -> session plan: continue the agent's Claude Code session, or start fresh
  -> a fresh session: the recent thread, packed into a byte budget, as conversation.md
  -> sources fetched by the harness, written to <workdir>/inputs/, stored as artifacts
  -> runner called with: system contract, prompt, workdir, tools, output schema
  -> output validated against the contract
  -> RecordOutcome, one transaction: run succeeded, memory version, questions, actions
  -> notifications: the report, then one per action waiting for approval
  -> action loop executes approved actions (auto ones arrive approved by rule)
```

Everything a run read and wrote is in Postgres: the prompt, system contract,
schema, every input, the raw output, the backend's full event stream, every
delivery attempt. The workdir is scratch and is removed after the run, except
for an agent with a session (below), whose workdir and HOME persist and whose
inputs go in a per-attempt directory removed after the run.

## Conversations

A run that starts a fresh session is shown its agent's recent thread: the
person's messages earlier runs read, earlier reports and errors, and the
questions asked, newest kept, within `conversationBudget` (24 KB, each item
clipped to a quarter of it; `internal/engine/conversation.go`). It is stored
as the `conversation.md` artifact. The packing is deterministic.

## Sessions (claude-code, opt-in per agent)

`session: {resume: true, idle: 6h, live: 10m}` keeps an agent's Claude Code
session between runs. Claude Code stores a session's transcript at
`$CLAUDE_CONFIG_DIR/projects/<cwd, non-alphanumerics as ->/<id>.jsonl`
(`runner.SessionFile`); orbit pins `CLAUDE_CONFIG_DIR` to
`<data>/agents/<name>/home/.claude` and the cwd to `<data>/agents/<name>/work`,
so `ORBIT_DATA_DIR` must be a persistent volume for resume to survive a restart. The
transcript is on the host that ran the session; a run claimed where it is
missing starts fresh rather than failing. For the worker fleet (below), the
central store would save it after a run and write it back before a resume,
as the Agent SDK's SessionStore does (it materializes
`<dir>/projects/<key>/<id>.jsonl` and points `CLAUDE_CONFIG_DIR` there).

- **Resume.** A run continues the session of the agent's latest finished run
  when that run succeeded on Claude Code under the same spec within `idle`,
  no live turn on the session failed, the chain is under `maxSessionDepth`
  (20) runs, and the transcript is on disk. Otherwise it starts fresh and the
  `orbit.session` event says why. Every resume is `--resume <id>
  --fork-session`: a new id each run, so an interrupted attempt never edits
  the session the next attempt continues. A continued run's prompt keeps the
  charter and memory (small, and the authority if the CLI compacts) and drops
  the conversation, which the session holds.
- **Live process.** A run a person's message starts opens one `claude -p
  --input-format stream-json` process and leaves it open for `live`; the next
  run of that agent (any trigger) is written to it as one user message while it
  holds the session that run would resume. Each message still is a run row
  with its own events and outcome. A turn ends at its `result` line, which
  carries that turn's own `structured_output` (verified on 2.1.316). Any
  failure, a crash, a timeout or a mismatch closes the process; at most
  `runner.MaxLive` (4) are open, the least recently used idle one closed first.
- **Recorded per run:** `session_mode` (fresh, resumed, live),
  `session_parent`, `session_depth`, `session_id`, and `session_cost_usd`. The
  CLI reports a cumulative cost per session (across turns, resumes and forks),
  so `cost_usd` is the difference from the parent run's total.

## orbit inside the CLI: tools and hooks (claude-code)

A Claude Code run starts orbit's own binary twice over, so deterministic work
happens in tested Go at the moment the model needs it (`internal/harness`):

- **Tools** (`orbit mcp`, an MCP stdio server from `--mcp-config` under
  `--strict-mcp-config`): `mcp__orbit__query` filters, groups and
  counts or sums the records of a JSON input, exactly (numbers are read as the
  decimal literals they are); `mcp__orbit__calc` is exact rational arithmetic.
  The system prompt tells the model to compute every figure with them. Both
  read only under the workdir, and append each result to a ledger in the run's
  state directory.
- **Hooks** (`orbit hook`, from `--settings`; with `--setting-sources ""` no
  other hook is loaded, so `disableAllHooks` is on whenever orbit has none):
  PreToolUse on Read, Glob and Grep denies a path that leaves the workdir after
  symlinks, a second wall behind the CLI's own; with `verify: [figures]`,
  PreToolUse on StructuredOutput sends the output back when a `$` amount in its
  summary, title or report is in neither the inputs (as written, or as cents)
  nor the ledger, rounding to the figure's precision allowed, at most
  `harness.MaxFigureDenials` (2) times per prompt. The engine runs the same
  check on the final output for every backend and notes any amount it cannot
  trace in the report.
- Not used: SessionStart (a live process starts one session for many runs, so
  per-run context belongs in the prompt, where it already is), PostToolUse
  and Stop (the stream already records every tool result, and the output to
  verify is the StructuredOutput call, not the stop).

## Continuations

An agent with the `continue` action may ask to be woken again
(`after_minutes`, 1 to 1440) with a note to itself. When executed, it queues a
`continue` run with `run_after`; `runs_one_continuation` keeps one waiting
per agent, and a chain is bounded by `actions.MaxContinueChain` (12): the
run at the limit has its proposal denied, and `EnqueueContinuation` holds the
same bound. A long task becomes a chain of short checkpointed runs, each with
the session continued when the agent has one.

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
Today: `notify` (an extra message to one of the agent's channels),
`continue` (wake this agent again later, above) and `webhook:<endpoint>` (a JSON body to an endpoint whose URL and credentials
are configuration, never chosen by the model).

Approval happens from a signed link (`/ui/actions/{id}?sig=`), the API, or
`orbit approve`. An action is executed at most once: one interrupted during
execution is failed for a person to check, never retried.

## The API

`internal/api` serves two listeners: the public one traefik routes to, where
`/api/v1` takes authentik tokens verified by orbit (`internal/auth`) and
`/ui` sits behind forward-auth, and an internal one that also takes the
operator token. A conversation is read as one thread (messages and runs
interleaved, `Store.Thread`), and a run is watched as Server-Sent Events
(`/runs/{id}/stream`), which poll the stored transcript and turn it into
prose and tool calls in code (`runner.Narrate`). The stream reads only rows,
so it follows a run across a requeue, a deploy or another replica with no
signalling between writers and readers. Contract: docs/api.md.

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
  as `succeeded` without a backend call.

- **Tickets.** A request thread: a person files it, discusses it with an
  agent, approves a plan. Built on the existing messages, runs and actions.
- **A build agent.** A separate trust tier from read-only agents: a
  short-lived container per run with a checkout and write tools, no orbit
  secrets, a GitHub credential limited to listed repositories. It opens PRs;
  merging is an action (`github:merge`) whose executor refuses unless every
  required check is green, because in the fleet a merge is a deploy.
- **Event triggers** from webhooks (a ticket assigned, a comment).
- **A worker fleet.** Executors on several hosts, as jobtracker's are; the
  queue already allows it (SKIP LOCKED claims, `runs_one_running`,
  heartbeats, one scheduler by advisory lock). What changes: agents declare
  capabilities and workers advertise theirs, so a run is claimed only where
  its sources are reachable (finance needs hetzner's docker network); source
  credentials live only on hosts with the capability; delivery and actions
  stay on one control host; workers read agent specs from the database, not
  files; sessions are stored centrally so a conversation can continue on any
  host. One subscription is shared, so the fleet adds reach, not capacity.
- **A UI** over `/api/v1` (the thread and the stream are its read side).
