# HTTP API

Base path `/api/v1`. JSON in and out; errors are `{"error": "..."}` with a
4xx/5xx status. Times are RFC 3339.

Every JSON request body is decoded strictly: a field the endpoint does not read is a `400` naming it (`json: unknown field "x"`), as is a body that is not one JSON value, is malformed, or exceeds the endpoint's size limit. An endpoint whose body is optional treats an empty body as `{}`: `POST /agents/{name}/runs`, `POST /agents/{name}/conversations`, `POST /actions/{id}/approve` and `/reject`, `POST /tickets` and every `POST /tickets/{id}/...` (transitions, comments, review), `POST /tasks/{id}/cancel`, `POST /memory/entries/{id}/retire` and `POST /skills/{name}/retire`. Any other empty body is a `400`. `POST /oauth/register` is the exception: RFC 7591 clients send metadata orbit does not use, so it ignores unknown fields.

An integer query parameter (`limit`, `after`, `attempt`, `offset`) that is not an integer is a `400` (`limit: want an integer`); an absent one takes its default.

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
  "spec_sha": "...",
  "spec": {"description": "Monthly spending review from Sure.", "...": "..."},
  "present": true, "enabled": true,
  "first_seen": "...", "updated_at": "...",
  "origin": "file",
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
`spec_sha`, defaults applied). `first_seen` is when orbit first stored the agent, `updated_at` when its stored spec, `enabled` or `present` last changed. `origin` and `version` are as in Defining agents (below); both are omitted for an agent no longer configured, and `version` for one not defined in the database. `last_run` is omitted before an agent's first run.

`GET /agents/{name}` is `{"agent": <as in the list>, "memory": "<markdown>",
"next_fires": [...]}`. `next_fires` is as in `GET /health` (below); `[]` for
an agent with no schedule, a disabled one, or one no longer configured.
Earlier memory is `GET /agents/{name}/memory` (below).

## Defining agents

An agent is defined in one of three places, reported as `origin` on every
agent row (`GET /agents`, `GET /agents/{name}`), with `version` for a
database agent:

- `file`: `agents/<name>.yaml` in the configuration directory. Read-only
  here: every change below is `409`.
- `database`: defined through this API (`agent_definitions`). Every change
  is a new version; nothing is overwritten or deleted.
- `builtin`: defined in code (`spec.Builtins`): `orbit`, the general agent.
  A file or database agent of the same name replaces it; the first `PUT
  /agents/orbit` defines it in the database.

A spec is the agent as JSON, with exactly the fields of an agent file
(docs/agents/configuration.md; durations as strings, `"15m"`), and is
validated by loading the whole configuration with it, as it will run: an
unknown field, a bad cross-reference or a name a file agent has is a `400`
with every problem in `error`. A change is in force on this instance when
the response returns, and on every other instance within 30 s.

`POST /agents`

```json
{"spec": {"name": "news", "description": "Morning headlines.",
          "charter": "Summarize the news I care about.",
          "triggers": [{"schedule": "0 8 * * *"}], "wake_on_message": true},
 "dry_run": false}
```

returns `201`:

```json
{"valid": true,
 "spec": {"name": "news", "...": "the agent as loaded, defaults applied"},
 "definition": {"id": 7, "name": "news", "version": 1, "spec": {"...": "as stored"},
                "enabled": true, "archived": false, "change": "create",
                "author": "kanishk", "created_at": "...",
                "run_id": null, "edited_by": "kanishk"},
 "next_fires": [{"trigger_index": 0, "schedule": "0 8 * * *", "at": "..."}],
 "agent": {"name": "news", "origin": "database", "version": 1, "...": "as in GET /agents"}}
```

- `dry_run: true` validates and stores nothing: `200 {"valid": true,
  "spec": ..., "next_fires": [...]}` (no `definition`, no `agent`).
- `spec.enabled` (default `true`) sets whether its schedules fire; it is
  stored on the definition, not in `spec`.
- A name already defined in the database (and not archived) is `409`; an
  archived name may be created again, as its next version.
- `author` is the token's person (`null` with the operator token). For a change an agent made (`agent:define`, below), it is the person whose message the agent was answering (`null` for an unattended run).
- `run_id` is the run of the agent that made the change, `null` for a change made through this API. `edited_by` names the editor: the agent's name when `run_id` is set, else `author`, else `operator`.
- `change` is `create`, `update`, `enable`, `disable`, `archive` or `revert`; a revert also carries `reverts`, the version it restored.

`PUT /agents/{name}` `{"spec": {...}, "dry_run": false}` replaces the spec
with a new version: `200`, the same shape (`change: "update"`). `spec.name`
may be omitted; if present it must be `{name}`. Omitting `spec.enabled`
keeps the current state.

`POST /agents/{name}/enable`, `/disable`, `/archive`: a new version with
the same spec, `200` in the same shape. An archived agent is no longer
configured (`present: false` in the list); its runs and conversations stay
readable. Changing an archived agent is `404`; a file or built-in agent is
`409`.

`POST /agents/{name}/definitions/{version}/revert` puts an earlier version back: a new version with that version's spec and enabled state (`change: "revert"`, `reverts: <version>`), `200` in the same shape. A version that archived the agent cannot be reverted to (`400`); an archived agent is `404`.

A database or built-in agent's spec may not have a `file` source, a `${VAR}` that is not in orbit.yaml's `credentials`, without a `sandbox`, a tool outside `Read`, `Glob`, `Grep`, `WebSearch`, `WebFetch`, `Task` and `TodoWrite`, a precheck or a `run` hook: each is a `400` naming it (docs/agents/configuration.md, Agents in the database).

**Agents change agents.** An agent whose rules enable `agent:define` (the built-in `orbit` does, `auto`) proposes `{"spec": {...}}`: a whole agent, as `POST /agents` takes it, for a new agent or one already defined in the database or built in, itself included. Executed, it goes through the same validation and is stored as the next version with the run as `run_id`. A file agent cannot be changed this way (the action fails, naming why). A change that widens the agent compared with its current version (a tool, source, credential, delegate target or need it adds, a source it changes, a move to a sandbox profile, an action type allowed more readily; for a new agent, any of these) is recorded with `rule: "approve"` and waits as `pending` for a person, whatever the agent's rule; one executing by rule that widens the agent as it is by then fails, naming what it widens.

`GET /agents/{name}/definitions` is `{"name": "news", "origin":
"database", "versions": [<definition>, ...]}`, newest first (empty for a
file or built-in agent).

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
`ticket_id` is set when the conversation is a ticket's (below), `task_id` when it is a delegated task's own (Delegation, below); a task conversation's open questions are to the delegating agent, so they do not set `needs_input`.

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

`POST /conversations` takes the same body plus `agent`, and creates the conversation with that agent, or with the built-in general agent `orbit` when `agent` is absent: what "New chat" calls. 404 when the agent is not configured.

`GET /conversations/{id}` returns `{"conversation": {...}}`, the same shape.

`GET /conversations/{id}/files` lists a chat agent's workspace for the conversation (below, Chat agents) as of its newest snapshot, stored after every run:

```json
{"files": [{"path": "src/main.go", "bytes": 13, "modified_at": "..."}],
 "snapshot": {"id": 4, "conversation_id": "uuid", "run_id": "uuid", "attempt": 1,
              "sha256": "hex", "bytes": 10240, "files": 3, "created_at": "..."}}
```

Regular files only, by path; orbit's own state (`.orbit/`, the run's `inputs/`) is left out. `snapshot` is `null` (and `files` empty) for a conversation without one. `GET /conversations/{id}/files/{path}` is one file, always as a download (`application/octet-stream`, `Content-Disposition: attachment`, `nosniff`, a CSP that runs nothing), 404 for a path the snapshot does not have.

`PATCH /conversations/{id}` `{"title": "...", "archived": true}` (either
field) returns the updated `{"conversation": {...}}`. A title is at most 240
characters (400 otherwise). The default conversation cannot be archived
(409). A message to an archived conversation brings it back.

`POST /conversations/{id}/messages` takes and returns exactly what
`POST /agents/{name}/messages` does (below), in that conversation.

### Conversation permissions

A conversation has a permission level for the runs its person's messages start (`trigger` `message`): one of `locked`, `ask`, `edits`, `auto`, `full`, narrowest first (docs/agents/configuration.md, Permissions), never wider than its agent's `ceiling`. Without one of its own it is at its agent's `level`. Every other run (schedule, manual, continue, task, delegation) is at the agent's level, or the conversation's when that is narrower: an unattended run never takes a raised level.

`GET /conversations/{id}/permissions`

```json
{"permissions": {
  "level": "edits", "source": "conversation",
  "agent_level": "ask", "ceiling": "full",
  "levels": ["locked", "ask", "edits", "auto", "full"],
  "current": {"version": 3, "level": "edits", "set_by": "kanishk", "set_at": "...", "expires_at": "..."},
  "history": [{"version": 3, "level": "edits", "set_by": "kanishk", "set_at": "...", "expires_at": "..."}]}}
```

- `level` is what the next run its person's message starts runs at; `source` is `conversation` when `current` sets it, else `agent` (no version, the newest is `level: ""`, or it expired).
- `levels` are the levels a `PUT` may set: those up to `ceiling`. A report agent's is `["locked"]`.
- `current` is the newest version, in force or not, `null` for none; `history` is every version, newest first, at most 50. A version is never changed: each `PUT` adds one. `level: ""` is a version that returned the conversation to its agent's own. `expires_at` is `null` for one that does not expire.

`PUT /conversations/{id}/permissions` `{"level": "edits", "expires_in": "1h"}` adds a version and returns `200` with the same `{"permissions": ...}` (without `history`).

- `level` is required: a level name, or `null` for the agent's own. One that is not a level, or above the agent's `ceiling`, is `400`.
- `expires_in` (optional) is a duration (`30m`, `1h`, `24h`), at most `168h`; after it the conversation is at its agent's level again. Not allowed with `null` (`400`).
- The person is the token's; only with the operator token, `"by"` names it (default `operator`).
- A reply being written takes the change within 2 s: orbit moves its Claude Code process with `set_permission_mode`, without restarting it, so a lowering holds from the next tool call. Raising works the same way for a run its person's message started; an unattended run takes only a lowering. A live process waiting between messages is moved before it takes the next run. Each application is a `permissions` item in the run's journal.

