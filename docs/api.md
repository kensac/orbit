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

`unread_messages` counts the agent's open questions. `needs_input` is true
while the agent waits on its person: an open question or a pending action.
`spec` is the agent's spec as loaded (the canonical JSON stored for
`spec_sha`, defaults applied).

`GET /agents/{name}` is `{"agent": <as in the list>, "memory": "<markdown>",
"next_fires": [...]}`. `next_fires` is as in `GET /health` (below); `[]` for
an agent with no schedule, a disabled one, or one no longer configured.
Earlier memory is `GET /agents/{name}/memory` (below).

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

## Memory

`GET /agents/{name}/memory?limit=`: the agent's memory versions, newest
first, `{"versions": [{"id": 12, "run_id": "uuid", "content": "<markdown>",
"created_at": "..."}]}`. `limit` defaults to 20; the maximum is 200.

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
             "by_status": {"queued": 0, "running": 1, "succeeded": 38, "failed": 3, "cancelled": 0}},
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
  by name. `by_status` always has all five statuses.

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
  `last_success` is its newest succeeded run, `null` if none.
  `next_fires` is each schedule trigger's next fire after now, read in the
  agent's timezone, soonest first, in UTC; `[]` for a disabled agent or one
  without triggers. A fire is enqueued by the scheduler's leader, so it is
  when the run is due, not when it starts.
