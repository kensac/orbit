# HTTP API

Base path `/api/v1`. JSON in and out; errors are `{"error": "..."}` with a
4xx/5xx status. Times are RFC 3339.

## Authentication: authentik

Every request carries `Authorization: Bearer <token>`, where the token is one
of:

- **An authentik access token (JWT) for a signed-in person.** orbit verifies
  it against its issuer's JWKS (`internal/auth`): the signature (asymmetric
  algorithms only; `HS*` and `none` are refused), `iss` in
  `ORBIT_OIDC_ISSUER`, `exp` (required) and `nbf`/`iat` with 30 s of
  leeway, `aud` in `ORBIT_OIDC_CLIENT_IDS`, membership of `ORBIT_OIDC_GROUP`
  in the `groups` claim, and a non-empty `preferred_username`. The person is
  that `preferred_username`: the author of their messages and the
  `decided_by` of their decisions, whatever the request body says. A client
  forwards the token of the person it is acting for and refreshes it before
  it expires; orbit never sees a password or a session.
- **The operator token** (`ORBIT_API_TOKEN`), for scripts on the host.
  Accepted only on the internal listener (below), never on the one traefik
  routes to.

orbit is at `https://orbit.kanishksachdev.com`. The pages under `/ui` are
behind authentik forward-auth; `/api/` is routed past forward-auth, and the
JWT check is its gate (the portfolio on Vercel calls it server-side).

### Configuration

| variable | meaning |
|---|---|
| `ORBIT_OIDC_ISSUER` | comma list of accepted issuers, one per authentik provider (`https://auth.kanishksachdev.com/application/o/<slug>/`, trailing slash included). Each provider signs with its own key, so each issuer's keys come from its own discovery document (`<issuer>.well-known/openid-configuration`, then `jwks_uri`). Unset: the public `/api/v1` refuses every request. |
| `ORBIT_OIDC_CLIENT_IDS` | comma list of accepted `aud` values (the providers' client ids). Required with the issuer. |
| `ORBIT_OIDC_GROUP` | the group a person must be in (today `infra-admins`). Required with the issuer. |
| `ORBIT_LISTEN` | the public listener, default `:8080`. traefik routes here. |
| `ORBIT_DRAIN_MAX` | the longest a drain waits for the runs it holds before handing the rest back (a Go duration). Default: the longest agent `timeout` plus 5 minutes. |
| `ORBIT_INTERNAL_LISTEN` | the internal listener, default `127.0.0.1:8081`: `/api/v1` (operator token or authentik token), `/healthz`, `/metrics`; no `/ui`. traefik never routes to it, so the operator token is told apart by the socket a request arrived on, not by a header anyone can forge. The default is reachable only inside the container (`docker exec orbit ...`); bind `:8081` to let other containers on the network reach it. |

Keys are fetched on the first token, cached, and refetched when a token names
an unknown `kid` (a rotation), at most every 30 s per issuer, and at least
daily. Fetches time out after 10 s. If authentik is unreachable and no keys
are cached, the request is `503`, not `401`.

### Errors

A rejected token is `401` with `{"error": "<check>: <reason>", "check":
"<check>"}` and a matching `WWW-Authenticate` header. `check` is one of
`token` (missing, not a JWT, or an algorithm not accepted), `iss`, `kid`,
`signature`, `exp`, `nbf`, `iat`, `aud`, `groups`, `preferred_username`.

## Agents

`GET /agents`

```json
{"agents": [{
  "name": "finance",
  "spec": {"description": "Monthly spending review from Sure.", "...": "..."},
  "present": true, "enabled": true,
  "last_run": {"id": "uuid", "status": "succeeded", "trigger": "message",
               "created_at": "...", "finished_at": "...", "summary": "..."},
  "unread_messages": 1,
  "pending_actions": 0,
  "needs_input": true
}]}
```

`unread_messages` counts the agent's open questions, in all its
conversations. `needs_input` is true
while the agent waits on its person: an open question or a pending action.
`spec` is the agent's spec as loaded (the canonical JSON stored for
`spec_sha`, defaults applied).

`GET /agents/{name}` is `{"agent": <as in the list>, "memory": "<markdown>",
"next_fires": [...]}`. `next_fires` is as in `GET /health` (below); `[]` for
an agent with no schedule, a disabled one, or one no longer configured.
Earlier memory is `GET /agents/{name}/memory` (below).

## Conversations

An agent has as many conversations as its person opens ("New chat"), and
one **default conversation** (`is_default`, titled `General`) where its
scheduled and manual runs speak and which the agent-level routes
(`/agents/{name}/thread`, `/agents/{name}/messages`) read and write. Each
conversation is its own thread: its inbox, open questions, the conversation
a run is shown and the backend session a run continues are that
conversation's only. Memory, schedules and rules stay per agent. A
conversation is never deleted, only archived.

`GET /agents/{name}/conversations?archived=<bool>&before=<cursor>&limit=<n>`

```json
{"conversations": [{
  "id": "uuid", "agent": "finance", "title": "How much on Uber Eats in October?",
  "is_default": false, "archived": false,
  "created_at": "...", "last_activity_at": "...", "needs_input": true,
  "ticket_id": null}],
 "has_more": false, "next_before": null}
```

Newest activity first (a message or a run in it is activity). `archived`
(default `false`) lists the archived ones instead. Paging is as for a
thread: `next_before` is an opaque cursor, `null` exactly when `has_more`
is false; `limit` defaults to 50, at most 200. `needs_input` is true while
one of its questions is open or one of its runs' actions is pending.
`ticket_id` is set when the conversation is a ticket's (below).

`POST /agents/{name}/conversations`

```json
{"title": "optional", "body": "optional first message", "wake": true,
 "client_request_id": "optional, minted by the client"}
```