## Uploads

Files a person gives an agent, as Claude Code takes a file: uploaded into a conversation, then carried by a message. The run that reads the message finds each file in its working directory at `uploads/<name>` and is told so in its prompt (`attached: uploads/<name> (<type>, <n> bytes)`); an image is readable with Claude Code's `Read`, as a picture. A file is at most 64 MiB (`store.MaxUploadBytes`), and kept whole (`uploads`).

`POST /conversations/{id}/uploads` takes `multipart/form-data` with one file (the field `file`, or the first part with a file name) and returns `201`:

```json
{"upload": {"id": "uuid", "name": "receipt.png", "media_type": "image/png",
            "bytes": 48213, "sha256": "hex", "created_at": "..."}}
```

- `name` is the file's base name, without path or control characters, at most 200 bytes. A name already taken in the conversation is numbered: `receipt (2).png`. The stored name is the one returned.
- `media_type` is the part's `Content-Type`, sniffed from the content when it is absent or `application/octet-stream`.

`POST /conversations/{id}/uploads/url` `{"name": "video.mp4", "media_type": "video/mp4", "bytes": 52428800}` returns a signed URL for a file larger than a proxy in front of orbit accepts:

```json
{"url": "https://orbit.kanishksachdev.com/api/v1/uploads/signed?...", "method": "PUT",
 "headers": {"Content-Type": "video/mp4"}, "expires_at": "...", "max_bytes": 52428800}
```

The browser sends the file as the body of a `PUT` to `url`, with no `Authorization` header: the URL's signature (HMAC under a key derived from `ORBIT_API_TOKEN`) is the credential, and it names one conversation, the name, the type, `bytes` as the most it accepts, the person it was issued to, and an expiry 15 minutes ahead (`api.UploadURLTTL`). The answer is `201` with the same `{"upload": ...}` as above. A URL uploads once: a second `PUT` is `409`. A body larger than `bytes` is `413`, an altered or expired URL `403`. It needs `public_url` in `orbit.yaml` (`503` without it).

`GET /conversations/{id}/uploads/{upload}` is the file: its `media_type` as `Content-Type`, `Content-Disposition: inline` for an image (not SVG) and `attachment` for everything else, with `nosniff` and a CSP that runs nothing. It carries `Content-Length`, `Last-Modified` (`created_at`) and `Accept-Ranges: bytes`: a `Range` request is `206` with only those bytes (a video seeking), one past the end `416`.

A message carries uploads with `attachments` (below): ids of uploads of the same conversation, at most 20; one from another conversation is a `400`. A message with attachments may have an empty `body`.

## Chat agents

An agent with `chat: true` (the built-in `orbit` is one) is a Claude Code session rather than a report writer: its run's `report` is the session's final message, as written, and `summary` that reply's first line. It changes its memory, asks questions and proposes actions with orbit's tools during the turn instead of in a schema, so its runs carry `questions` and `actions` as any run does. Each conversation has its own workspace (`GET /conversations/{id}/files`): files the agent writes, and the person's uploads, stay between messages. **Instructions.** In a run that answers its person's message, a chat agent's actions are that person's instructions, as typing into Claude Code is: an action whose rule is `auto`, or `approve` (the person asked for it), executes during the turn and the agent reads its result; the row records `instructed_by` and, for `approve`, `decided_by` as that person. A run nobody's message woke (a schedule, a manual run, a continuation) keeps its rules: `approve` waits. `deny` is never offered. On a worker, which holds no executor credentials, every action waits for control.

**GitHub** (the built-in has `github:clone` auto, `github:pr` and `github:merge` approve): `github:clone {repo, dir}` puts a repository's default branch into the workspace; `github:pr {repo, dir, branch, title, body, push_only}` takes the working tree at `dir` (never its `.git`), commits it as the App's bot user on top of the default branch, holds it to the build's diff policy (no `.github/`, no credential in an added line), pushes `orbit/chat/<branch>` and opens or updates its PR; `github:merge` merges it once every required check passed. Tokens stay in orbit; the repositories are `github.repositories`.

