# HTTP API

Base path `/api/v1`. JSON in and out; errors are `{"error": "..."}` with a
4xx/5xx status. Times are RFC 3339.

## Authentication: authentik

Every request carries `Authorization: Bearer <token>`, where the token is one
of:

- **An authentik access token (JWT) for a signed-in person.** orbit verifies
  it against authentik's JWKS: signature, `iss`
  (`https://auth.kanishksachdev.com/application/o/<slug>/`), `exp`, `aud`
  in `ORBIT_OIDC_CLIENT_IDS`, and membership of `ORBIT_OIDC_GROUP` in the
  `groups` claim (today `infra-admins`). The person is the token's
  `preferred_username`; it is recorded as the author of their messages and
  decisions. A client forwards the token of the person it is acting for and
  refreshes it before it expires; orbit never sees a password or a session.
- **The operator token** (`ORBIT_API_TOKEN`), accepted on the tailnet
  listener only, for scripts and the CLI.

orbit is reached two ways:

- **Tailnet:** `http://100.79.4.41:8095`. Both token kinds, plus the approval
  and run pages under `/ui`.
- **Public:** `https://orbit-api.kanishksachdev.com`, for server-side callers
  off the tailnet (the portfolio on Vercel). Only `/api/v1`, and only
  authentik tokens. A 401 says which check failed.

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
  "pending_actions": 0
}]}
```

`unread_messages` counts the agent's open questions. `GET /agents/{name}`
adds `"memory": "<markdown>"`.

## A conversation

A conversation with an agent is its thread: the person's messages, and the
runs that answered them (and scheduled runs, which also speak). An agent's
reply is its run's `report`, plus any `questions` it asked.

`GET /agents/{name}/thread?before=<time>&limit=<n>` (newest last; page
backwards with `before` = the first item's `at`)

```json
{"items": [
  {"kind": "message", "id": "uuid", "at": "...", "from": "person",
   "body": "How much did I spend on Uber Eats?", "run_id": "uuid-of-run-that-read-it"},
  {"kind": "run", "id": "uuid", "at": "...", "trigger": "message",
   "status": "succeeded", "summary": "...", "report": "<markdown>",
   "questions": ["..."], "error": null, "cost_usd": 0.03,
   "actions": [{"id": "uuid", "type": "notify", "status": "pending", "reason": "...", "params": {}}]},
  {"kind": "message", "id": "uuid", "at": "...", "from": "agent", "body": "<a question>", "run_id": "uuid"}
]}
```

`from` is `person` or `agent`. A run's `status` is `queued`, `running`,
`succeeded`, `failed` or `cancelled`.

`POST /agents/{name}/messages`

```json
{"body": "text", "reply_to": "optional message id", "author": "kanishk", "wake": true}
```

returns `201 {"message_id": "uuid", "run_id": "uuid"}`. `run_id` is present
when the message woke a run (`wake`, default: the agent's `wake_on_message`).
Stream that run to show the reply as it is written.

`POST /agents/{name}/runs` `{"prompt": "optional"}` starts a run without a
message: `202 {"run_id": "uuid"}`.

## Streaming a run

`GET /runs/{id}/stream` is Server-Sent Events. Events, in order:

| event | data | meaning |
|---|---|---|
| `status` | `{"status": "queued"}` | sent first, then on every change |
| `text` | `{"text": "..."}` | the agent's visible reasoning or prose so far, appended |
| `tool` | `{"name": "Read", "summary": "inputs/transactions.json"}` | the agent used a tool |
| `done` | the run, as in a thread item | final; the stream ends |

The stream replays from the start of the run's latest attempt, so a client
that connects late (or reconnects) still gets everything. A run that is
requeued by a deploy streams `status: queued` again and continues. A
heartbeat comment (`: ping`) is sent every 15 s.

The final answer is `done.report`, not the concatenated `text`: `text` is
what the agent narrated while working.

## Runs, events, artifacts

- `GET /runs?agent=&status=&before=&limit=`
- `GET /runs/{id}`
- `POST /runs/{id}/cancel` (queued runs only; 409 otherwise)
- `GET /runs/{id}/events?attempt=&after=&limit=`: the backend's raw stream
- `GET /runs/{id}/artifacts`, `GET /artifacts/{id}`: what the run read and wrote
- `GET /runs/{id}/deliveries`

## Actions

- `GET /actions?agent=&status=pending`
- `GET /actions/{id}`
- `POST /actions/{id}/approve`, `POST /actions/{id}/reject` `{"by": "kanishk"}`:
  409 when it was already decided.

## Memory

`GET /agents/{name}/memory?limit=`: every version, newest first.