returns `201 {"conversation_id": "uuid", "message_id": "uuid", "run_id": "uuid"}`
(`message_id` with a `body`, `run_id` when it woke a run; `wake` defaults to
the agent's `wake_on_message`). Without a `title`, the first message names
it: its first non-empty line, Markdown markers and extra whitespace
removed, cut at a word to 60 characters (`store.DefaultTitle`, code, never
a model). `client_request_id` makes a retry return the first result, as for
messages below.

`GET /conversations/{id}` returns `{"conversation": {...}}`, the same shape.

`PATCH /conversations/{id}` `{"title": "...", "archived": true}` (either
field) returns the updated `{"conversation": {...}}`. A title is at most 240
characters (400 otherwise). The default conversation cannot be archived
(409). A message to an archived conversation brings it back.

`POST /conversations/{id}/messages` takes and returns exactly what
`POST /agents/{name}/messages` does (below), in that conversation.

## A conversation's thread

A thread is the person's messages, the agent's questions, and every run
(whatever its trigger) of one conversation. An agent's reply is its run's
`report`, plus any `questions` it asked.

`GET /conversations/{id}/thread?before=<cursor>&limit=<n>`, and
`GET /agents/{name}/thread?before=<cursor>&limit=<n>` for the default
conversation:

```json
{"items": [
  {"kind": "message", "id": "uuid", "at": "...", "from": "person",
   "body": "How much did I spend on Uber Eats?", "author": "kanishk",
   "run_id": "uuid-of-run-that-read-it"},
  {"kind": "run", "id": "uuid", "at": "...", "trigger": "message",
   "status": "succeeded", "attempt": 1,
   "started_at": "...", "finished_at": "...", "steps": 4,
   "summary": "...", "report": "<markdown>",
   "questions": [{"id": "uuid-of-the-question-message", "body": "...", "open": true}],
   "error": null, "cost_usd": 0.03, "gate": null,
   "actions": [{"id": "uuid", "type": "notify", "status": "pending", "reason": "...", "params": {}}],
   "needs_input": true},
  {"kind": "message", "id": "uuid-of-the-question-message", "at": "...", "from": "agent",
   "body": "<a question>", "run_id": "uuid", "open": true}
 ],
 "has_more": true,
 "next_before": "opaque-cursor"}
```

- **Order and paging.** Items are oldest first, totally ordered by (`at`, a
  run before a message at the same instant, `id`). A page is the newest
  `limit` items before `before`; omit `before` for the newest page. To page
  back, pass `next_before`. It is `null` exactly when `has_more` is false.
  The cursor is opaque, and no item is skipped or repeated across pages,
  even when items share a timestamp. `limit` defaults to 50; the maximum is
  200, and a larger value is clamped.
- **Messages.** `from` is `person` (to the agent) or `agent` (a question it
  asked). `author` is the person who wrote it (absent for questions).
  `reply_to`, when set, is the message it answers. `run_id` is the run that
  read a person's message (`null` until one starts) or the run that asked a
  question.
- **Runs.** `trigger` is `schedule`, `manual`, `message`, `continue` (the agent asked to be woken again) or `build` (a ticket's build run, docs/tickets.md). `status` is
  `queued`, `running`, `succeeded`, `failed`, `cancelled` or `skipped` (it
  ended without a backend call because a gate, code, found nothing to judge;
  `gate` names it, and is `null` for every other status). `attempt` counts
  claims (0 while never started; a run interrupted by a deploy is claimed
  again). `started_at` is the latest attempt's start, `null` while queued.
  `finished_at` is `null` until final. `steps` is the number of tool calls in
  the latest attempt, exactly the `tool` events its stream shows. `summary`,
  `report`, `error` and `cost_usd` are `null` when absent. `questions` and
  `actions` are always arrays.
- **Questions** appear twice by design: in their run's `questions`, and as
  `from: "agent"` message items with the same `id`. A client dedupes by
  `id`, and replies by sending a message with `reply_to` set to it, in the
  same conversation (a `reply_to` from another one is a 400). `open` is true
  until a message names the question in `reply_to` or any later message
  reaches its conversation (the next run there sees the person's words).
- **Waiting on the person.** `needs_input` on a run is true while one of its
  questions is open or one of its actions is `pending`: the run finished,
  and the agent is waiting for an answer or an approval. It is a state
  distinct from `status`, which says only whether the run itself is done.
  The agents list carries the same flag per agent.
- **Actions.** `status` is `pending` (waiting for a person), `approved`
  (by a person or by an `auto` rule, about to execute), `rejected` (by a
  person), `denied` (by rule or invalid; never executes), `executing`,
  `executed` or `failed`.

`POST /agents/{name}/messages`

```json
{"body": "text", "reply_to": "optional message id", "wake": true,
 "client_message_id": "optional, minted by the client"}
```

posts to the default conversation, and returns
`201 {"message_id": "uuid", "run_id": "uuid"}`. `run_id` is present
when the message woke a run (`wake`, default: the agent's `wake_on_message`).
Stream that run to show the reply as it is written.

**Retries.** With `client_message_id` (1 to 200 characters, unique per agent;
a UUID), a retry returns the first result, the same `message_id` and
`run_id`, and creates nothing. The message, its run and the receipt are one
transaction, so a lost response is safe to retry, and concurrent retries act
once. Reusing an id for a different request is `409`. A request that failed
leaves no receipt.

The author is the
token's person. Only with the operator token, an `"author"` field names it
(otherwise none is recorded).

`POST /agents/{name}/runs` `{"prompt": "optional"}` starts a run without a
message, in the default conversation: `202 {"run_id": "uuid"}`.
`GET /agents/{name}/messages` lists the default conversation's messages.

## Streaming a run

`GET /runs/{id}/stream` is Server-Sent Events. Events, in order:

| event | data | meaning |
|---|---|---|
| `status` | `{"status": "queued", "attempt": 0}` | sent first, then on every change |
| `text` | `{"text": "..."}` | a new chunk of the agent's prose, to append to what came before |
| `tool` | `{"name": "Read", "summary": "inputs/transactions.json"}` | the agent used a tool |
| `done` | the run, identical to a thread run item (`kind: "run"`) | final; the stream ends |

- **`text`** chunks concatenate into the attempt's prose. Each is one block
  of the agent's prose; every chunk after an attempt's first starts with a
  blank line (`"\n\n"`), so a client appends them verbatim.
- **`tool`** is one tool call; `summary` is one line (a file path relative to
  the run's directory, a pattern, a command, a URL), at most 160 characters,
  possibly empty. The call that returns the final answer is not shown.
- **Replay and resume.** Without `Last-Event-ID`, the stream replays the
  run's latest narration from its start (the latest attempt, preceded by
  the attempts it continues; below), then follows live, so a client that
  connects late gets everything. Every `text` and `tool` event has an `id`
  (`<attempt>.<seq>.<index>`); a client that reconnects with `Last-Event-ID`
  set to the last one it received gets only what followed it (a browser
  `EventSource` does this itself), from any instance: the stream is read
  from the database, so a reconnect that a deploy sends to the new instance
  continues seamlessly. An id outside the latest narration, or one that
  does not parse, means a full replay. `status` and `done` are sent again on
  every connection and carry no id.
- **Requeue.** A run handed back (a deploy's drain past its cap, a crash)
  streams `status: queued`, then `status: running` with the next `attempt`.
  When that status carries `"continues": <n>` and `n` is the attempt the
  client was showing, the new attempt resumed that attempt's session: the
  prose so far stands and the next `text` continues it (a chunk with its
  `"\n\n"`). Without `continues`, the prose and tools shown so far belong to
  an abandoned attempt: a client clears them, and the new attempt streams
  from its start.
- A heartbeat comment (`: ping`) is sent every 15 s. A finished run streams
  its status, its replay and `done` at once.
- The stream ends when the instance serving it exits (the end of a deploy's
  drain); reconnect with `Last-Event-ID`.

Events come from the backend's stored transcript (`GET /runs/{id}/events`),
turned into `text` and `tool` by code (`runner.Narrate`): Claude Code's text
blocks and tool calls; Codex's reasoning, messages, commands, searches and
file changes; the Messages API's text. The final answer is `done.report`, not
the concatenated `text`: `text` is what the agent narrated while working.

## Runs, events, artifacts

### A run

`GET /runs?agent=&status=&before=&limit=` is `{"runs": [<run>]}`, newest first. `before` is an RFC 3339 time (a run's `created_at`); `limit` defaults to 50, at most 500. `GET /runs/{id}` is `{"run": <run>, "attempts": [<attempt>]}`. A run (`store.Run`):

```json
{"id": "uuid", "attempt": 1, "agent": "finance", "spec_sha": "hex",
 "trigger": "schedule", "trigger_index": 0, "scheduled_for": "...",
 "prompt": "", "status": "succeeded",
 "runner_kind": "claude-code", "model": "...", "worker": "orbit/0",
 "created_at": "...", "started_at": "...", "finished_at": "...",
 "error": "...", "summary": "...", "report": "<markdown>",
 "output": {}, "usage": {}, "cost_usd": 0.03, "session_id": "...",
 "session_mode": "fresh", "session_parent": "uuid", "session_depth": 1,
 "run_after": "...", "continues": "uuid", "chain": 1,
 "gate": "unchanged", "steps": 4}
```

- Always present: `id`, `attempt`, `agent`, `spec_sha`, `trigger`, `prompt` (`""` when none), `status`, `created_at` and `steps`. Every other field is omitted when it has no value (not `null`).
- `trigger` and `status` are as in a thread run item. `trigger_index` and `scheduled_for` are set on a `schedule` run: which of the agent's triggers fired, and the fire time.
- `runner_kind`, `model` and `worker` are the latest attempt's backend, its model (absent when the runner names none) and the instance that claimed it. `spec_sha` is the spec the latest attempt ran under.
- `output` is the backend's raw structured output and `usage` its token usage, as it reported them. `cost_usd` is every attempt's cost together.
- `session_id` is the backend session the run ended in. `session_mode` (`fresh`, `resumed`, `live`), `session_parent` (the run whose session it continued) and `session_depth` are set for an agent with a session.
- `run_after`, `continues` (the run that asked) and `chain` (its place in the chain, from 1) are set on a `continue` run.
- `gate` names what ended a `skipped` run.
- `steps` is the number of tool calls in the latest attempt, counted as a thread's run item counts it.

An attempt (`store.Attempt`), oldest first:

```json
{"attempt": 2, "worker": "orbit/0", "runner_kind": "claude-code", "spec_sha": "hex",
 "started_at": "...", "ended_at": "...", "outcome": "succeeded",
 "session_id": "...", "recovered_from": 1, "recover_skipped": "...",
 "cost_usd": 0.01, "session_cost_usd": 0.04}
```

`attempt`, `worker` and `started_at` are always present; the rest is omitted when unset. `outcome` is `succeeded`, `failed`, `skipped`, `handed_back` (stopped by its instance) or `lost` (reaped without a heartbeat), absent while the attempt runs. `recovered_from` is the attempt whose session it continued, `recover_skipped` why it started over instead. `cost_usd` is the attempt's share, absent when unknown (a killed process saved none); `session_cost_usd` is the backend's running total for the session after it.

`POST /runs/{id}/cancel` is `200 {"cancelled": true}` for a queued run, 409 otherwise.

### Events

`GET /runs/{id}/events?attempt=&after=&limit=` is `{"events": [<event>]}`, one attempt's transcript in order: `attempt` (default: the run's latest), the events with `seq` above `after` (default 0), at most `limit` (default 1000, at most 5000). Page by passing the last `seq` as `after`.

```json
{"attempt": 1, "seq": 12, "at": "...", "kind": "assistant", "payload": {}}
```

`payload` is one line of the backend's stream as it sent it (a line that was not JSON is stored as a JSON string). `kind` is the line's `type`, with `.<subtype>` when it has one (`system.init`, `result.success`, `assistant`, `user`, `rate_limit_event`; Codex's `item.started`, `item.completed`, ...), `response` for the Messages API, and `raw` for a line without a type. orbit's own events are `orbit.*`: `orbit.source_fetched` and `orbit.source_failed`, `orbit.transform` and `orbit.transform_failed`, `orbit.gate`, `orbit.session`, `orbit.recovery`, `orbit.figures_unverified`, and a build's `orbit.build`, `orbit.git`, `orbit.sandbox`, `orbit.permission` and `orbit.permission_denials`.

### The journal

A run's journal is its typed record: what the agent said, thought, called and changed, as items a client renders without reading any backend's event format. It is derived from the events above by code (`internal/journal`), written in the transaction that stores each event, and backfilled for runs from before it existed with the same code, so the rows are the same either way. It is append-only: an item that changes (a tool call that returns, a plan that is updated, a turn that ends) gets a new revision carrying its whole body, and earlier revisions stay as they were.

**Addressing.** A row is `(run, epoch, seq)`. An epoch is one timeline: an attempt that resumed the interrupted attempt's session (`recovered_from`) continues its epoch, and an attempt that started over opens a new epoch numbered by itself. So a run handed back once and continued has one epoch, `1`; one whose second attempt started over has epochs `1` and `2`. `seq` counts rows within an epoch from 1, without gaps. A row's `cursor` is `"<epoch>.<seq>"`.

`GET /runs/{id}/journal?epoch=&after=&limit=` reads one epoch's rows (every revision) in `seq` order:

```json
{"epoch": 1, "epochs": [1],
 "items": [<row>],
 "cursor": "1.42", "has_more": false, "reset": null}
```

- `epoch` defaults to the epoch of the run's latest attempt (`0` while the run has never started, with no items). `epochs` lists every epoch the run has, oldest first.
- `after` is a cursor: rows after it are returned. A cursor into another epoch than the one read, or past its last row, is not continued: the response starts from the epoch's first row and `reset` says why (`epoch_changed`, `cursor_ahead`), and a client discards what it holds of that run. An `after` that is not a cursor is a 400.
- `limit` defaults to 500, at most 2000. `cursor` is the last returned row's (or `after` when none was returned); pass it as `after` for the next page. `has_more` is true when rows follow.

A row:

```json
{"run_id": "uuid", "epoch": 1, "seq": 7, "cursor": "1.7", "attempt": 1,
 "item_id": "tool:toolu_01", "revision": 2, "kind": "tool_call",
 "turn_id": "turn:1.1", "parent_id": null,
 "at": "...", "v": 1, "body": {}}
```

- `item_id` is stable across revisions; `revision` counts from 1. To show the current state, keep the highest revision per `item_id`, placed where its revision 1 was. `GET /runs/{id}/journal/items/{item_id}?epoch=` is `{"item": <row>}`, an item's latest revision (404 when the epoch has none).
- `turn_id` is the turn the item belongs to (a `turn` item's own id for the turn itself), `null` for orbit's steps outside any turn (fetching sources, planning a session).
- `parent_id` is the subagent nesting: an item produced inside a subagent names the `tool_call` item of the call that spawned it (Claude Code's `Task`/`Agent`), and is `null` at the top level. Nested subagents nest the same way.
- `at` is when orbit stored the event the row came from (the attempt's end for a turn closed by it). Order by `seq`, not `at`.
- `v` is the body's schema version, 1 today. A body changes shape only with a new version; a reader reads older versions in the current shape (`journal.Upcast`), and a row from a newer version is served as stored.
- `kind` is one of the kinds below, and may grow: a client shows a kind it does not know as nothing, never as an error.

**Bounded values.** A value that can be large (a tool's output, a diff, a thought, a status payload) is sent as

```json
{"head": "first bytes...", "bytes": 48213, "sha256": "hex", "truncated": true}
```

`head` is at most 4096 bytes (`journal.HeadMax`), cut at a character boundary; `bytes` and `sha256` are the whole value's. When `truncated` is true, `GET /runs/{id}/journal/bodies/{sha256}` returns the whole value as stored (`text/plain; charset=utf-8`, or `application/octet-stream` when it is not UTF-8), 404 for a digest no row of that run cut short. When `truncated` is false, `head` is the whole value.

**Kinds and bodies.** Ids are shown as patterns: `<a>.<s>.<i>` is the attempt, the source event's `seq` in its attempt, and the item's place among that event's items.

| `kind` | `item_id` | body |
|---|---|---|
| `message` | `message:<a>.<s>.<i>` (Codex: `message:codex.<id>`) | `{"role": "assistant", "text": "..."}`; `role` is `assistant` for the agent's prose, `user` for a user turn the backend recorded (the prompt a subagent was given). One per block of prose; `text` is whole. |
| `thinking` | `thinking:<a>.<s>.<i>` (Codex: `thinking:codex.<id>`) | `{"text": <bounded>, "tokens_estimate": 120, "redacted": true}`; the model's reasoning where the backend exposes it. `tokens_estimate` is the text's size in tokens, estimated from its bytes (4 per token), not a count the backend reported (a turn's `usage` has those). `redacted` (only when true) is a thought sent encrypted: no text, estimate 0. |
| `tool_call` | `tool:<call id>` (Claude Code's `tool_use` id; Codex: `tool:codex.<item id>`) | `{"call_id": "toolu_01", "name": "Read", "input": {...}, "summary": "inputs/a.json", "state": "running", "result": <bounded>}`. Revision 1 is the call, `state: "running"`, no `result`; the revision written when it returns has `state` `completed` or `failed` (the tool reported an error, a command exited non-zero) and `result`, the tool's output as text. `input` is the call's input, whole. `summary` is one line (a path relative to the run's directory, a pattern, a command, a URL), at most 160 characters, possibly empty. Codex's are `shell` (`{"command"}`), `web_search` (`{"query"}`), `<server>.<tool>` (`{"arguments"}`) and `edit` (`{"changes"}`). The call that returns the final answer is not a tool call. |
| `diff` | `diff:<call id>` (Codex: `diff:codex.<item id>.<n>`) | `{"call_id": "toolu_02", "path": "notes.md", "change": "edit", "patch": <bounded>}`; a change a file tool made, once it succeeded. `path` is relative to the run's directory. `change` is `edit` or `write` (Claude Code), or Codex's `add`, `delete`, `update`. `patch` is a unified diff of the replaced text (`--- a/<path>`, `+++ b/<path>`, one `@@` hunk per replacement, without line numbers, which Claude Code does not report); absent when the backend said only that the file changed (Codex). |
| `plan` | `plan` (one per epoch) | `{"entries": [{"text": "read the inputs", "status": "completed"}], "text": "..."}`; the agent's working plan. `entries` is its todo list (Claude Code's `TodoWrite`, Codex's `todo_list`), each `status` `pending`, `in_progress` or `completed`. `text` (when present) is a plan it presented (`ExitPlanMode`). Each update is a revision. |
| `approval` | `approval:<request id>` | `{"request_id": "...", "tool": "Bash", "input": {...}, "state": "pending", "message": "..."}`; a build's backend asking orbit's policy whether a tool call may run. Revision 1 is the request (`pending`); the decision is the next, `allowed` or `denied`, with orbit's `message`. Actions waiting for a person are a run's `actions`, not these. |
| `question` | `question:<a>.<s>.<i>` (a question tool's: `question:<call id>.<n>`) | `{"text": "Which card?", "header": "...", "options": [{"label": "...", "description": "..."}], "multi_select": true}`; something the agent asked its person: a question in its final answer (`options` empty), or a question tool's, with what it offered. The thread's question messages are the ones a person replies to. |
| `status` | `status:<a>.<s>.0` | `{"code": "orbit.source_fetched", "detail": <bounded>}`; something that happened to the run that is not the model's output: orbit's steps (`orbit.*`, as in Events), the backend's session start (`system.init`), a rate limit report (`rate_limit_event`), a line that was not JSON (`raw`). `detail` is the event's payload. |
| `turn` | `turn:<a>.<n>` | below |

**Turns.** A turn is one exchange with the backend, from the request to its result. It opens at the backend's first event (Claude Code's `system.init` or first message, Codex's `turn.started`, a Messages API response) and closes at its result; a live process answers several runs, one turn each.

```json
{"state": "completed", "outcome": "success",
 "started_at": "...", "completed_at": "...", "duration_ms": 4200,
 "cost_usd": 0.02, "usage": {}, "error": "..."}
```

- `state` is how the turn stands as orbit observed it: `running`; `completed` (the backend answered); `interrupted` (orbit saw its process end without a result: a timeout, a crash, a drain's hand-back); `unverifiable` (its attempt was lost, reaped without a heartbeat, and nothing observed how it ended).
- `outcome` is the backend's own verdict, kept apart from `state`: `success`, `error`, `max_turns`, `max_tokens`, `refused`, or another word a backend reports. It is absent when none was reported, which is never success: an `interrupted` or `unverifiable` turn has none, and a `completed` turn can have `error` (an API error ends a turn too).
- `completed_at` is absent while running and on an `unverifiable` turn. `duration_ms` and `cost_usd` are the backend's (Claude Code's result), `usage` its token counts, `error` its message when `outcome` is not `success`. Each is absent when not reported.
- A turn is revision 1 when it opens and one more revision when it ends; the closing revision of an interrupted or lost attempt is written in the transaction that ends the attempt, so a reader that sees the run final sees how its turn ended.

### Artifacts

`GET /runs/{id}/artifacts` is `{"artifacts": [<artifact>]}`, by attempt, direction and name, without content:

```json
{"id": "uuid", "run_id": "uuid", "attempt": 1, "direction": "in",
 "name": "inputs/transactions.json", "media_type": "application/json",
 "sha256": "hex", "bytes": 48213, "created_at": "..."}
```

`direction` is `in` (what the run read) or `out` (`output.json`, what the backend returned). The inputs the model was shown are under `inputs/` (`inputs/<run>-<attempt>/` for an agent with a session); a source only transforms read is under `sources/`; `memory.md`, `messages.json`, `conversation.md`, `system.md`, `prompt.md` and `schema.json` are the rest of what the run was given. `GET /artifacts/{id}` is the content itself, with its `media_type` as `Content-Type` (served with `nosniff` and a `default-src 'none'` CSP).

### Deliveries

`GET /runs/{id}/deliveries` is `{"deliveries": [<delivery>]}`, every notification attempt of the run, oldest first:

```json
{"id": 7, "run_id": "uuid", "action_id": "uuid", "channel": "phone",
 "purpose": "approval", "status": "failed", "error": "...", "attempted_at": "..."}
```

`purpose` is `report`, `failure`, `approval` (an action waiting for a person; `action_id` names it) or `action` (an approved action whose execution failed; `action_id` names it). `status` is `sent` or `failed`, with the channel's `error`. `run_id`, `action_id` and `error` are omitted when unset.

## Actions

`GET /actions?agent=&status=&run_id=&before=<cursor>&limit=<n>` is `{"actions": [<action>], "has_more": false, "next_before": null}`, newest first. `status` is as in a thread action; `run_id` narrows to one run's proposals (400 when it is not a UUID). Paging is as for a thread: `next_before` is an opaque cursor, `null` exactly when `has_more` is false; `limit` defaults to 100, at most 500. `GET /actions/{id}` is `{"action": <action>}`:

```json
{"id": "uuid", "run_id": "uuid", "agent": "finance", "type": "webhook:home-assistant",
 "params": {}, "reason": "...", "rule": "approve", "status": "executed",
 "decided_by": "kanishk", "decided_at": "...", "executed_at": "...",
 "result": {}, "error": "...", "created_at": "..."}
```

`rule` is the agent's rule for the type when it was proposed (`auto`, `approve`, `deny`). `decided_by` is the person, or `rule` for an action its rule approved or denied. `result` is what the executor returned; `error` is why it was denied (an unknown type, invalid params, a limit) or why it failed. `decided_by`, `decided_at`, `executed_at`, `result` and `error` are omitted when unset.

- `POST /actions/{id}/approve`, `POST /actions/{id}/reject`: `200 {"ok":
  true}`; 404 for an unknown action; 409 when it was already decided. With
  `{"client_request_id": "..."}` (same rules as `client_message_id`), a retry
  of a decision that succeeded returns `200 {"ok": true}` again instead of
  409. The
  decider is the token's person. Only with the operator token, `{"by":
  "name"}` names it (default `operator`).

## Tickets

A ticket is a conversation with a lifecycle ([tickets.md](tickets.md)):
`POST /tickets` (opens its conversation; returns `ticket_id`,
`conversation_id`, `message_id`, `run_id`), `GET /tickets`,
`GET /tickets/{id}` (the ticket, its plans and transitions; a ticket that
reached merging carries `merge_action_id`, its `github:merge` action),
`POST /tickets/{id}/plans/{version}/approve`, and
`POST /tickets/{id}/{abandon,retry,reopen}`. Its thread and messages are
its conversation's (`/conversations/{conversation_id}/thread`,
`/messages`); a message there reopens a `planned` ticket, and a `merged`,
`done` or `abandoned` one refuses messages (409). A ticket's conversation
carries `ticket_id` in the conversation list. Filing and approving take
`client_request_id`; the author and approver are the token's person. A
transition the ticket's state does not allow is 409.

## Memory

`GET /agents/{name}/memory?limit=`: the agent's memory versions, newest
first, `{"versions": [{"id": 12, "run_id": "uuid", "content": "<markdown>",
"created_at": "..."}]}`. `limit` defaults to 20; the maximum is 200.

## Memory entries

Beside each agent's memory document, orbit keeps **memory entries**: one durable fact each (a correction the person gave, a preference, a fact about their world, where something is), typed, versioned, and shared at one of three scopes:

- `profile`: the person; every agent reads it.
- `topic`: a domain such as `finance` or `homelab`; every agent that reads the topic (its spec's `topics`, or attached below).
- `agent`: one agent's own.

Every run of an agent is shown an index of the active entries it reads (`GET /agents/{name}/context`, below) and the entries in full as an input file (docs/ARCHITECTURE.md, Memory entries).

An entry:

```json
{"id": "uuid", "scope": "topic", "topic": "finance", "key": "transfers-are-not-spending",
 "version": 2, "status": "active",
 "type": "feedback", "title": "Transfers are not spending",
 "description": "one line for the index", "body": "<markdown>",
 "why": "...", "how_to_apply": "...",
 "superseded_by": null, "source": "person", "source_run_id": null,
 "source_conversation_id": null, "author": "kanishk", "note": "",
 "created_at": "...", "updated_at": "..."}
```

- `scope` with `topic` (scope `topic`) or `agent` (scope `agent`); neither for `profile`. Topic names are spec names (`^[a-z][a-z0-9-]{0,62}$`).
- `key` is a slug (`^[a-z0-9][a-z0-9-]{0,79}$`) unique within its scope, stable across versions; omitted on create, it is made from the title (`-2`, `-3`, ... when taken). A key once used stays the entry's, retired or not.
- `type` is one of Claude Code's memory types: `user` (who the person is), `feedback` (how they want work done), `project` (facts about their world and work), `reference` (where to find things).
- `title` (one line, at most 200 bytes) and `body` (at most 16 KB) are required; `description` is one line of at most 400 bytes; `why` and `how_to_apply` at most 4 KB each.
- `version` counts versions, the newest in force. `updated_at` is when it was written, `created_at` when the entry was. `status` is `active` or `retired`; `superseded_by` is the entry that replaced a retired one.
- `source` is who wrote this version: `person` (this API), `learned` (a learning run), `import` (`POST /memory/import`). `source_run_id` and `source_conversation_id` name what it was learned from. `author` is the token's person (with the operator token, the body's `author`, if any). `note` says why this version was written.

Nothing is deleted. Every change is a new version, kept:

- `GET /memory/entries?scope=&topic=&agent=&type=&status=&q=&limit=`: `{"entries": [...]}`, current versions, by scope (profile, topics, agents), then name, then most recently changed. `topic` or `agent` (not both) selects one place. `status` is `active` (default), `retired` or `all`; `q` matches key, title, description, body, why and how_to_apply, case-insensitively. `limit` defaults to 200, at most 1000.
- `POST /memory/entries` `{"scope", "topic"|"agent", "key"?, "type", "title", "description"?, "body", "why"?, "how_to_apply"?, "note"?}`: `201 {"entry": ...}`. An explicit `key` already used in the scope is 409; an `agent` that is not configured is 400. Unknown fields are 400.
- `GET /memory/entries/{id}`: `{"entry": <current>, "versions": [...]}`, every version newest first.
- `PATCH /memory/entries/{id}` `{"version"?, "type"?, "title"?, "description"?, "body"?, "why"?, "how_to_apply"?, "status"?, "note"?}`: the next version, with absent fields kept. `version` is the version the client read; another current one is 409, so two edits never silently overwrite each other (omit it to write regardless). `status: "active"` restores a retired entry (clearing `superseded_by`). An edit that changes nothing writes no version and returns the current one.
- `POST /memory/entries/{id}/retire` `{"version"?, "superseded_by"?, "note"?}`: the next version, retired. `superseded_by` must be another, active entry (400 otherwise). This is how an entry is deleted.
- `POST /memory/import` `{"dry_run": bool, "entries": [<entry as for create, key optional>], "note"?}`: matches each by scope and key. A new key creates an entry, a known one gets a new version when its content differs or it was retired, an identical one is left alone, so an import can be repeated. `{"dry_run", "results": [{"scope", "topic"|"agent", "key", "outcome": "created"|"updated"|"unchanged", "id", "version"}]}`, in request order. All of it is one transaction: one invalid entry fails the whole import (400, naming its index). `dry_run` reports and writes nothing (`id` is absent for an entry it would create). At most 500 entries. `orbit import claude-memory` calls this (deployment.md).
- `GET /memory/topics`: `{"topics": [{"name", "entries", "agents"}]}` by name: every topic that holds entries or is read by an agent. `entries` counts its active entries; `agents` are the agents that read it, from their spec or attached.

### What an agent reads

- `GET /agents/{name}/context`: `{"agent", "topics": [{"name", "from": ["spec"|"api"]}], "entries": <count>, "index": "<markdown>"}`. `index` is exactly what a run's prompt carries (with `inputs/_memory.md` for the run's own inputs path), `""` when the agent reads no entries.
- `PUT /agents/{name}/topics/{topic}`: attach a topic, `{"attached": true, "created": bool}` (`created` false when it was attached already). `DELETE /agents/{name}/topics/{topic}`: detach, `{"detached": true}`, 404 when not attached through the API (a topic the spec names stays until the spec changes). The agent must be configured (404).

## MCP

`/mcp` is an MCP server (Streamable HTTP, stateless) on both listeners, so the Claude apps and Claude Code can talk to orbit's agents. It takes what `/api/v1` takes on the same listener: an authentik access token (verified as above; the person is the author and decider), and on the internal listener the operator token. Without a valid token it answers `401` with `WWW-Authenticate: Bearer resource_metadata="<public_url>/.well-known/oauth-protected-resource/mcp"`. Every request stands alone (no `Mcp-Session-Id`), so a call may land on either instance of a rollout.

| tool | arguments | returns |
|---|---|---|
| `list_agents` | none | each configured agent: `name`, `description`, `enabled`, `needs_input`, `open_questions`, `pending_actions`, `last_run` |
| `list_conversations` | `agent`, `archived`, `limit` | as `GET /agents/{name}/conversations` |
| `create_conversation` | `agent`, `title`, `message`, `wait_seconds` | `conversation_id`, `message_id`, `run_id`, and `reply` as `send_message` |
| `send_message` | `body`, and `agent` (its default conversation) or `conversation_id`; `reply_to`, `wait_seconds` | `message_id`, `run_id`, `reply`: the run as a thread run item (`report`, `questions`, `actions`, `status`) |
| `wait_for_reply` | `run_id`, `wait_seconds` | `reply`, the run as a thread run item |
| `read_thread` | `agent` or `conversation_id`, `limit` (default 20), `before` | `items`, `has_more`, `next_before`, as a thread page |
| `list_actions` | `agent`, `status` (default `pending`; `all`) | `actions` |
| `approve_action`, `reject_action` | `action_id` | the decided `action`; a decided one is an error |

- **Waiting.** `send_message` and `create_conversation` wait for the run they woke up to `wait_seconds` (default 45, at most 300; `-1` returns at once). A run still going when the wait ends is returned with its status and a `note` to call `wait_for_reply`. A message that woke no run (`wake_on_message` off) says so in `note`.
- **Streaming.** When the call carries a `progressToken`, the wait sends the run's narration (the stream's `text` and `tool` events, a tool as `<name> <summary>`) as progress notifications on the call's response.
- Results are one text content block of indented JSON. A tool error (an unknown agent, a missing field, a store conflict) is a tool result with `isError`.

### Signing in: orbit's OAuth proxy

authentik has no dynamic client registration, which the Claude apps require, so orbit is the authorization server an MCP client sees, and authentik signs the person in under orbit's own client. Nothing is stored: client ids, the state sent through authentik, codes and refresh tokens are sealed (AES-GCM) under a key derived from `ORBIT_API_TOKEN`, so either instance can finish a flow and rotating that token ends every registration and refresh token (a connector is then re-added).

| path | |
|---|---|
| `GET /.well-known/oauth-protected-resource`, `.../oauth-protected-resource/mcp` | RFC 9728: `resource` `<public_url>/mcp`, `authorization_servers` `[<public_url>]` |
| `GET /.well-known/oauth-authorization-server` (and `.../mcp`) | RFC 8414: issuer `<public_url>`, the endpoints below, `S256` only, `token_endpoint_auth_methods_supported: ["none"]` |
| `POST /oauth/register` | RFC 7591: any number of `redirect_uris`, each `https` on an allowed host or `http(s)` on a loopback address; `201` with a `client_id` (public client, no secret) |
| `GET /oauth/authorize` | `response_type=code`, `client_id`, a registered `redirect_uri`, `code_challenge` with `S256` (required), `state`; redirects to authentik with orbit's client, scope `openid profile email offline_access` |
| `GET /oauth/callback` | authentik's redirect: exchanges its code, checks the access token as `/mcp` would (so a person outside the group fails here, with the reason), and redirects to the client with a code (valid 2 minutes, once) and `iss` |
| `POST /oauth/token` | `authorization_code` (with `code_verifier`, the same `client_id` and `redirect_uri`) or `refresh_token`; returns authentik's access token and orbit's sealed refresh token, which refreshes through authentik |

The access token is authentik's own (the `orbit-mcp` provider), so its issuer and client id are accepted wherever an authentik token is.

| variable | meaning |
|---|---|
| `ORBIT_MCP_OAUTH_ISSUER` | the authentik provider's issuer, `https://auth.kanishksachdev.com/application/o/orbit-mcp/`. Unset: no OAuth endpoints, and `/mcp` takes only a token a client already has. Added to `ORBIT_OIDC_ISSUER`'s list. |
| `ORBIT_MCP_OAUTH_CLIENT_ID` | that provider's client id; added to `ORBIT_OIDC_CLIENT_IDS`. |
| `ORBIT_MCP_OAUTH_CLIENT_SECRET` | that provider's client secret (a confidential client). |
| `ORBIT_MCP_REDIRECT_HOSTS` | comma list of hosts a client may redirect to (the host or a subdomain of it), default `claude.ai,claude.com`. Loopback is always allowed. |

`ORBIT_OIDC_GROUP` is required with the issuer. The authentik provider needs the redirect URI `<public_url>/oauth/callback` (strict), the `openid`, `email`, `profile` and `offline_access` scope mappings (profile carries `preferred_username` and `groups`), the `authorization_code` and `refresh_token` grants, and the application bound to the group. traefik must route `/mcp`, `/.well-known/oauth-` and `/oauth/` past forward-auth, as it does `/api/`.

## Usage

`GET /usage?from=YYYY-MM-DD&to=YYYY-MM-DD`

```json
{"timezone": "America/New_York",
 "from": "2026-10-03", "to": "2026-11-01",
 "subscription": {
   "windows": [
     {"window": "seven_day", "status": "allowed", "utilization": 0.14,
      "resets_at": "2026-11-08T12:00:00Z", "expired": false,
      "observed_at": "...", "run_id": "uuid"}],
   "overage": {"status": "rejected", "resets_at": null,
               "disabled_reason": "org_level_disabled", "in_use": false,
               "observed_at": "...", "run_id": "uuid"}},
 "cost": {
   "total": {"api_equivalent_usd": 1.234567, "runs": 42, "runs_with_cost": 40,
             "by_status": {"queued": 0, "running": 1, "succeeded": 38, "failed": 3, "cancelled": 0, "skipped": 0}},
   "by_agent": [{"agent": "finance", "api_equivalent_usd": 0.811000, "runs": 30,
                 "runs_with_cost": 29, "by_status": {"...": 0}}],
   "by_day": [{"date": "2026-10-03", "api_equivalent_usd": 0.000000, "runs": 0}]}}
```

- **Subscription windows are what Claude Code last reported, not a live
  query.** The CLI writes a `rate_limit_event` into a run's stream when its
  rate-limit information changes; orbit stores it in the run's events
  (`GET /runs/{id}/events`), and this reads the newest one per window from
  runs created in the last 8 days (`store.RateLimitLookback`: the longest
  window, `seven_day`, and a day). No Claude Code run, no observation: a
  window absent from the list was not reported in that time.
- `window` is the CLI's name for it (`five_hour`, `seven_day`,
  `seven_day_opus`, `seven_day_sonnet`, ...). `status` is `allowed`,
  `allowed_warning` or `rejected`. `utilization` is the fraction of the
  window used, 0 to 1 (0.14 is 14%), or `null` when the CLI did not say.
  `resets_at` is when the window resets, `null` when not reported.
  `expired` is true once `resets_at` has passed: the window has reset since
  the observation, so its `utilization` is out of date. `observed_at` is when
  orbit stored the event and `run_id` the run whose stream carried it.
- The CLI's line names one window at top level (`rateLimitType`) and lists
  windows in `unifiedWindows`; each window is taken from the newest event
  that mentions it, and within one event the `unifiedWindows` entry wins.
- `overage` is the overage state of the newest event that carries one
  (`overageStatus`, `overageResetsAt`, `overageDisabledReason`,
  `isUsingOverage`), or `null`. Fields the CLI did not send are `null`.
- **Cost is the CLI's API-equivalent estimate, not a bill.** It is each
  run's `cost_usd`: what the run's tokens would cost at API prices, as the
  backend reported it. On a subscription nothing is charged per run.
  Sums are exact decimal arithmetic in Postgres, six places, sent as a JSON
  number with exactly those digits. `runs_with_cost` counts the runs that
  reported a cost; a run without one (queued, cancelled, failed before the
  backend answered, or a backend that reports none) adds nothing.
- **The range** is calendar days in the deployment's timezone (`timezone`
  in `orbit.yaml`, `UTC` when unset), `from` and `to` inclusive, by each
  run's `created_at`. Defaults: `to` is today, `from` 29 days before `to`
  (30 days). At most 366 days; `from` after `to`, a longer range or a date
  that is not `YYYY-MM-DD` is `400`. `by_day` lists every day of the range,
  oldest first, zero or not. `by_agent` lists agents with a run in the range,
  by name. `by_status` always has all six statuses.

## Readiness and drain

- `GET /readyz` (both listeners, unauthenticated, like `/healthz`): `200
  ready` while the instance takes new work; `503 draining` while it drains,
  `503 database: ...` when it cannot reach Postgres. `/healthz` is liveness:
  200 while the process serves, draining or not. traefik routes by
  `/readyz`.
- `POST /api/v1/drain` (internal listener only, operator token): start
  draining, as SIGTERM does. `202 {"draining": true, "started": true,
  "since": "..."}`; `started` is false when it was already draining. `orbit
  drain` in the container calls it.

## Health

`GET /health` (authenticated, unlike `/healthz`)

```json
{"checked_at": "...",
 "config": {"valid": true, "loaded_at": "...",
            "last_error": null, "last_error_at": null},
 "queue": {"due": 0, "waiting_continuations": 1, "oldest_due_at": null},
 "running": [{"run_id": "uuid", "agent": "finance", "worker": "orbit/0",
              "attempt": 1, "started_at": "...", "heartbeat_at": "...",
              "heartbeat_age_seconds": 12.4}],
 "pending_actions": 2,
 "agents": [{"name": "finance", "enabled": true, "timezone": "America/New_York",
             "last_success": {"run_id": "uuid", "finished_at": "..."},
             "next_fires": [{"trigger_index": 0, "schedule": "0 8 * * *",
                             "at": "2026-11-02T13:00:00Z"}]}]}
```

- `config.valid` is false while the newest configuration on disk fails to
  load; the previous one stays in force (the `orbit_config_valid` gauge).
  `loaded_at` is when the configuration in force was loaded. `last_error`
  and `last_error_at` are the most recent load failure since the process
  started, kept after a later load succeeds (then `last_error_at` is before
  `loaded_at`); `null` when none failed.
- `queue.due` counts queued runs an executor may claim now (behind a running
  run of the same agent, too); `waiting_continuations` counts `continue` runs
  whose `run_after` is still ahead. `oldest_due_at` is the oldest due run's
  `created_at`.
- `running` lists running runs, oldest first. `heartbeat_age_seconds` is
  measured by the database clock (`checked_at`); a run is requeued past
  `engine.StaleAfter` (300 s).
- `agents` is every agent in the configuration in force, by name.
  `last_success` is its newest succeeded or skipped run (a gate finding
  nothing to judge is not a stale agent), `null` if none.
  `next_fires` is each schedule trigger's next fire after now, read in the
  agent's timezone, soonest first, in UTC; `[]` for a disabled agent or one
  without triggers. A fire is enqueued by the scheduler's leader, so it is
  when the run is due, not when it starts.