With `sandbox: workstation` (the built-in's, when sandboxd is configured) the session runs in a sandbox container with Claude Code's full tool set and the open internet; `sandbox: build` with the tools it names.

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
   "run_id": "uuid-of-run-that-read-it",
   "reads": [{"run_id": "uuid-of-run-that-read-it", "attempt": 1, "read_at": "...", "released_at": null}],
   "attachments": [{"id": "uuid", "name": "receipt.png", "media_type": "image/png",
                    "bytes": 48213, "sha256": "hex", "created_at": "..."}]},
  {"kind": "run", "id": "uuid", "at": "...", "trigger": "message",
   "status": "succeeded", "attempt": 1,
   "started_at": "...", "finished_at": "...", "steps": 4,
   "turn": {"state": "completed", "outcome": "success", "started_at": "...", "completed_at": "..."},
   "summary": "...", "report": "<markdown>",
   "questions": [{"id": "uuid-of-the-question-message", "body": "...", "open": true}],
   "error": null, "cost_usd": 0.03, "gate": null,
   "actions": [{"id": "uuid", "type": "notify", "status": "pending", "reason": "...", "params": {}}],
   "tasks": [{"id": "uuid", "key": "facts", "agent": "researcher", "status": "working",
              "conversation_id": "uuid", "depends_on": [], "summary": null, "error": null,
              "decision_action_id": null}],
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
  `reply_to`, when set, is the message it answers. `attachments` are the uploads it carries (Uploads, above), in order; always an array. `run_id` is the run that
  holds a person's message (`null` while it waits for a run: none has read it, or every attempt that did handed it back) or the run that asked a
  question. `reads` (a person's message, once a run has read it) is every attempt that read it, oldest first: `{"run_id", "attempt", "read_at", "released_at"}`, where `released_at` is when a failed or interrupted attempt handed it back, `null` for the one holding it.
- **Runs.** `trigger` is `schedule`, `manual`, `message`, `continue` (the agent asked to be woken again), `build` (a ticket's build run, docs/tickets.md), `learn` (a learning run, below; only in an agent's learning conversation), `task` (in a delegated task's conversation: the task starting, or its parent writing to it) or `delegation` (the agent woken because tasks it delegated settled; below). `status` is
  `queued`, `running`, `succeeded`, `failed`, `cancelled` or `skipped` (it
  ended without a backend call because a gate, code, found nothing to judge;
  `gate` names it, and is `null` for every other status). `attempt` counts
  claims (0 while never started; a run interrupted by a deploy is claimed
  again). `started_at` is the latest attempt's start, `null` while queued.
  `finished_at` is `null` until final. `steps` is the number of tool calls in
  the latest attempt's timeline, exactly the `tool` events its stream shows.
  `turn` is the body of the newest revision of the timeline's latest journal
  turn (`state`, `outcome`, ...; as in The journal), `null` before the
  backend was called. `summary`,
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
- **Tasks** are the tasks the run delegated (its `delegate` actions), in the order it listed them; always an array. `summary` is the finished task's report summary. For the whole task, and live status, see Delegation below.
- **Actions.** `status` is `pending` (waiting for a person), `approved`
  (by a person or by an `auto` rule, about to execute), `rejected` (by a
  person), `denied` (by rule or invalid; never executes), `executing`,
  `executed` or `failed`.

`POST /agents/{name}/messages`

```json
{"body": "text", "reply_to": "optional message id", "wake": true,
 "attachments": ["optional upload ids"],
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

`GET /runs/{id}/stream` is Server-Sent Events over the run's journal (Runs, events, artifacts: The journal): every row of the latest attempt's timeline, then each new row as it is written. Events, in order:

| event | id | data | meaning |
|---|---|---|---|
| `status` | none | `{"status": "running", "attempt": 2, "epoch": 1, "continues": 1}` | sent first, then on every change |
| `item` | the row's cursor | a journal row, exactly as `GET /runs/{id}/journal` returns it | the timeline grew by one revision |
| `text` | the row's cursor | `{"text": "..."}` | a new chunk of the agent's prose, to append to what came before |
| `tool` | the row's cursor | `{"name": "Read", "summary": "inputs/transactions.json"}` | the agent called a tool |
| `done` | none | the run, identical to a thread run item (`kind: "run"`) | final; the stream ends |

- **`item`** is the stream to render from: one per journal row, revisions included, so a client keeps the highest `revision` per `item_id` (a tool call's result, a plan's update and a turn's end arrive as new revisions of items it already has).
- **`text` and `tool`** are the prose-only view of the same rows, for a client that shows a run as lines: `text` for the first revision of each `message` of role `assistant`, `tool` for the first revision of each `tool_call` (its `name` and `summary`). A row with one is sent as that event followed by its `item`, in one write, both with the row's id. `text` chunks concatenate into the timeline's prose: every chunk after the timeline's first starts with a blank line (`"\n\n"`), so a client appends them verbatim. A model's reasoning is a `thinking` item, not `text`.
- **`status`** carries `epoch` once the run has started (absent while queued without an attempt), and `continues` when the attempt resumed an earlier attempt's session (the attempt it continues). When `epoch` changes from the one a client was showing, the run started over: what the client holds belongs to an abandoned timeline, so it clears, and the new timeline streams from its first row. When only `attempt` changes, the timeline goes on: the client keeps what it has, and the next rows continue it.
- **Replay and resume.** Without `Last-Event-ID`, the stream replays the timeline from its first row, then follows live, so a client that connects late gets everything. A client that reconnects with `Last-Event-ID` set to the last id it received (a browser `EventSource` does this itself) gets only the rows after that cursor, from any instance: the stream is read from the database, so a reconnect that a deploy sends to the new instance continues seamlessly. A cursor into another timeline, past the timeline's last row, or an id that is not a cursor (such as the `<attempt>.<seq>.<index>` ids of earlier releases) means a full replay of the current timeline. `status` and `done` are sent again on every connection.
- **Requeue.** A run handed back (a deploy's drain past its cap, a crash) streams `status: queued`, then `status: running` with the next `attempt`, and the interrupted turn as a new revision (`state: interrupted`, or `unverifiable` when its worker was lost).
- A heartbeat comment (`: ping`) is sent every 15 s. A finished run streams its status, its replay and `done` at once.
- The stream ends when the instance serving it exits (the end of a deploy's drain); reconnect with `Last-Event-ID`.

The final answer is `done.report`, not the concatenated `text`: `text` is what the agent narrated while working.

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
- `report` is the submitted `report`, except when the attempt's final assistant prose (its last run of top-level `message` items, as in The journal) holds more: the submitted one was empty, one line where the prose has several, or shorter. Then `report` is that prose, `summary` stays the submitted one, and `output` keeps what was submitted. A chat agent's `report` is its final message.
- `session_id` is the backend session the run ended in. `session_mode` (`fresh`, `resumed`, `live`), `session_parent` (the run whose session it continued) and `session_depth` are set for an agent with a session.
- `run_after`, `continues` (the run that asked) and `chain` (its place in the chain, from 1) are set on a `continue` run.
- `gate` names what ended a `skipped` run.
- `steps` is the number of tool calls in the latest attempt's timeline, counted as a thread's run item counts it.

An attempt (`store.Attempt`), oldest first:

```json
{"attempt": 2, "worker": "orbit/0", "runner_kind": "claude-code", "spec_sha": "hex",
 "started_at": "...", "ended_at": "...", "outcome": "succeeded",
 "session_id": "...", "recovered_from": 1, "recover_skipped": "...",
 "cost_usd": 0.01, "session_cost_usd": 0.04}
```

`attempt`, `worker` and `started_at` are always present; the rest is omitted when unset. `outcome` is `succeeded`, `failed`, `skipped`, `handed_back` (stopped by its instance) or `lost` (reaped without a heartbeat), absent while the attempt runs. `recovered_from` is the attempt whose session it continued, `recover_skipped` why it started over instead. `cost_usd` is the attempt's share, absent when unknown (a killed process saved none); `session_cost_usd` is the backend's running total for the session after it.

`POST /runs/{id}/cancel` stops a run. A queued run is cancelled at once: `200 {"cancelled": true, "status": "cancelled"}`. A running run is asked to stop: `202 {"cancelled": false, "status": "cancelling"}`; its attempt sees the request within 2 s (`engine.cancelPoll`), interrupts the backend (a chat agent's turn through Claude Code's control channel, which ends the turn with what it has; any other run's backend is stopped), and ends the run `cancelled`, with the reply so far as its `report` and `error` `"stopped by its person"`. A stopped run notifies nothing. Any other state is 409.

### Events

`GET /runs/{id}/events?attempt=&after=&limit=` is `{"events": [<event>]}`, one attempt's transcript in order: `attempt` (default: the run's latest), the events with `seq` above `after` (default 0), at most `limit` (default 1000, at most 5000). Page by passing the last `seq` as `after`.

```json
{"attempt": 1, "seq": 12, "at": "...", "kind": "assistant", "payload": {}}
```

`payload` is one line of the backend's stream as it sent it (a line that was not JSON is stored as a JSON string). `kind` is the line's `type`, with `.<subtype>` when it has one (`system.init`, `result.success`, `assistant`, `user`, `rate_limit_event`; Codex's `item.started`, `item.completed`, ...), `response` for the Messages API, and `raw` for a line without a type. orbit's own events are `orbit.*`: `orbit.source_fetched` and `orbit.source_failed`, `orbit.transform` and `orbit.transform_failed`, `orbit.gate`, `orbit.session`, `orbit.recovery`, `orbit.figures_unverified`, and a build's `orbit.build`, `orbit.git`, `orbit.sandbox`, `orbit.permission` and `orbit.permission_denials`, and a chat run's `orbit.permission` (a permission request's decision) and `orbit.permissions` (its level applied; The journal, `permissions`).

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
| `permissions` | `permissions:<a>.<s>.<i>` | `{"level": "auto", "applied": "ask", "mode": "default", "when": "start", "source": "conversation", "version": 3, "set_by": "kanishk", "expires_at": "...", "reason": "..."}`; a chat run's permission level applied to its Claude Code process: `when` is `start` (a new process), `turn` (a live process taking the run) or `live` (changed during the turn, Conversation permissions). `level` is what was asked for, `applied` what the process is at (`auto` the CLI refuses is `ask`), `mode` its Claude Code mode. `source` is `conversation` (with `version`, `set_by` and `expires_at` when set) or `agent`. `reason` is the CLI's refusal, or why a change was not applied. One per application, never revised. |
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

`direction` is `in` (what the run read) or `out` (`output.json`, what the backend returned). The inputs the model was shown are under `inputs/` (`inputs/<run>-<attempt>/` for an agent with a session); a source only transforms read is under `sources/`; an agent with `repos` has `inputs/_repos.json` (Repositories, below; the trees themselves are not stored); `memory.md`, `messages.json`, `conversation.md`, `system.md`, `prompt.md` and `schema.json` are the rest of what the run was given. `GET /artifacts/{id}` is the content itself, with its `media_type` as `Content-Type` (served with `nosniff` and a `default-src 'none'` CSP), and `Content-Length` and `Range` support as an upload has.

### Deliveries

`GET /runs/{id}/deliveries` is `{"deliveries": [<delivery>]}`, every notification attempt of the run, oldest first:

```json
{"id": 7, "run_id": "uuid", "action_id": "uuid", "channel": "phone",
 "purpose": "approval", "status": "failed", "error": "...", "attempted_at": "..."}
```

`purpose` is `report`, `failure`, `mail_reply` (the answer to a mail, to its sender), `mail_failure` (the short note that a run answering a mail failed), `approval` (an action waiting for a person; `action_id` names it) or `action` (an approved action whose execution failed; `action_id` names it). `status` is `sent` or `failed`, with the channel's `error`. `run_id`, `action_id` and `error` are omitted when unset.

## Actions

`GET /actions?agent=&status=&run_id=&before=<cursor>&limit=<n>` is `{"actions": [<action>], "has_more": false, "next_before": null}`, newest first. `status` is as in a thread action; `run_id` narrows to one run's proposals (400 when it is not a UUID). Paging is as for a thread: `next_before` is an opaque cursor, `null` exactly when `has_more` is false; `limit` defaults to 100, at most 500. `GET /actions/{id}` is `{"action": <action>}`:

```json
{"id": "uuid", "run_id": "uuid", "agent": "finance", "type": "webhook:home-assistant",
 "params": {}, "reason": "...", "rule": "approve", "status": "executed",
 "decided_by": "kanishk", "decided_at": "...", "executed_at": "...",
 "result": {}, "error": "...", "created_at": "...", "instructed_by": "kanishk"}
```

`rule` is the agent's rule for the type when it was proposed (`auto`, `approve`, `deny`). `decided_by` is the person, or `rule` for an action its rule approved or denied. `instructed_by` is the person whose message a chat agent was answering when it proposed the action: their message is the instruction (Chat agents, above), so an `approve` action they asked for is approved by them (`decided_by` is that person) without a second approval. `result` is what the executor returned; `error` is why it was denied (an unknown type, invalid params, a limit) or why it failed. `decided_by`, `decided_at`, `executed_at`, `result` and `error` are omitted when unset.

- `POST /actions/{id}/approve`, `POST /actions/{id}/reject`: `200 {"ok":
  true}`; 404 for an unknown action; 409 when it was already decided. With
  `{"client_request_id": "..."}` (same rules as `client_message_id`), a retry
  of a decision that succeeded returns `200 {"ok": true}` again instead of
  409. The
  decider is the token's person. Only with the operator token, `{"by":
  "name"}` names it (default `operator`). The body is optional; one that is malformed or has another field is a `400` and decides nothing.

## Attention and push

Something waiting on a person is an **attention** row, written in the same transaction as the thing itself, one per thing (`key`): a pending approval (`approval`, key `action:<id>`), a run's question (`question`, `question:<message id>`), a failed run (`failure`, `failure:<run id>`) and a decision: a ticket plan to approve (`decision`, `plan:<ticket id>:<version>`) or a delegated task blocked on a person (`decision`, `action:<task:decision action id>`). Whether it is still open is read from the thing at query time: the action still `pending`, the question unanswered, the plan the ticket's newest while it is `planned`, a task's decision action still `pending`, a failure until a later run of its conversation succeeds or the person writes there. Dismissing closes any of them for the inbox only (a dismissed approval stays pending).

`GET /attention?limit=<n>` is everything open, newest first (`limit` defaults to and is at most 500):

```json
{"items": [{
  "id": "uuid", "key": "action:uuid", "kind": "approval", "agent": "finance",
  "run_id": "uuid", "conversation_id": "uuid", "action_id": "uuid",
  "message_id": null, "ticket_id": null, "plan_version": null,
  "title": "finance wants to webhook:pay", "detail": "<the action's reason>",
  "push": true, "created_at": "...", "link": "/orbit/c/<conversation_id>#action-<action_id>"}]}
```

`title` is the agent's name and a fixed phrase, never anything a run wrote. `detail` is the question, the action's reason, the run's error or the ticket's title, for a signed-in inbox only. `push` is false for a run a person's message woke (`engine.Attended`): listed, not pushed. `link` is where to act, under `ORBIT_PUSH_LINK_BASE`: the conversation, with `#action-<id>` for an approval or a task's decision, `#message-<id>` for a question. `POST /attention/{id}/dismiss` is `{"ok": true}` (repeating it is fine; 404 for an unknown id).

**Web Push.** Control sends each attention row with `push` set to every registered device, straight to the browser's push service (RFC 8291 encryption, RFC 8292 VAPID; `internal/webpush`). The payload is only `{"title", "url", "tag", "kind", "count"}`: the title above and the link. Rows that arrive within 20 s of the previous push (`push.Gap`) wait and go out together as one summary (`"3 things need you: finance, ops"`, url `<base>/inbox`, tag `orbit-inbox`); a row that closed before its turn, or waited more than a day (`push.MaxAge`), is not sent. `tag` and the `Topic` header are the row's key, so a device and the push service each keep one notification per thing. A send that fails is not retried; the inbox still lists it.

- `GET /push` is `{"enabled": true, "public_key": "<VAPID public key, base64url>"}` (`enabled: false`, `public_key: null` without keys).
- `POST /push/subscriptions` takes a browser's `PushSubscription.toJSON()` (`{"endpoint", "expirationTime", "keys": {"p256dh", "auth"}}`; `expirationTime` is ignored) and an optional `label`, and returns `201 {"id": "uuid"}`. The endpoint must be https on a known push service (FCM, Apple, Windows, Mozilla; `push.AllowedEndpoint`), else 400. The same endpoint again refreshes its row. 503 when push is not configured.
- `GET /push/subscriptions` is the caller's active devices (`id`, `endpoint`, `author`, `label`, `created_at`, `last_sent_at`, `last_error`, `failures`; never the keys); every device for the operator token.
- `DELETE /push/subscriptions` with `{"endpoint": "..."}` stops pushing to it (404 when none is active). A push service answering 404 or 410 revokes it too.
- `POST /push/test` sends `orbit notifications are on` to the caller's devices now, ignoring the gap: `{"sent": <devices that accepted it>}`.

| variable | meaning |
|---|---|
| `ORBIT_VAPID_PUBLIC_KEY`, `ORBIT_VAPID_PRIVATE_KEY` | the VAPID pair, base64url (`orbit vapid` prints a new one). Both unset: push is off, the inbox still works. Set but invalid or mismatched: `serve` refuses to start. Rotating the pair invalidates every subscription (browsers bind them to the key), so devices subscribe again. |
| `ORBIT_VAPID_SUBJECT` | the contact push services may use, `mailto:` or `https:`. Required with the keys. |
| `ORBIT_PUSH_LINK_BASE` | the app path links start with, default `/orbit` (the portfolio's chat). |

## Tickets

A ticket is a conversation with a lifecycle ([tickets.md](tickets.md)): `POST /tickets` (opens its conversation; returns `ticket_id`, `conversation_id`, `message_id`, `run_id`; `stack_on` files a request on top of another, tickets.md, Stacked tickets), `GET /tickets`, `GET /tickets/{id}` (the ticket, its plans and transitions; a ticket that reached merging carries `merge_action_id`, its `github:merge` action), `POST /tickets/{id}/plans/{version}/approve`, `POST /tickets/{id}/{abandon,retry,reopen}`, and `POST /tickets/{id}/checks/rerun` (`202`; the ticket loop reruns the PR's failed Actions jobs on its next tick, and a failed ticket goes back to review; `GET /tickets/{id}` lists every rerun in `reruns`), and review comments on its PR: `GET`/`POST /tickets/{id}/comments`, `POST /tickets/{id}/comments/{comment}/{edit,withdraw}` and `POST /tickets/{id}/review`, which sends the drafts as one message (tickets.md, Review comments). Its thread and messages are its conversation's (`/conversations/{conversation_id}/thread`, `/messages`); a message there reopens a `planned` ticket, and a `merged`, `done` or `abandoned` one refuses messages (409). A ticket's conversation carries `ticket_id` in the conversation list. Filing and approving take `client_request_id`; the author and approver are the token's person. A transition the ticket's state does not allow is 409.

`GET /tickets?status=&agent=&before=&limit=` is `{"tickets": [...]}`, newest first. `status` and `agent` match exactly; `before` is an RFC 3339 time and returns tickets created before it (400 if it does not parse), so the last ticket's `created_at` is the next page's cursor. `limit` defaults to 50, at most 500. Each ticket:

```json
{"id": "uuid", "kind": "request", "title": "...", "agent": "builder",
 "conversation_id": "uuid", "repo": "kensac/orbit", "status": "review",
 "author": "kanishk", "approved_plan": 1, "base_sha": "...", "branch": "orbit/1a2b3c4d",
 "pr_number": 61, "head_sha": "...",
 "created_at": "...", "updated_at": "...", "merge_action_id": "uuid"}
```

`repo`, `author`, `approved_plan`, `base_sha`, `branch`, `pr_number`, `head_sha`, `merge_sha`, `error`, `merge_action_id` and `stack_on` are omitted when unset; `error` is set only while the ticket is `failed`. `GET /tickets/{id}` is `{"ticket": <as above>, "plans": [...], "events": [...], "reruns": [...]}`.

## Delegation

An agent hands tasks to other agents (docs/ARCHITECTURE.md, Delegation). Each task has its own conversation of its agent, which is an ordinary conversation (thread, messages, runs) whose first message is the brief, written by the delegating agent: a message's `author` of `agent:<name>` is an agent, not a person. The delegating conversation's thread shows each run's `tasks`; these routes show the rest.

`GET /conversations/{id}/tasks` is `{"tasks": [<task>]}`: the tasks delegated from that conversation, oldest first. `GET /tasks/{id}` is `{"task": <task>}`:

```json
{"id": "uuid", "key": "facts", "agent": "researcher", "conversation_id": "uuid",
 "parent_agent": "planner", "parent_conversation_id": "uuid", "parent_run_id": "uuid",
 "action_id": "uuid", "brief": "Find the facts.", "fresh": false, "depth": 1,
 "status": "done", "depends_on": ["other-key"],
 "decision": {"action_id": "uuid", "question": "Spend $20?", "status": "executed", "decided_by": "kanishk"},
 "report": {"run_id": "uuid", "summary": "...", "report": "<markdown>"},
 "error": null, "run_id": "uuid", "run_status": "succeeded",
 "questions": [{"id": "uuid", "body": "Which year?", "open": true}],
 "created_at": "...", "updated_at": "...", "finished_at": "..."}
```

- `status`: `waiting` (a dependency is not done), `blocked` (its decision is pending), `working` (a run in its conversation is queued or running), `needs_input` (its agent asked the delegating agent something; `questions`), `done` (`report` is the reply of the run that finished it), `failed` or `cancelled` (`error` says why). A message from the delegating agent moves a `done` task back to `working`; `report` keeps the previous reply until the next.
- `depends_on` is the keys of tasks in the same batch (`action_id`) it waits for. `decision` is `null` without one; otherwise approve or reject its `action_id` like any action (`POST /actions/{id}/approve`), and the task starts or is cancelled within seconds.
- `run_id` and `run_status` are the newest run in its conversation; stream it with `/runs/{id}/stream` to show the task's work as it happens. `fresh` is a worker started from its agent's spec without its memory. `depth` is 1 for a task a top-level conversation delegated.

`POST /tasks/{id}/cancel` `{"reason": "optional"}`: `200 {"cancelled": true}`, `409` when the task is already over. Its queued runs are cancelled (a running one finishes, unheeded) and its dependents with it. The canceller is the token's person (with the operator token, `"by"` names it).

`GET /conversations/{id}/tasks/stream` is Server-Sent Events: a `tasks` event (`{"tasks": [<task>]}`, as the list) on connect and again whenever any task of the conversation changes, and `: ping` every 15 s. It never ends on its own; reconnect after a deploy (there is no `Last-Event-ID`: the first event is the whole state). A chat UI shows the delegating run's tasks from its thread item, then keeps them live from this stream, and opens a task's `conversation_id` to show its work.

## Workspace

**Partly built.** The contract the portfolio's workspace builds against (design: [workspace.md](workspace.md)). Built: `GET /agents/{name}/workspace` for an agent with a box, and Terminals (below). Every other route here is planned and answers `404` until built; each part says "built" in the PR that implements it.

A workspace is a file tree a person and an agent share. Routes take the authentik token as above; writes, trash, uploads, terminals and git writes also require membership of `ORBIT_WORKSPACE_GROUP` (default `ORBIT_OIDC_GROUP`; `403`, `check: "group"`). Errors carry a `check` beside `error`, as authentication errors do:

| `check` | status | meaning |
|---|---|---|
| `path` | 400 | not clean, absolute, outside the tree, or through a symlink out of it |
| `not_found` | 404 | no such path, ref, terminal or comment |
| `exists` | 409 | the target of a create, rename or restore exists |
| `version` | 412 | `If-Match` is not the current version; the body carries `current` (the entry, or `null` when it is gone) |
| `version_required` | 428 | a write to an existing file without `If-Match` |
| `read_only`, `no_shell`, `no_git` | 409 | the backend does not have that capability (`capabilities`, below) |
| `busy` | 409 | a snapshot write while a run of the conversation is running |
| `too_large` | 413 | over a size limit (workspace.md, Auth, scope and limits) |
| `limit` | 409 | a count limit (terminals, clients) |
| `rate` | 429 | over a rate, with `Retry-After` |
| `box_starting` | 503 | the box is starting, with `Retry-After` |

### Finding a workspace

`GET /conversations/{id}/workspace` and `GET /agents/{name}/workspace` return `{"workspace": <workspace>}`. Built: `GET /agents/{name}/workspace`, for an agent with a box (Terminals, below; `null` for any other agent until the snapshot backend is built); the conversation route is planned.

```json
{"id": "uuid", "scope": "conversation", "conversation_id": "uuid", "agent": "orbit",
 "backend": "snapshot", "cwd": "",
 "capabilities": {"read": true, "write": true, "terminals": false, "git": false,
                  "watch": true, "presets": []},
 "as_of": {"snapshot_id": 4, "run_id": "uuid", "at": "..."},
 "box": null, "created_at": "..."}
```

- `scope` is `conversation` (the snapshot backend: one per conversation) or `agent` (a box: one per agent, shared by its conversations). From a conversation, `cwd` is that conversation's directory in the tree (`""` on the snapshot backend, `conversations/<id>` in a box); a client opens there and lets the person go up. From an agent, `cwd` is `""`.
- `backend` is informative; a client decides from `capabilities`. `presets` lists the terminal presets (`shell`, `claude`).
- `as_of` is the snapshot read (snapshot backend), `null` in a box.
- `box` is `{"host": "oci", "arch": "arm64", "state": "running", "image": "sha256:..."}` in a box, `state` one of `starting`, `running`, `stopped`. A request to a stopped box starts it, waits up to 30 s, then answers `box_starting`. Built: `host` and `arch` are sandboxd's docker host's, `image` the reference the container runs (`ghcr.io/kensac/orbit-workstation@sha256:...`), `state` `running` or `stopped` (also before the box's first start); reading the workspace does not start the box, creating or attaching a terminal does, and `box_starting` is not used yet. Until the box serves files and git (W10, W13) its `capabilities` are `terminals: true` and `presets: ["shell", "claude"]` with every other capability false.
- A conversation of an agent without chat workspaces returns `{"workspace": null}`.
- Conversations gain `workspace_id` (the same id; `null` without one) in every conversation shape above.

### Files

Every path is a query parameter, `path`, relative to the root. An entry:

```json
{"path": "src/main.go", "name": "main.go", "kind": "file", "bytes": 1342,
 "mode": "0644", "modified_at": "...", "version": "sha256:<hex>",
 "target": null, "ignored": false, "git": "M", "has_children": null}
```

`kind` is `file`, `dir` or `symlink` (`target` its link text; a symlink is never followed out of the tree). `version` is for files only. `ignored` is from `.gitignore`. `git` is the porcelain status letters in a box (`null` outside a repository and on the snapshot backend). `has_children` is for directories.

- `GET /workspaces/{id}/tree?path=&cursor=&limit=` lists one directory: `{"path", "entries": [<entry>], "next_cursor": null}`, directories first, then by name; `limit` defaults to 500, at most 2000; `next_cursor` is opaque, `null` on the last page. `version` is omitted from tree entries (it costs a read of every file).
- `GET /workspaces/{id}/stat?path=` is `{"entry": <entry>}`, with `version`.
- `GET /workspaces/{id}/files?path=&ref=` is a file for the editor: `{"entry": <entry>, "text": "...", "reason": null}`. `text` is the whole file as UTF-8, byte for byte (line endings and a BOM kept). When it cannot be edited, `text` is absent and `reason` is `binary` (a NUL in the first 8000 bytes, or not UTF-8) or `too_large` (over 2 MiB). `ref` reads a past version from `history` (below). The response's `ETag` is the version.
- `PUT /workspaces/{id}/files?path=` writes the body (raw bytes, at most 8 MiB) as the file. `If-Match: "<version>"` is required for an existing file; `If-None-Match: *` creates and fails `exists` if it is there. Parent directories are created. `200` (or `201` created) `{"entry": <entry>}`.
- `POST /workspaces/{id}/mkdir` `{"path"}`: `201 {"entry"}`.
- `POST /workspaces/{id}/rename` `{"from", "to", "version"?}`: `200 {"entry"}` at `to`. Never overwrites (`exists`); `version` guards a file's content as `If-Match` does.
- `POST /workspaces/{id}/trash` `{"path", "version"?}` moves a file or directory to the trash: `200 {"trash": <item>}`. `GET /workspaces/{id}/trash?cursor=&limit=` lists items, newest first: `{"items": [...], "next_cursor"}`. `POST /workspaces/{id}/trash/{item}/restore` `{"to"?}` puts it back at its path or `to`: `200 {"entry"}`. There is no route that empties the trash.

```json
{"id": "uuid", "path": "notes/old.md", "kind": "file", "bytes": 210,
 "trashed_at": "...", "by": "kanishk", "restored_at": null}
```

- `POST /workspaces/{id}/upload?dir=` takes `multipart/form-data`, any number of file parts, each at most 64 MiB, named by the part's file name (cleaned as conversation uploads are). An existing name is `exists` unless `overwrite=1`, which trashes the old file first. `201 {"entries": [<entry>]}`. `POST /workspaces/{id}/upload/url` `{"path", "bytes", "media_type"}` returns a signed `PUT` URL exactly as `POST /conversations/{id}/uploads/url` does, up to 1 GiB, writing to `path` (`exists` applies).
- `GET /workspaces/{id}/download?path=&ref=` is a file as a download (`Content-Disposition: attachment`, `nosniff`, a CSP that runs nothing), or a directory as `application/gzip` (a tar.gz, `.orbit/` left out), at most 1 GiB. `inline=1` serves a raster image (not SVG) inline, for a preview.

On the snapshot backend a write, mkdir, rename, trash, restore or upload stores a new snapshot, refused with `busy` while a run of the conversation is running; `as_of` moves to it.

### History

`GET /workspaces/{id}/history?path=&before=&limit=` is a file's versions, newest first: `{"versions": [{"ref": "opaque", "version": "sha256:<hex>", "at": "...", "by": {"kind": "run", "run_id": "uuid", "person": null, "terminal_id": null}, "source": "snapshot"}], "next_before": null}`. `source` is `snapshot`, `checkpoint` (a box's per-turn record) or `git` (a commit; `rev` is set). `by.kind` is `run`, `person`, `terminal` or `unknown` (a change seen between turns that nothing attributed). Read one with `GET /workspaces/{id}/files?path=&ref=`.

### The change stream

`GET /workspaces/{id}/changes?path=` is Server-Sent Events for a subtree (default the root):

| event | id | data |
|---|---|---|
| `ready` | cursor | `{"backend": "box", "cursor": "..."}`, first on every connection |
| `change` | cursor | `{"path", "op": "create", "kind": "file", "from": null, "version": "sha256:<hex>", "by": {"kind", "run_id", "person", "terminal_id"}}`; `op` is `create`, `modify`, `trash`, `restore` or `rename` (`from` set) |
| `resync` | none | `{"reason": "cursor_lost"}` or `"overflow"`: re-read what is shown, then carry on |

`Last-Event-ID` resumes from a cursor; a cursor the serving instance no longer holds sends `resync`. `: ping` every 15 s. Changes made through this API and by runs are attributed; a change a terminal process made is `terminal` when one terminal was in the directory's session, else `unknown`.

### A reply's changes

Thread run items and runs gain `changes`: `{"files": 3, "additions": 40, "deletions": 2}`, `null` for a run with no workspace.

`GET /runs/{id}/changes` is what the run changed in its workspace:

```json
{"run_id": "uuid", "workspace_id": "uuid",
 "before": {"ref": "opaque", "at": "..."}, "after": {"ref": "opaque", "at": "..."},
 "files": [<diff file>], "truncated": false}
```

`before` and `after` are the snapshots (snapshot backend) or checkpoints (box) at the run's start and end. A run of an agent without a workspace is `404` (`check: "no_workspace"`); its journal's `diff` items are what it changed.

A diff file, here and in git diffs:

```json
{"path": "src/a.go", "old_path": null, "status": "modified", "binary": false,
 "additions": 3, "deletions": 1, "truncated": false,
 "hunks": [{"old_start": 10, "old_lines": 4, "new_start": 10, "new_lines": 6,
            "header": "func main() {",
            "lines": [{"kind": "context", "old": 10, "new": 10, "text": "..."},
                      {"kind": "add", "old": null, "new": 11, "text": "..."},
                      {"kind": "delete", "old": 11, "new": null, "text": "..."}]}]}
```

`status` is `added`, `modified`, `deleted`, `renamed` or `mode`. `text` is the line without its newline. Caps: 300 files and 10 MiB per response, 5000 lines per file (`truncated` on the file or the response); a truncated file is read whole with `path`.

### Git (box only)

`capabilities.git` false answers `no_git`. `repo` is a repository's path in the tree (a directory with `.git`, found up to three levels deep).

- `GET /workspaces/{id}/git/repos`: `{"repos": [{"path": "orbit", "branch": "main", "head": "sha", "upstream": "origin/main", "ahead": 1, "behind": 0, "remote": "https://github.com/kensac/orbit"}]}`, at most 50.
- `GET /workspaces/{id}/git/status?repo=`: `{"repo", "branch", "head", "files": [{"path", "orig_path": null, "index": "M", "worktree": ".", "conflict": false}]}` (porcelain v2 letters, `.` for unchanged).
- `GET /workspaces/{id}/git/diff?repo=&base=HEAD&head=worktree&path=&context=3&staged=false`: `{"base": {"rev": "HEAD", "sha"}, "head": {"rev": "worktree", "sha": null}, "files": [<diff file>], "truncated"}`. `base` is a revision or `merge-base:<rev>` (what the branch changed since it left `<rev>`); `head` is `worktree` (default), `index` or a revision; `path` narrows to one file or directory.
- `GET /workspaces/{id}/git/log?repo=&rev=HEAD&path=&before=&limit=`: `{"commits": [{"sha", "parents": [], "author": {"name", "email", "at"}, "subject", "body"}], "next_before"}`, `limit` default 50, at most 500.
- `GET /workspaces/{id}/git/show?repo=&rev=&path=`: the file at a revision, shaped as `GET /files`.
- `GET /workspaces/{id}/git/branches?repo=`: `{"branches": [{"name", "sha", "upstream", "current"}]}`.
- `POST /workspaces/{id}/git/commit` `{"repo", "message", "paths": [], "all": false}`: commits in the box as the person (author from the person's git identity), `paths` staged first, `all` as `git commit -a`. `201 {"sha"}`.
- `POST /workspaces/{id}/git/push` `{"repo", "remote": "origin", "branch"}`: pushes in the box with the person's terminal GitHub credential (direct mode). `200 {"pushed": true, "output": "..."}`; git's refusal is `409` with its output.

Unattended agents push through the action `workspace:push`, `{"repo", "branch", "base_sha", "head_sha", "reason"}`, denied unless the agent's spec names it: orbit reads the commits from the box as a bundle, applies the build diff policy, and pushes with an App token for the repository (`github.repositories`) to `branch`, which may not be the default or a protected branch. `result` is `{"pushed": "sha", "branch"}`.

### Terminals (box only): built

**Built.** What is still planned is said where it applies; a planned route answers `404`.

`capabilities.terminals` false answers `no_shell`. An agent has a box, and its workspace terminals, when its chats run in a workstation sandbox (`sandbox: workstation`) and orbit reaches sandboxd. A terminal:

```json
{"id": "uuid", "workspace_id": "uuid", "preset": "claude", "title": "Claude Code (direct)",
 "cwd": "", "state": "running", "exit_code": null, "end_reason": null,
 "cols": 120, "rows": 32, "clients": 2, "created_by": "kanishk",
 "created_at": "...", "last_attached_at": "...", "last_output_at": null, "ended_at": null}
```

`state` is `running` or `exited`; `end_reason` is `exit` (its session ended), `killed` or `box_restarted` (reserved: not yet written). `exit_code` is set with `exit`. Rows are never deleted: an exited terminal keeps its record. `clients` counts the sockets attached on the instance answering. `last_attached_at` is the newest attach; `last_output_at` is not recorded yet (always `null`). `cols` and `rows` are the size last asked for.

- `POST /workspaces/{id}/terminals` `{"preset": "shell", "cwd"?, "cols", "rows", "title"?}`: `201 {"terminal"}`. `preset` is `shell` or `claude`; `cols` 1 to 1000, `rows` 1 to 500; `cwd` is a clean path relative to `/work` (default `""`, `/work` itself; `path` otherwise); `title` defaults to `Shell` or `Claude Code (direct)`. The box is created or started here (a failure is `502` naming sandboxd's reason). At most 8 running per workspace (`limit`). `claude` options (`{"resume"?, "continue"?, "model"?}`) are planned and answer `400`.
- `GET /workspaces/{id}/terminals?state=running|exited|all`: `{"terminals": [...]}`, newest first, at most 500. `GET /terminals/{tid}`: `{"terminal"}`.
- `POST /terminals/{tid}/kill` `{"signal"?: "HUP"}` (`HUP`, `INT`, `TERM`, `KILL`; each ends the tmux session the same way): `200 {"terminal"}`, `exited` with `end_reason` `killed`. A terminal already ended is returned as it is.
- Planned: `PATCH /terminals/{tid}` `{"title"}`, `POST /terminals/{tid}/resize` (an attached client resizes in band), `GET /terminals/{tid}/scrollback?lines=`, and `GET /workspaces/{id}/claude-sessions?cwd=` (`{"sessions": [{"id", "started_at", "last_at", "first_prompt"}]}`, for a `resume` picker).

**Attaching.** `POST /terminals/{tid}/attach` `{"origin"?}` mints a ticket for the token's person and returns:

```json
{"url": "wss://orbit.kanishksachdev.com/api/v1/terminals/{tid}/ws?ticket=<ticket>", "protocol": "orbit.terminal.v1",
 "ticket": "<43 chars>", "protocols": ["orbit.terminal.v1", "orbit.ticket.<ticket>"], "expires_at": "..."}
```

The ticket is single-use, expires in 30 s, and is bound to the terminal, the person and an Origin: the body's `origin`, else the request's `Origin` header, else (a mint from a server-side proxy, which has neither) any of `ORBIT_WORKSPACE_ORIGINS` (comma list, default `https://kanishksachdev.com,https://www.kanishksachdev.com`); a named origin outside that list is `400` (`check: "origin"`). The ticket is both the `ticket` field and the `url`'s query. An ended terminal is `409` (`exited`). At most 10 tickets a minute per person (`429`, `rate`, `Retry-After`).

`GET /terminals/{tid}/ws` is the WebSocket. The browser opens it with `new WebSocket(url, protocols)`: the ticket travels in `Sec-WebSocket-Protocol` as `orbit.ticket.<ticket>` beside `orbit.terminal.v1`, which the server selects. A client that cannot set a second subprotocol may send the ticket as `?ticket=` instead (then it can reach access logs; it is still single-use and expires in 30 s); `orbit.terminal.v1` must be offered either way. Before upgrading, an `Origin` outside `ORBIT_WORKSPACE_ORIGINS` is `403` (`origin`), and a missing, unknown, used, expired or mismatched ticket is `401` (`ticket`). After the upgrade a terminal that cannot attach sends an `error` frame and closes. At most 8 clients per terminal on one instance (`limit`). Bearer tokens on the socket are planned.

| frame | direction | content |
|---|---|---|
| binary | both | terminal bytes: input to the terminal, output from it |
| `{"type": "ready", "terminal": {...}, "replay_bytes": 0}` | server | first. tmux redraws the screen on attach; replaying scrollback first (`replay_bytes` above 0: the next that many binary bytes are the replay) is planned |
| `{"type": "resize", "cols", "rows"}` | client | this client's size; send one after `ready` and on every change |
| `{"type": "ping"}` / `{"type": "pong"}` | both | the client pings every 10 s and may treat 25 s without a frame as a dead socket; the server answers each ping with a pong at once and sends its own ping every 15 s (Cloudflare closes a socket idle for 100 s) |
| `{"type": "exit", "code": 0, "reason": "exit"}` | server | the session ended; the terminal is `exited`; the socket closes (1000) |
| `{"type": "detached"}` | server | this client left tmux (its detach key) while the session lives; the socket closes (1000) and the terminal stays `running` |
| `{"type": "error", "check": "...", "error": "..."}` | server | then the socket closes (1008) |

Close codes: `4001` when the instance drains or shuts down: reconnect with a new ticket and the ready instance takes the terminal, which kept running. `4008` when the client took no output for 30 s: reattach. A socket closing for any reason detaches only its own tmux client; the session lives on.

**Without a WebSocket** (planned, for a proxy that cannot carry one): `GET /terminals/{tid}/output` is SSE, `ready` then `data` events (`{"b64": "..."}`, id an output offset), resumable with `Last-Event-ID` (a lost offset replays the scrollback after a `ready`); `POST /terminals/{tid}/input` `{"b64": "...", "cols"?, "rows"?}` writes input, `204`.

### Review comments

Comments on lines, sent to the conversation's agent as one message, as a ticket's review comments are (tickets.md).

- `POST /conversations/{id}/comments` `{"path", "start_line", "end_line", "side": "new", "anchor": {"kind": "run", "run_id": "uuid"}, "body"}`: `201 {"comment"}`, a draft. `anchor.kind` is `run` (a reply's changes), `git` (`repo`, `base`, `head` as for a git diff) or `worktree` (the file as it is). `side` is `new` or `old`. Lines `0` and `0` mean the whole file; a body is at most 8 KB.
- `GET /conversations/{id}/comments?status=`: `{"comments": [...]}`. `status` is `draft`, `sent`, `superseded` or `withdrawn`.
- `POST /conversations/{id}/comments/{c}/edit` `{"body", "start_line"?, "end_line"?}` writes a new version that supersedes it; `POST .../withdraw` withdraws a draft.
- `POST /conversations/{id}/review` `{"note"?, "comments"?: ["ids"], "wake"?: true, "client_request_id"?}`: sends the drafts (all of the conversation's, or the ones named) as one message, formatted by code: each comment's file, lines and anchor, the code it is on quoted, then its body quoted line by line. `201 {"message_id", "run_id"}` as `POST /conversations/{id}/messages`.

### Audit

`GET /workspaces/{id}/events?before=&limit=`: `{"events": [{"id", "at", "person", "op", "path", "from", "version_before", "version_after", "bytes", "terminal_id", "run_id", "request_id"}], "next_before"}`, newest first. `op` is each write route's name (`write`, `mkdir`, `rename`, `trash`, `restore`, `upload`, `commit`, `push`, `terminal.create`, `terminal.attach`, `terminal.detach`, `terminal.kill`, `review`). Terminal input is never recorded.

## Memory

`GET /agents/{name}/memory?limit=`: the agent's memory versions, newest
first, `{"versions": [{"id": 12, "run_id": "uuid", "content": "<markdown>",
"created_at": "...", "author": null, "restored_from": null}]}`. `limit` defaults to 20; the maximum is 200. `author` is the person who wrote a version through the API (`null` for a run's, whose `run_id` is set); `restored_from` is the version a restore copied.

`POST /agents/{name}/memory` writes the next version: `{"content": "<markdown>"}`, or `{"restore": <version id>}` to copy an earlier version of this agent's memory. Exactly one of the two (400 otherwise); `content` at most the deployment's `memory_max_bytes` (400); an unknown version or one of another agent is 404. Returns `201 {"version": {...}}`, shaped as above. The author is the token's person (with the operator token, the body's `author`). Nothing is replaced: the new version is the newest, every earlier one stays.

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

- `GET /memory/entries?scope=&topic=&agent=&type=&status=&q=&run_id=&limit=`: `{"entries": [...]}`, current versions, by scope (profile, topics, agents), then name, then most recently changed. `topic` or `agent` (not both) selects one place. `status` is `active` (default), `retired` or `all`; `q` matches key, title, description, body, why and how_to_apply, case-insensitively. `run_id` selects entries that run wrote a version of. `limit` defaults to 200, at most 1000.
- `POST /memory/entries` `{"scope", "topic"|"agent", "key"?, "type", "title", "description"?, "body", "why"?, "how_to_apply"?, "note"?}`: `201 {"entry": ...}`. An explicit `key` already used in the scope is 409; an `agent` that is not configured is 400.
- `GET /memory/entries/{id}`: `{"entry": <current>, "versions": [...]}`, every version newest first.
- `PATCH /memory/entries/{id}` `{"version"?, "type"?, "title"?, "description"?, "body"?, "why"?, "how_to_apply"?, "status"?, "note"?}`: the next version, with absent fields kept. `version` is the version the client read; another current one is 409, so two edits never silently overwrite each other (omit it to write regardless). `status: "active"` restores a retired entry (clearing `superseded_by`). An edit that changes nothing writes no version and returns the current one.
- `POST /memory/entries/{id}/restore` `{"restore": <version>, "version"?, "note"?}`: the next version, a copy of an earlier one's content and status (and its `superseded_by` while that entry is still active). `restore` must be an earlier version (400); `version` is as for `PATCH`. `note` defaults to `restored version <n>`.
- `POST /memory/entries/{id}/retire` `{"version"?, "superseded_by"?, "note"?}`: the next version, retired. `superseded_by` must be another, active entry (400 otherwise). This is how an entry is deleted.
- `POST /memory/import` `{"dry_run": bool, "entries": [<entry as for create, key optional>], "note"?}`: matches each by scope and key. A new key creates an entry, a known one gets a new version when its content differs or it was retired, an identical one is left alone, so an import can be repeated. `{"dry_run", "results": [{"scope", "topic"|"agent", "key", "outcome": "created"|"updated"|"unchanged", "id", "version"}]}`, in request order. All of it is one transaction: one invalid entry fails the whole import (400, naming its index). `dry_run` reports and writes nothing (`id` is absent for an entry it would create). At most 500 entries. `orbit import claude-memory` calls this (deployment.md).
- `GET /memory/topics`: `{"topics": [{"name", "entries", "agents"}]}` by name: every topic that holds entries or is read by an agent. `entries` counts its active entries; `agents` are the agents that read it, from their spec or attached.

### Entries agents write

An agent writes entries itself, without approval: every run's output carries `entries` (docs/ARCHITECTURE.md, Memory entries), applied with the run's outcome as versions with `source: "learned"`, `source_run_id` the run and `source_conversation_id` its conversation. A person reverts one with `restore` or `retire` above. A write the store refuses (a stale version, a key taken, an invalid field, a ref the run was not shown) is skipped and the rest apply. Each run's writes and what became of them are its `entries.json` artifact (`GET /runs/{id}/artifacts`): `[{"op", "ref", "title", "result": {"outcome": "written"|"unchanged"|"refused", "error", "id", "key", "version"}}]`, in output order.

### Learning runs

After a conversation goes quiet, orbit runs a short learning pass over it (trigger `learn`) that reads what was said since the last pass and writes entries the same way. It runs in the agent's **learning conversation**: one per agent, `purpose: "learning"`, titled `Learning`, archived, so it is listed only with `archived=true`; its thread is every learning run, each report listing the writes it made. Conversations carry `purpose` (`null` for a person's). Nothing a learning run does appears in the conversation it read.

`POST /conversations/{id}/learn` queues a learning run of that conversation now, whatever the quiet, through its newest message from the person: `202 {"run_id": "uuid"}`. 409 when the conversation has no message from the person or a learning run for that point exists already (learned, or tried); 400 for the learning conversation itself.

### What an agent reads

- `GET /agents/{name}/context`: `{"agent", "topics": [{"name", "from": ["spec"|"api"]}], "entries": <count>, "index": "<markdown>"}`. `index` is exactly what a run's prompt carries (with `inputs/_memory.md` for the run's own inputs path), `""` when the agent reads no entries.
- `PUT /agents/{name}/topics/{topic}`: attach a topic, `{"attached": true, "created": bool}` (`created` false when it was attached already). `DELETE /agents/{name}/topics/{topic}`: detach, `{"detached": true}`, 404 when not attached through the API (a topic the spec names stays until the spec changes). The agent must be configured (404).

## MCP

`/mcp` is an MCP server (Streamable HTTP, stateless) on both listeners, so the Claude apps and Claude Code can talk to orbit's agents. It takes what `/api/v1` takes on the same listener: an authentik access token (verified as above; the person is the author and decider), and on the internal listener the operator token. Without a valid token it answers `401` with `WWW-Authenticate: Bearer resource_metadata="<public_url>/.well-known/oauth-protected-resource/mcp"`. Every request stands alone (no `Mcp-Session-Id`), so a call may land on either instance of a rollout.

| tool | arguments | returns |
|---|---|---|
| `list_agents` | none | each configured agent: `name`, `description`, `enabled`, `needs_input`, `open_questions`, `pending_actions`, `last_run` |
| `list_conversations` | `agent`, `archived`, `limit`, `before` | as `GET /agents/{name}/conversations`: `conversations`, `has_more`, `next_before` |
| `create_conversation` | `agent`, `title`, `message`, `wait_seconds` | `conversation_id`, `message_id`, `run_id`, and `reply` as `send_message` |
| `send_message` | `body`, and `agent` (its default conversation) or `conversation_id`; `reply_to`, `wait_seconds` | `message_id`, `run_id`, `reply`: the run as a thread run item (`report`, `questions`, `actions`, `status`) |
| `wait_for_reply` | `run_id`, `wait_seconds` | `reply`, the run as a thread run item |
| `read_thread` | `agent` or `conversation_id`, `limit` (default 20, at most 200), `before` | as a thread page: `items`, `has_more`, `next_before` |
| `list_actions` | `agent`, `status` (default `pending`; `all` for every status), `run_id`, `limit`, `before` | as `GET /actions`: `actions`, `has_more`, `next_before` |
| `approve_action`, `reject_action` | `action_id` | the decided `action`; a decided one is an error |
| `search` | `q`, and as `GET /search`: `agent`, `conversation_id`, `run_id`, `kind`, `from`, `to`, `limit`, `offset` | as `GET /search` |

- **Waiting.** `send_message` and `create_conversation` wait for the run they woke up to `wait_seconds` (default 45, at most 300; `-1` returns at once). A run still going when the wait ends is returned with its status and a `note` to call `wait_for_reply`. A message that woke no run (`wake_on_message` off) says so in `note`.
- **Streaming.** When the call carries a `progressToken`, the wait sends the run's narration (the stream's `text` and `tool` events, a tool as `<name> <summary>`) as progress notifications on the call's response.
- **Same as HTTP.** A tool that mirrors an HTTP list or read makes the same store call and returns the same body as its HTTP counterpart, paging included: `before` is the previous page's `next_before`, which is `null` exactly when `has_more` is false. Only `read_thread`'s default `limit` (20) and `list_actions`' default `status` (`pending`) differ.
- Results are one text content block of indented JSON. A tool error (an unknown agent, a missing field, a store conflict) is a tool result with `isError`, worded as the HTTP error body's `error` is (`not found`, `not a valid id`, `before: ...`, `no longer in a state that allows this`).

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

## Skills

orbit keeps a library of Claude Code skills: each a `SKILL.md` (frontmatter with `name` and `description`, then the instructions) and optional files beside it (references, scripts, assets), every version kept. An agent runs with the skills its spec names (`skills`) and those attached here; a run gets each at its current version (docs/ARCHITECTURE.md, Skills). Agents also write and retire their own skills, applied directly (below).

A skill:

```json
{"name": "monthly-close", "version": 2, "status": "active",
 "description": "Use when the person asks to close a month.",
 "sha256": "<hex>", "source": "agent", "source_run_id": "uuid", "author": null,
 "note": "...", "restored_from": null, "created_at": "...", "updated_at": "...",
 "file_paths": ["references/format.md"],
 "content": "<SKILL.md>", "files": [{"path": "references/format.md", "content": "<base64>"}],
 "agents": ["finance"]}
```

- `name` matches `^[a-z][a-z0-9-]{0,62}$`; a `name` in the frontmatter must be the same. `description` is the frontmatter's (required: it is how Claude Code decides to use the skill). `SKILL.md` is UTF-8, at most 256 KB; at most 200 files, each at most 4 MB, 16 MB in all; a file's `path` is relative, clean, inside the skill and not `SKILL.md`. A file's `content` is base64.
- `version` counts versions, the newest in force; `status` is `active` or `retired` (a retired skill is given to no run). `sha256` is over `SKILL.md` and every file. `source` is `person`, `agent` (a run wrote it: `source_run_id`) or `import`. `author` is the token's person for `person` and `import`. `restored_from` is the version a restore copied.
- `content` and `files` are in the responses of `GET /skills/{name}` and `GET /skills/{name}/versions/{v}` only; lists carry `file_paths`. `agents` (in the list and `GET /skills/{name}`) are the agents that run with it, from their spec or attached.

Routes:

- `GET /skills?status=`: `{"skills": [...]}` by name, current versions; `status` as for entries.
- `POST /skills` `{"name", "content", "files"?, "note"?}`: `201 {"skill"}`; 409 when the name exists, 400 for an invalid skill.
- `GET /skills/{name}`: `{"skill": <current, in full>, "versions": [<every version without content>, newest first]}`.
- `GET /skills/{name}/versions/{version}`: `{"skill": <that version, in full>}`.
- `PUT /skills/{name}` `{"content", "files"?, "version"?, "note"?}`: the next version (`files` replaces the files; omitted means none). `version` is the version the client read; another is 409. Content and files identical to the current active version write nothing: `{"skill", "changed": false}`.
- `POST /skills/{name}/restore` `{"restore": <version>, "version"?, "note"?}`: the next version, an active copy of an earlier one.
- `POST /skills/{name}/retire` `{"version"?, "note"?}`: the next version, retired.
- `POST /skills/import` `{"dry_run", "skills": [{"name", "content", "files"?}], "note"?}`: each skill's next version where it differs from the current one (or is new, or retired); `{"dry_run", "results": [{"name", "outcome": "created"|"updated"|"unchanged", "version"}]}`, one transaction, at most 100 skills. `orbit import skills` calls it (deployment.md).
- `PUT /agents/{name}/skills/{skill}` and `DELETE` attach and detach, as for topics; attaching an unknown skill is 404. `GET /agents/{name}/context` lists the agent's `skills`: `[{"name", "from": ["spec"|"api"], "version", "status"}]`, `status` `missing` (and `version` null) for a name no skill has.

**Skills agents write.** A run's output carries `skills` (at most 3): `write` (the whole `SKILL.md` of a new skill, or of one the run has, whose files stay) and `retire` (one the run has). They apply with the run's outcome as `source: "agent"` versions, a write against the version the run was given (a newer one refuses it), and a written skill is attached to the agent. What became of each is the run's `skill-writes.json` artifact (`[{"op", "name", "result": {"outcome", "error", "key", "version"}}]`); the versions the run was given are its `skills.json` artifact. A person reverts with `restore` or `retire`.

## Search

`GET /search?q=&agent=&conversation_id=&run_id=&kind=&from=&to=&limit=&offset=`

Full-text search across everything orbit stores, ranked best first.

```json
{"query": "electricity bill",
 "total": 3,
 "results": [
   {"kind": "report", "agent": "finance", "conversation_id": "uuid", "run_id": "uuid",
    "attempt": 1, "seq": null, "message_id": null, "memory_version_id": null,
    "entry_id": null, "entry_version": null, "scope": null, "owner": null, "title": null,
    "author": null, "tool": null, "at": "...", "rank": 0.083,
    "snippet": "The electricity bill rose because of a rate change ...",
    "snippet_parts": [{"text": "The ", "match": false}, {"text": "electricity", "match": true},
                      {"text": " ", "match": false}, {"text": "bill", "match": true},
                      {"text": " rose because of a rate change ...", "match": false}],
    "links": {"run": "/api/v1/runs/uuid", "run_page": "https://orbit.kanishksachdev.com/ui/runs/uuid",
              "conversation": "/api/v1/conversations/uuid/thread"}}]}
```

- **`kind`** is what matched: `message` (a person's message to an agent; `author`, `message_id`), `question` (an agent's question; `message_id`, `run_id`), `report` (a run's summary and report), `error` (a failed run's error), `narration` (the prose a backend wrote while working, one transcript line; `attempt`, `seq` locate it in `GET /runs/{id}/events`), `tool_call` (one tool call's input as JSON, `tool` its name; tool results are not searched), `memory` (an agent's memory document; `memory_version_id`), `memory_entry` (a memory entry: `entry_id`, `entry_version`, `scope`, `owner`, `title`, and `agent` for an agent's own, `null` for a profile or topic entry). Of an agent's memory versions only the newest matching one is returned; a memory entry only as its current version, and never once retired. The `kind` parameter is a comma list of these.
- **`q`** is web-search syntax: words (all must match, stemmed: `bills` finds `bill`), `"a quoted phrase"`, `or` between alternatives, `-word` to exclude. Required (`400` without one). A query of only stop words matches nothing.
- **Ranking** is Postgres `ts_rank_cd` normalised by document length (`rank`, comparable only within one response), ties newest first. A memory entry's title weighs more than its body, and a tool's name more than its input.
- **`snippet`** is up to two fragments of the matching text joined by ` ... `; `snippet_parts` is the same text split into runs, `match` true on matched words, so a client highlights without parsing markup (the text is never escaped or marked up).
- **Filters:** `agent` (that agent's documents; a profile or topic entry belongs to none), `conversation_id`, `run_id` (a UUID, else `400`); `from` and `to` are a calendar day in the deployment's timezone (`to` includes its day) or an RFC 3339 time (`to` excluded), by the document's `at` (a message's or memory version's creation, a run's finish, a transcript line's time). `limit` defaults to 20, at most 100; `offset` pages, at most 1000. `total` counts every match within the filters, whatever the page.
- **`links`** are where the result is read in full: the run (`run`, and `run_page` when `public_url` is set) and its conversation's thread. A memory version names its run's conversation when it has one.
- Documents are derived by triggers as rows are written, and existing rows were indexed by the migration that added search, so a result is there as soon as its row is.

## Repositories

An agent with `repos` (docs/agents/configuration.md, Repositories) is given each repository's tree at one commit, without `.git`, at `inputs/repos/<name>/`. What it read is the input `_repos.json`, `[{"repo": "kensac/orbit", "ref": "main", "sha": "<40 hex>"}]` (`ref` absent for the default branch), and each repository is an `orbit.repo` event, `{"repo", "ref", "sha", "path", "files", "skipped"}` (`skipped`: symlinks and submodules left out); a fetch that fails is an `orbit.repo_failed` event `{"repo", "error"}` and fails the run, naming the repository.

A Claude Code run of such an agent has six read-only tools, `mcp__orbit__<name>` (a chat agent's are its SDK tools of the same names), run by orbit against its mirror and GitHub. Every tool takes `repo` (`owner/name`, one of the agent's; it may be left out when the agent has one); an unknown argument, a repository the agent does not name, or a revision or path starting with `-` is a tool error the model reads. Text answers are at most 64 KiB, cut at a line end with a note saying how much was left out.

| tool | arguments | answer |
|---|---|---|
| `git_log` | `rev` (default: the spec's `ref`, else the default branch), `path`, `since` (YYYY-MM-DD), `max` (default 20, at most 200) | `[{"sha", "author", "date", "subject"}]`, newest first |
| `git_show` | `rev` (required), `path` | the commit (`--format=fuller`, stat and patch), or with `path` that file at `rev` |
| `git_diff` | `base`, `head` (required), `path`, `stat` | the patch from `base` to `head`, or with `stat` the files and line counts |
| `git_blame` | `path` (required), `rev` (default as for `git_log`), `start`, `end` (1-based, inclusive) | `git blame -l --date=short` of those lines |
| `pr_list` | `state` (`open`, the default, `closed`, `all`), `limit` (default 30, at most 300) | `[{"number", "title", "state", "draft", "merged_at", "closed_at", "created_at", "updated_at", "html_url", "user": {"login"}, "head": {"ref", "sha"}, "base": {"ref"}}]`, most recently updated first, every page up to `limit`; `merged_at` is `null` unless it merged |
| `pr_view` | `number` (required) | `{"pull_request": {...pr_list's fields, "body", "merged", "mergeable", "mergeable_state", "merge_commit_sha", "additions", "deletions", "changed_files", "commits"}, "checks": [{"name", "status", "conclusion", "url"}], "statuses": [{"context", "state", "url"}]}`, the checks and statuses of its head commit |

A revision is anything git resolves in the mirror: a branch, a tag, a sha, `main~3`, or a pull request's head as `refs/pull/<n>/head`.

## Usage

`GET /usage?from=YYYY-MM-DD&to=YYYY-MM-DD`

```json
{"timezone": "America/New_York",
 "from": "2026-10-03", "to": "2026-11-01",
 "subscription": {
   "windows": [
     {"window": "five_hour", "status": null, "utilization": 0.42,
      "resets_at": "2026-11-01T19:00:00Z", "expired": false,
      "observed_at": "...", "source": "poll", "run_id": null},
     {"window": "seven_day", "status": "allowed", "utilization": 0.14,
      "resets_at": "2026-11-08T12:00:00Z", "expired": false,
      "observed_at": "...", "source": "stream", "run_id": "uuid"}],
   "overage": {"status": "rejected", "resets_at": null,
               "disabled_reason": "org_level_disabled", "in_use": false,
               "observed_at": "...", "run_id": "uuid"},
   "poll": {"enabled": true, "every": "5m0s", "last_at": "...", "last_ok_at": "...", "last_error": null}},
 "deferral": {"enabled": true, "threshold": 0.8,
              "held": [{"run_id": "uuid", "agent": "digest", "first_at": "...", "last_at": "...", "times": 3,
                        "window": "five_hour", "utilization": 0.86, "threshold": 0.8, "source": "poll",
                        "observed_at": "...", "held_until": "..."}]},
 "cost": {
   "total": {"api_equivalent_usd": 1.234567, "runs": 42, "runs_with_cost": 40,
             "by_status": {"queued": 0, "running": 1, "succeeded": 38, "failed": 3, "cancelled": 0, "skipped": 0}},
   "by_agent": [{"agent": "finance", "api_equivalent_usd": 0.811000, "runs": 30,
                 "runs_with_cost": 29, "by_status": {"...": 0}}],
   "by_day": [{"date": "2026-10-03", "api_equivalent_usd": 0.000000, "runs": 0}]}}
```

- **Each window is the freshest of two readings, and says which.** `source: "stream"`: the CLI writes a `rate_limit_event` into a run's stream when its rate-limit information changes; orbit stores it in the run's events (`GET /runs/{id}/events`), and this reads the newest one per window from runs created in the last 8 days (`store.RateLimitLookback`: the longest window, `seven_day`, and a day); `run_id` is that run. `source: "poll"`: with `usage_poll` configured, control asks the endpoint Claude Code's own `/usage` reads every few minutes and keeps each answer; this reads the newest successful one from the same 8 days (`run_id` and `status` are `null`: the endpoint reports neither). **The poll is best effort**: the endpoint is undocumented, its shape is the one Claude Code caches from it, and a poll that fails is recorded (`poll.last_error`) while the windows keep the last good reading. Of the two, the one observed later wins (`observed_at`); a tie goes to the stream. A window absent from the list was reported by neither in that time.
- `poll` is the poller's state: `enabled` (and `every`) as configured, `last_at` the newest poll of any outcome, `last_ok_at` the newest successful one, `last_error` the newest poll's error when it failed, else `null`.
- `deferral` is low-priority holding (`defer_low_priority`; docs/agents/configuration.md): `enabled`, `threshold` (`usage.DeferLowAbove`, a fraction like `utilization`) and `held`, the scheduled runs held now, oldest hold first: when first and last held, how many times, and the window, reading and source behind the latest hold, and `held_until`, when it is judged again.
- `window` is the CLI's name for it (`five_hour`, `seven_day`,
  `seven_day_opus`, `seven_day_sonnet`, ...). `status` is `allowed`,
  `allowed_warning` or `rejected`. `utilization` is the fraction of the
  window used, 0 to 1 (0.14 is 14%), or `null` when the CLI did not say.
  `resets_at` is when the window resets, `null` when not reported.
  `expired` is true once `resets_at` has passed: the window has reset since
  the observation, so its `utilization` is out of date. `observed_at` is when
  orbit stored the event or made the poll. The endpoint reports percent; it is divided by 100, so both sources are fractions.
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
 "queue": {"due": 0, "waiting_continuations": 1, "deferred": 0, "oldest_due_at": null},
 "running": [{"run_id": "uuid", "agent": "finance", "worker": "orbit/0",
              "attempt": 1, "started_at": "...", "heartbeat_at": "...",
              "heartbeat_age_seconds": 12.4}],
 "pending_actions": 2,
 "agents": [{"name": "finance", "enabled": true, "priority": "normal", "timezone": "America/New_York",
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
  whose `run_after` is still ahead, and `deferred` scheduled runs held back while the subscription is nearly spent (`/usage` `deferral.held` lists them). `oldest_due_at` is the oldest due run's
  `created_at`.
- `running` lists running runs, oldest first. `heartbeat_age_seconds` is
  measured by the database clock (`checked_at`); a run is requeued past
  `engine.StaleAfter` (300 s).
- `agents` is every agent in the configuration in force, by name, with its `priority` (`normal` or `low`).
  `last_success` is its newest succeeded or skipped run (a gate finding
  nothing to judge is not a stale agent), `null` if none.
  `next_fires` is each schedule trigger's next fire after now, read in the
  agent's timezone, soonest first, in UTC; `[]` for a disabled agent or one
  without triggers. A fire is enqueued by the scheduler's leader, so it is
  when the run is due, not when it starts. A cron trigger carries `schedule`; an RRULE trigger carries `rrule` and `start` instead, and one whose series has ended is not listed.
