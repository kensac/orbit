# HTTP API

Base path `/api/v1`. Every request needs `Authorization: Bearer <token>`.
JSON in and out; errors are `{"error": "..."}` with a 4xx/5xx status. Times
are RFC 3339.

orbit is reached two ways:

- **Tailnet:** `http://100.79.4.41:8095`, with `ORBIT_API_TOKEN`. Everything
  below, plus the approval and run pages under `/ui`.
- **Public:** `https://orbit-api.kanishksachdev.com`, for server-side callers
  that cannot join the tailnet (the portfolio on Vercel). Only `/api/v1`, and
  only with a client token (`ORBIT_CLIENT_TOKENS`). The caller authenticates
  its own user; orbit trusts the client. Never call it from a browser: the
  token would ship to every visitor.

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
