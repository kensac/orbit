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

`unread_messages` counts the agent's open questions. `GET /agents/{name}`
adds `"memory": "<markdown>"`. `needs_input` is true while the agent waits
on its person: an open question or a pending action.

## A conversation

A conversation with an agent is its thread: the person's messages, the
agent's questions, and every run (whatever its trigger; scheduled runs also
speak). An agent's reply is its run's `report`, plus any `questions` it
asked.

`GET /agents/{name}/thread?before=<cursor>&limit=<n>`

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
   "error": null, "cost_usd": 0.03,
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
- **Runs.** `trigger` is `schedule`, `manual`, `message` or `continue` (the
  agent asked to be woken again). `status` is
  `queued`, `running`, `succeeded`, `failed` or `cancelled`. `attempt` counts
  claims (0 while never started; a run interrupted by a deploy is claimed
  again). `started_at` is the latest attempt's start, `null` while queued.
  `finished_at` is `null` until final. `steps` is the number of tool calls in
  the latest attempt, exactly the `tool` events its stream shows. `summary`,
  `report`, `error` and `cost_usd` are `null` when absent. `questions` and
  `actions` are always arrays.
- **Questions** appear twice by design: in their run's `questions`, and as
  `from: "agent"` message items with the same `id`. A client dedupes by
  `id`, and replies by sending a message with `reply_to` set to it. `open`
  is true until a message names the question in `reply_to` or any later
  message reaches the agent (the next run sees the person's words).
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

returns `201 {"message_id": "uuid", "run_id": "uuid"}`. `run_id` is present
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
message: `202 {"run_id": "uuid"}`.

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
- **Replay and resume.** Without `Last-Event-ID`, the stream replays from
  the start of the run's latest attempt, then follows live, so a client that
  connects late gets everything. Every `text` and `tool` event has an `id`
  (`<attempt>.<seq>.<index>`); a client that reconnects with `Last-Event-ID`
  set to the last one it received gets only what followed it (a browser
  `EventSource` does this itself). An id from an earlier attempt, or one
  that does not parse, means a full replay of the latest attempt. `status`
  and `done` are sent again on every connection and carry no id.
- **Requeue.** A run requeued by a deploy streams `status: queued`, then
  `status: running` with the next `attempt`. When `attempt` changes, the
  prose and tools shown so far belong to an abandoned attempt: a client
  clears them, and the new attempt streams from its start.
- A heartbeat comment (`: ping`) is sent every 15 s. A finished run streams
  its status, its replay and `done` at once.
- The stream also ends when orbit shuts down (a deploy); reconnect.

Events come from the backend's stored transcript (`GET /runs/{id}/events`),
turned into `text` and `tool` by code (`runner.Narrate`): Claude Code's text
blocks and tool calls; Codex's reasoning, messages, commands, searches and
file changes; the Messages API's text. The final answer is `done.report`, not
the concatenated `text`: `text` is what the agent narrated while working.

## Runs, events, artifacts

- `GET /runs?agent=&status=&before=&limit=`
- `GET /runs/{id}`: also `session_mode` (`fresh`, `resumed`, `live`),
  `session_parent` and `session_depth` for an agent with a session, and
  `run_after`, `chain` and `continues` for a `continue` run.
- `POST /runs/{id}/cancel`: `200 {"cancelled": true}`; queued runs only, 409
  otherwise
- `GET /runs/{id}/events?attempt=&after=&limit=`: the backend's raw stream
- `GET /runs/{id}/artifacts`, `GET /artifacts/{id}`: what the run read and wrote
- `GET /runs/{id}/deliveries`

## Actions

- `GET /actions?agent=&status=pending`: `status` as in a thread action
- `GET /actions/{id}`
- `POST /actions/{id}/approve`, `POST /actions/{id}/reject`: `200 {"ok":
  true}`; 404 for an unknown action; 409 when it was already decided. With
  `{"client_request_id": "..."}` (same rules as `client_message_id`), a retry
  of a decision that succeeded returns `200 {"ok": true}` again instead of
  409. The
  decider is the token's person. Only with the operator token, `{"by":
  "name"}` names it (default `operator`).

## Tickets

A ticket is a request or question with its own thread
([tickets.md](tickets.md)): `POST /tickets`, `GET /tickets`,
`GET /tickets/{id}` (the ticket, its plans and transitions),
`GET /tickets/{id}/thread` (paged exactly as an agent's thread, same items
and cursor), `POST /tickets/{id}/messages`,
`POST /tickets/{id}/plans/{version}/approve`, and
`POST /tickets/{id}/{abandon,retry,reopen}`. Bodies and shapes are in
tickets.md. As for messages and decisions, the author and approver are the
token's person (body `author` and `by` count only with the operator token),
and filing takes `client_request_id`, posting `client_message_id` and
approving `client_request_id` with the same retry rules. A transition the
ticket's state does not allow is 409.

An agent's thread (`GET /agents/{name}/thread`) is its general thread: it
leaves out ticket messages and runs, which are in their ticket's thread.

## Memory

`GET /agents/{name}/memory?limit=`: every version, newest first.
