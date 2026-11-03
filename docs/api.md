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
JWT check is its gate (the portfolio on Vercel calls it server-side). `/hooks/` is routed past forward-auth too, and takes no token: a delivery's signature is its credential (Webhooks, below).

### Configuration

| variable | meaning |
|---|---|
| `ORBIT_OIDC_ISSUER` | comma list of accepted issuers, one per authentik provider (`https://auth.kanishksachdev.com/application/o/<slug>/`, trailing slash included). Each provider signs with its own key, so each issuer's keys come from its own discovery document (`<issuer>.well-known/openid-configuration`, then `jwks_uri`). Unset: the public `/api/v1` refuses every request. |
| `ORBIT_OIDC_CLIENT_IDS` | comma list of accepted `aud` values (the providers' client ids). Required with the issuer. |
| `ORBIT_OIDC_GROUP` | the group a person must be in (today `infra-admins`). Required with the issuer. |
| `ORBIT_WORKSPACE_GROUP` | the group a person must also be in to change a workspace (Workspace, below). Unset: `ORBIT_OIDC_GROUP`'s members may. |
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
  "needs_input": true,
  "agent_status": {"state": "waiting", "kind": "question", "since": "...",
                   "source": "run", "run_id": "uuid"}
}]}
```

`unread_messages` counts the agent's open questions, in all its
conversations. `needs_input` is true
while the agent waits on its person: an open question or a pending action.
`spec` is the agent's spec as loaded (the canonical JSON stored for
`spec_sha`, defaults applied). `first_seen` is when orbit first stored the agent, `updated_at` when its stored spec, `enabled` or `present` last changed. `origin` and `version` are as in Defining agents (below); both are omitted for an agent no longer configured, and `version` for one not defined in the database. `last_run` is omitted before an agent's first run. `agent_status` is what the agent is doing (below).

`GET /agents/{name}` is `{"agent": <as in the list>, "memory": "<markdown>",
"next_fires": [...]}`. `next_fires` is as in `GET /health` (below); `[]` for
an agent with no schedule, a disabled one, or one no longer configured.
Earlier memory is `GET /agents/{name}/memory` (below).

## Agent status

`agent_status` says what an agent is doing, for a person deciding whether to look. It is on every agent, conversation, run, thread run item and running run in `GET /health`, in a stream, and in the MCP `list_agents`:

```json
{"state": "waiting", "kind": "permission", "detail": "Write",
 "since": "...", "source": "hooks", "run_id": "uuid"}
```

- `state` is `idle` (no run yet, or the latest was cancelled before it ran), `queued`, `working`, `waiting` (on the agent's person), `done` or `errored`.
- `kind`, only while `waiting`: `permission` (a tool call held for permission), `question` (the agent asked mid-turn, or a finished run's question is open), `elicitation` (an MCP server asked the person for input) or `approval` (an action is pending).
- `detail`, when there is one: the tool held for permission, what an elicitation asks, or why it errored (a Claude Code error code such as `rate_limit`, or the run's error, at most 300 bytes).
- `since` is when the state began, as far as orbit knows; `run_id` is the run it describes (absent with no run).
- `source` is who decided it. While a Claude Code run runs, its hooks (`hooks`) report its turn: prompt and tool events are `working`, a permission request, an elicitation or a question is `waiting`, a failed turn is `errored`; a turn that ended stays `working` until orbit has recorded its output. Everything else is the run's row (`run`): another backend, the time before the first hook, and every finished run (failed is `errored`; an open question or a pending action is `waiting`; otherwise `done`). The hooks report only for the attempt they ran in.
- An agent's or a conversation's status is that of its current run (the running one, else the newest), except that a finished current run gives way to an open question or a pending action anywhere in it, however old: `waiting`, with `kind` `approval` before `question`.

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
- A change stored whose reload then failed on this instance is still a success, with `pending_reload`: the reason. The change is stored and valid; the config sync retries the reload until it is in force, so retrying the request would only store another version. `spec` is then the agent as validated (absent after an archive). An `agent:define` action's result carries the same `pending_reload`.
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

**Agents change agents.** An agent whose rules enable `agent:define` (the built-in `orbit` does, `auto`) proposes `{"spec": {...}}`: a whole agent, as `POST /agents` takes it, for a new agent or one already defined in the database or built in, itself included. Executed, it goes through the same validation and is stored as the next version with the run as `run_id`. A file agent cannot be changed this way (the action fails, naming why). A change that widens the agent compared with its current version (a tool, source, credential, delegate target or need it adds, a source it changes, a move to a sandbox profile, an action type allowed more readily; for a new agent, any of these) is recorded with `rule: "approve"` and waits as `pending` for a person, whatever the agent's rule; one executing by rule that widens the agent as it is by then fails, naming what it widens. Its params may also carry `template` (`{"name", "version"}`), which a `template:create` proposal sets (Templates, below): where the spec came from, a record only.

**Schedules by talking.** `schedule:add`, `schedule:pause`, `schedule:resume` and `schedule:remove` change one database or built-in agent's triggers as the next version (`change: "update"`, the run as `run_id`), and `run:now` starts a manual run (docs/agents/configuration.md, Changing schedules by talking). Times are the person's words, parsed by orbit in the agent's timezone. An executed one's `result`, and a pending one's `preview` (Actions, below), is:

```json
{"agent": "news", "timezone": "America/New_York", "triggers": [0],
 "trigger": {"rrule": "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;BYHOUR=8;BYMINUTE=0;BYSECOND=0", "start": "2026-11-02T00:00:00", "prompt": "headlines"},
 "rule": "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;BYHOUR=8;BYMINUTE=0;BYSECOND=0",
 "paused_until": "2026-11-09T05:00:00Z",
 "next_fires": ["2026-11-03T08:00:00-05:00", "2026-11-04T08:00:00-05:00", "2026-11-05T08:00:00-05:00"],
 "version": 4}
```

`triggers` are the indexes changed (as `next_fires`' `trigger_index`); `trigger` is the one added or removed; `rule` is set when one trigger changed; `paused_until` only for a pause with an end; `next_fires` are the changed triggers' next three fires, merged, as local times (`[]` when paused with no end); `version` only once executed. `run:now`'s result is `{"agent": "news", "run_id": "uuid"}`.

**Save this conversation as an agent.** An agent with `agent:save` (the built-in `orbit`, `approve`) proposes `{"name", "description", "charter", "starters"}`. orbit drafts the rest from what the conversation did (docs/agents/configuration.md, Saving a conversation as an agent) and records the proposal as an `agent:define` with the drafted spec, `rule: "approve"` and `status: "pending"`; it never appears as an `agent:save` action unless it is refused (`denied`, with `error`, for example a name in use). Its `preview` is the dry-run diff (Actions, below); approving it creates the agent. `spec.starters` (on every agent row) are prompts a client offers for a new conversation.

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
  "agent_status": {"state": "done", "since": "...", "source": "run", "run_id": "uuid"},
  "ticket_id": null, "workspace_id": null, "task_id": null, "purpose": null,
  "last_run": {"id": "uuid", "status": "running", "trigger": "message",
               "created_at": "...", "finished_at": null, "summary": null},
  "read_through": "...", "from_session": null}],
 "has_more": false, "next_before": null}
```

Newest activity first (a message or a run in it is activity). `archived`
(default `false`) lists the archived ones instead. Paging is as for a
thread: `next_before` is an opaque cursor, `null` exactly when `has_more`
is false; `limit` defaults to 50, at most 200. `needs_input` is true while
one of its questions is open or one of its runs' actions is pending.
`ticket_id` is set when the conversation is a ticket's (below), `workspace_id` when it has a workspace (Workspace, below), `task_id` when it is a delegated task's own (Delegation, below); a task conversation's open questions are to the delegating agent, so they do not set `needs_input`.
`last_run` is the conversation's newest run (`id`, `status`, `trigger`, `created_at`, `finished_at`, `summary`; the last two `null` until it has them), `null` before its first, so a list shows which conversations are working. `read_through` is how far the person has read it (The board, below), `null` before they first did.

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

Either route also takes `from_session`: an imported session (Imported sessions, below), by orbit's `id` or by its `session_id`, to continue on the server. The conversation's runs resume that session (`--resume <id> --fork-session`) until one of them succeeds, and from then on the conversation's own session, as any session agent's do: the session's current lines are written as a transcript under a session id of orbit's (derived from the session and its `content_sha256`, never the laptop's own id) into the session store, and a run writes it back on whichever host claims it. Its `from_session` is the session's `id`. Without a `title` or `body`, the session's title (or first prompt) names it. The agent must run Claude Code with `session.resume` (the built-in `orbit` does): 400 otherwise, or for a session whose transcript is over 64 MiB compressed; 404 for an unknown session.

`GET /conversations/{id}` returns `{"conversation": {...}}`, the same shape.

`GET /conversations/{id}/files` lists a chat agent's workspace for the conversation (below, Chat agents) as of its newest snapshot, stored after every run:

```json
{"files": [{"path": "src/main.go", "bytes": 13, "modified_at": "..."}],
 "snapshot": {"id": 4, "conversation_id": "uuid", "run_id": "uuid", "attempt": 1,
              "source": "run", "author": null,
              "sha256": "hex", "bytes": 10240, "files": 3, "created_at": "..."}}
```

Regular files only, by path; orbit's own state (`.orbit/`, the run's `inputs/`) is left out. `snapshot` is `null` (and `files` empty) for a conversation without one; one a person's workspace write stored has `source: "person"`, `author` the person, and `run_id` and `attempt` null. `GET /conversations/{id}/files/{path}` is one file, always as a download (`application/octet-stream`, `Content-Disposition: attachment`, `nosniff`, a CSP that runs nothing), 404 for a path the snapshot does not have.

`PATCH /conversations/{id}` `{"title": "...", "archived": true}` (either
field) returns the updated `{"conversation": {...}}`. A title is at most 240
characters (400 otherwise). The default conversation cannot be archived
(409). A message to an archived conversation brings it back.

`POST /conversations/{id}/messages` takes and returns exactly what
`POST /agents/{name}/messages` does (below), in that conversation.

## The board

Every open conversation's state at a glance, for a sidebar or a status page, without opening each thread.

`GET /board?idle=<bool>&limit=<n>`

```json
{"rows": [{
  "conversation_id": "uuid", "agent": "finance", "title": "How much on Uber Eats in October?",
  "is_default": false, "ticket_id": null,
  "task": {"id": "uuid", "status": "blocked", "parent_conversation_id": "uuid"},
  "state": "needs_you",
  "preview": {"from": "agent", "text": "October: $212 across 9 orders", "at": "..."},
  "last_activity_at": "...", "age_seconds": 340, "read_through": "...",
  "last_run": {"id": "uuid", "status": "succeeded", "trigger": "message", "created_at": "...", "finished_at": "...", "summary": "..."},
  "counts": {"approvals": 1, "questions": 0, "decisions": 0, "tasks": 0, "unread": 1}}],
 "as_of": "...", "idle_after_seconds": 1800}
```

- **Rows** are the conversations that are not archived (a learning conversation is), delegated tasks' conversations included, newest activity first; at most `limit` (default 200, at most 500).
- **`state`** is derived by code (`store.BoardState`) from the counts and runs, the first that holds:
  - `needs_you`: an approval, a question to the person, or a decision (a task's `task:decision`, a ticket plan to approve) is open, or the row's task is `blocked`.
  - `working`: a run is running, or queued and due (a `continue` waiting for its `run_after` is not), or the row's task is `working` or `needs_input` (asking its delegating agent).
  - `failed`: the newest run failed, finished after `read_through` or active within the idle window.
  - `done_unread`: a run finished (`succeeded`, `skipped` or `failed`) after `read_through`.
  - `read`: none of these, with activity within the idle window.
  - `idle`: none of these, quiet for `board.idle` (orbit.yaml, default 30 minutes; `idle_after_seconds`). Left out unless `idle=1`.
- **`counts`**: `approvals` are the pending actions of its runs other than task decisions; `questions` its open questions to the person (a task conversation's are to its delegating agent, so `0`); `decisions` pending `task:decision` actions plus a ticket plan to approve; `tasks` the tasks it delegated that are not over; `unread` the runs that finished after `read_through`.
- **`preview`** is the newest prose: a message (`from` is `person`, or `agent` for a question or a delegating agent's brief) or a finished run's summary (its report without one), on one line, at most 140 characters, cut at a word. `null` for a conversation with neither.
- `task` is set for a delegated task's conversation (Delegation, below); `age_seconds` is `as_of` minus `last_activity_at`; `last_run` is as in the conversation list.

**Read points.** `POST /conversations/{id}/read` `{"through": "optional RFC 3339 time"}` moves the conversation's `read_through` to `through` (default now), never past now and never back: `200 {"conversation_id": "uuid", "read_through": "..."}`, `404` for an unknown conversation. The body is optional. A thread fetched with `mark_read=1` does the same with the time it was read. A client marks a conversation read while the person is looking at it, with the time of the newest item it showed.

**The change feed.** `GET /events` is Server-Sent Events of small change records, so a client that shows every conversation keeps one stream open instead of one per run:

| event | id | data | meaning |
|---|---|---|---|
| `ready` | the cursor | `{"cursor": "...", "resumed": true}` | first on every connection |
| `change` | its cursor | `{"kind": "run", "conversation": "uuid", "run": "uuid", "cursor": "..."}` | something changed; read what it names again |

- `kind` is `run` (a run created, or its status moved), `message` (a message written), `attention` (an attention row opened or dismissed), `action` (an action proposed or decided), `task` (a task created or moved; sent for the task's conversation and for the delegating one) or `conversation` (created, renamed, archived, or its read point moved). `run` is `null` when the record has none.
- **Resume.** A client that reconnects with `Last-Event-ID` (or `?cursor=`) set to the last id it received gets every record after it, from any instance: `ready` says `resumed: true`. Without one, with an id that is not a cursor, or more than 1000 records behind, the feed starts at the newest record and `ready` says `resumed: false`: read `GET /board` again, then follow the changes.
- Records are written by database triggers, so every writer is covered; a record appears once its transaction is older than every transaction still open, so one committed late is delivered late, never skipped. The feed is read every second; `: ping` every 15 s. It never ends on its own; reconnect after a deploy.
- A record carries no content: a client reads the board, a thread or a run's stream for that.

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

An agent with `chat: true` (the built-in `orbit` is one) is a Claude Code session rather than a report writer: its run's `report` is the session's final message, as written, and `summary` that reply's first line. It changes its memory, asks questions and proposes actions with orbit's tools during the turn instead of in a schema, so its runs carry `questions` and `actions` as any run does; it searches its earlier work and the memory entries it reads with the same tool (`mcp__orbit__search`) other runs have. Each conversation has its own workspace (`GET /conversations/{id}/files`): files the agent writes, and the person's uploads, stay between messages. **Instructions.** In a run that answers its person's message, a chat agent's actions are that person's instructions, as typing into Claude Code is: an action whose rule is `auto`, or `approve` (the person asked for it), executes during the turn and the agent reads its result; the row records `instructed_by` and, for `approve`, `decided_by` as that person. A run nobody's message woke (a schedule, a manual run, a continuation) keeps its rules: `approve` waits. `deny` is never offered. On a worker, which holds no executor credentials, every action waits for control.

**GitHub** (the built-in has `github:clone` auto, `github:pr` and `github:merge` approve): `github:clone {repo, dir}` puts a repository's default branch into the workspace; `github:pr {repo, dir, branch, title, body, push_only}` takes the working tree at `dir` (never its `.git`), commits it as the App's bot user on top of the default branch, holds it to the build's diff policy (no `.github/`, no credential in an added line), pushes `orbit/chat/<branch>` and opens or updates its PR; `github:merge` merges it once every required check passed. Tokens stay in orbit; the repositories are `github.repositories`.

With `sandbox: workstation` (the built-in's, when sandboxd is configured) the session runs in a sandbox container with Claude Code's full tool set and the open internet; `sandbox: build` with the tools it names.

## A conversation's thread

A thread is the person's messages, the agent's questions, and every run
(whatever its trigger) of one conversation. An agent's reply is its run's
`report`, plus any `questions` it asked.

`GET /conversations/{id}/thread?before=<cursor>&limit=<n>&fold=<bool>&mark_read=<bool>`, and
`GET /agents/{name}/thread?before=<cursor>&limit=<n>&fold=<bool>&mark_read=<bool>` for the default
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
   "questions": [{"id": "uuid-of-the-question-message", "body": "...", "open": true},
                 {"id": "uuid-of-a-form", "body": "Which account?", "open": true, "fields": [<field>]}],
   "error": null, "cost_usd": 0.03, "tokens": {"input": 12, "output": 840, "cache_read": 41200, "cache_creation": 3100, "cache_hit_ratio": 0.929773, "num_turns": 3, "model": "claude-opus-5-5"},
   "gate": null,
   "actions": [{"id": "uuid", "type": "notify", "status": "pending", "reason": "...", "params": {}}],
   "tasks": [{"id": "uuid", "key": "facts", "agent": "researcher", "status": "working",
              "conversation_id": "uuid", "depends_on": [], "summary": null, "error": null,
              "decision_action_id": null}],
   "needs_input": true, "changes": {"files": 3, "additions": 40, "deletions": 2},
   "agent_status": {"state": "waiting", "kind": "approval", "since": "...", "source": "run", "run_id": "uuid"}},
  {"kind": "message", "id": "uuid-of-the-question-message", "at": "...", "from": "agent",
   "body": "<a question>", "run_id": "uuid", "open": true},
  {"kind": "message", "id": "uuid-of-a-form", "at": "...", "from": "agent",
   "body": "Which account?", "run_id": "uuid", "open": true,
   "fields": [{"name": "account", "label": "Account", "type": "select",
               "options": ["checking", "savings"], "default": "checking", "required": true}]},
  {"kind": "message", "id": "uuid", "at": "...", "from": "person", "reply_to": "uuid-of-a-form",
   "body": "Answers to \"Which account?\":\n1. Account: savings", "author": "kanishk",
   "answers": {"account": "savings"}, "attachments": [], "...": "as above"}
 ],
 "has_more": true,
 "next_before": "opaque-cursor",
 "read_through": "...",
 "meter": {"run_id": "uuid", "session_id": "...", "context_tokens": 3182, "context_window": 200000,
           "session_cost_usd": 0.07, "cost_usd": 0.07, "runs": 2,
           "tokens": {"input": 17, "cache_read": 8100, "cache_creation": 500, "output": 80}}}
```

- **Read point.** `read_through` beside `items` is the conversation's read point as it was before this request (`null` before the person first read it): a client draws its "New" divider above the first item after it. With `mark_read=1` the request also moves the read point to when the page was read (`POST /conversations/{id}/read`, The board).
- **Meter** (on every page, of the conversation now): `run_id` is its latest finished run and `session_id` that run's backend session, the one the next run continues when it continues one; `context_tokens` is what the model held after its last call in that run (the last assistant message's input, cache read, cache creation and output tokens as the backend reported them; `null` when none reported usage); `context_window` the largest window the run's models reported (Claude Code's `modelUsage`), `null` when none did; `session_cost_usd` the backend's running total for that session. `cost_usd`, `tokens` (summed from each run's reported usage; Codex's cached input counts as `cache_read`) and `runs` are every finished run of the conversation. Before any run finished, the run and session fields are `null` and the totals 0.
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
  question. `reads` (a person's message, once a run has read it) is every attempt that read it, oldest first: `{"run_id", "attempt", "read_at", "released_at"}`, where `released_at` is when a failed or interrupted attempt handed it back, `null` for the one holding it. `steered_into`, present only on a person's message a running reply took mid-turn (`delivery: "steer"`, below), is that run.
- **Runs.** `trigger` is `schedule`, `manual`, `message`, `continue` (the agent asked to be woken again), `build` (a ticket's build run, docs/tickets.md), `learn` (a learning run, below; only in an agent's learning conversation), `task` (in a delegated task's conversation: the task starting, or its parent writing to it), `delegation` (the agent woken because tasks it delegated settled, one failed or one asks it something; below) or `webhook` (a signed delivery to one of its webhook triggers; Webhooks, below). `status` is
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
  `actions` are always arrays. `tokens` is the run's tokens, as on a run (Runs, below), `null` when no attempt reported any.
- **Questions** appear twice by design: in their run's `questions`, and as
  `from: "agent"` message items with the same `id`. A client dedupes by
  `id`, and replies by sending a message with `reply_to` set to it, in the
  same conversation (a `reply_to` from another one is a 400). `open` is true
  until a message names the question in `reply_to` or any later message
  reaches its conversation (the next run there sees the person's words).
- **Forms.** A question may carry `fields`, a form for its answer (absent on a question without one, and on a person's message): each field is `name` (`^[a-z][a-z0-9_]{0,62}$`, what an answer is keyed by), `label` (one line, at most 200 bytes; absent or empty shows `name`), `type` (`string`, `number`, `boolean`, `date` as `YYYY-MM-DD`, `select`, `file`), `options` (a select's choices, 1 to 50, each one line of at most 200 bytes; only on a select), `default` (a valid answer, or absent or `null` for none; never on a file) and `required`. At most 12 fields. Code checks every form when the question is recorded (`forms.Validate`): a run's form that does not validate is dropped and the question kept, so the person answers in words; the chat `ask` tool refuses it, naming why, so the agent can fix it. A client renders the form and replies with `answers` (below). A run's `questions[]` carries the same `fields`. Channels that cannot show a form (mail, ntfy, webhooks) list it under the question as a numbered list: `1. Account (one of: checking, savings; default: checking; required)`.
- **Answers.** A person's reply that answered a form carries `answers`, by field name, as code normalized them (a number as its exact decimal, a date as `YYYY-MM-DD`, a file as its upload's id); absent otherwise. Its `body` is what the person typed followed by `Answers to "<the question's first line>":` and a numbered list, `1. Label: value` (a boolean as `yes` or `no`, a file as `uploads/<name>`, a field left empty as `(no answer)`): that is what the agent reads.
- **Waiting on the person.** `needs_input` on a run is true while one of its
  questions is open or one of its actions is `pending`: the run finished,
  and the agent is waiting for an answer or an approval. It is a state
  distinct from `status`, which says only whether the run itself is done.
  The agents list carries the same flag per agent.
- **Folded skips.** With `fold` (default `true`; `false` lists every run, anything else is a `400`), each streak of consecutive items that are `skipped` runs of the same `trigger`, trigger index and `gate` is one item: the streak's newest run, with `"folded": {"count": 12, "first_at": "...", "last_at": "...", "run_ids": ["oldest", "...", "newest"]}`. `last_at` is the shown run's `at`; every run of the streak is still its own run (`GET /runs/{id}`), and nothing stored changes. Any other item between two skips (a message, another run) ends a streak. `limit`, `has_more` and the cursor count every run, so paging is exact, and a streak that crosses a page boundary is one folded item on each page. `folded` is absent on every other item. The MCP tool `read_thread` folds the same way unless `unfold` is set.
- **Tasks** are the tasks the run delegated (its `delegate` actions), in the order it listed them; always an array. `summary` is the finished task's report summary. For the whole task, and live status, see Delegation below.
- **Actions.** `status` is `pending` (waiting for a person), `approved`
  (by a person or by an `auto` rule, about to execute), `rejected` (by a
  person), `denied` (by rule or invalid; never executes), `executing`,
  `executed` or `failed`.

`POST /agents/{name}/messages`

```json
{"body": "text", "reply_to": "optional message id", "wake": true,
 "attachments": ["optional upload ids"], "delivery": "queue",
 "answers": {"optional": "values for the form of the question reply_to names"},
 "client_message_id": "optional, minted by the client"}
```

posts to the default conversation, and returns
`201 {"message_id": "uuid", "run_id": "uuid", "delivered": "queued"}`. `run_id` is present
when the message woke a run (`wake`, default: the agent's `wake_on_message`).
Stream that run to show the reply as it is written.

`answers` fills the form of the question `reply_to` names: an object keyed by field name, each value the JSON type its field takes (a string for `string`, `date`, `select` and `file`, a number, a boolean). It is checked against the form in the message's transaction: a name the form lacks, a value of the wrong type or outside a select's options, a date that is not a calendar day, a required field left out or empty, `answers` without `reply_to`, a `reply_to` whose question has no form, or a file answer that is not an upload of the conversation is a `400` naming it. A field left out takes its `default`. A file answer's upload is attached to the message (after `attachments`). With `answers`, `body` may be empty; at least one of `body`, `attachments` and `answers` is required.

**Delivery.** `delivery` is `queue` (the default) or `steer` (anything else is 400). A queued message waits for the conversation's next run, as above, even while a run is working. A steered one goes into the reply being written: when the conversation has a running run of a chat agent (one nobody asked to stop), the answer is `"delivered": "steered"` with that run's `run_id`, no run is woken, and the running reply takes the message within a few seconds, at its next tool call (the agent sees it mid-turn and answers it in the same reply); keep streaming that run. Its journal then has a `steer` item, and the thread's message carries `steered_into` (and `run_id`) naming the run. A steered message the reply ends without taking (it made no further tool call, or the person stopped it) is answered by the conversation's next run, which orbit queues when the run ends: its thread message stays without `run_id` until that run starts (a message already written into the turn gets an `orbit.steer_withdrawn` status in the reply's journal). With nothing running, for an agent that is not a chat agent, or with `attachments`, a steer is queued exactly as `queue` would be (`wake` applies) and the answer says `"delivered": "queued"`. `delivered` is always present.

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
| `agent_status` | none | an [agent status](#agent-status) | after every `status`, and on its own when it changes (the run's hooks report a wait, say) |
| `done` | none | the run, identical to a thread run item (`kind: "run"`) | final; the stream ends |

- **`item`** is the stream to render from: one per journal row, revisions included, so a client keeps the highest `revision` per `item_id` (a tool call's result, a plan's update and a turn's end arrive as new revisions of items it already has).
- **`text` and `tool`** are the prose-only view of the same rows, for a client that shows a run as lines: `text` for the first revision of each `message` of role `assistant`, `tool` for the first revision of each `tool_call` (its `name` and `summary`). A row with one is sent as that event followed by its `item`, in one write, both with the row's id. `text` chunks concatenate into the timeline's prose: every chunk after the timeline's first starts with a blank line (`"\n\n"`), so a client appends them verbatim. A model's reasoning is a `thinking` item, not `text`.
- **`status`** carries `epoch` once the run has started (absent while queued without an attempt), and `continues` when the attempt resumed an earlier attempt's session (the attempt it continues). When `epoch` changes from the one a client was showing, the run started over: what the client holds belongs to an abandoned timeline, so it clears, and the new timeline streams from its first row. When only `attempt` changes, the timeline goes on: the client keeps what it has, and the next rows continue it.
- **Replay and resume.** Without `Last-Event-ID`, the stream replays the timeline from its first row, then follows live, so a client that connects late gets everything. A client that reconnects with `Last-Event-ID` set to the last id it received (a browser `EventSource` does this itself) gets only the rows after that cursor, from any instance: the stream is read from the database, so a reconnect that a deploy sends to the new instance continues seamlessly. A cursor into another timeline, past the timeline's last row, or an id that is not a cursor (such as the `<attempt>.<seq>.<index>` ids of earlier releases) means a full replay of the current timeline. `status` and `done` are sent again on every connection.
- **Requeue.** A run handed back (a deploy's drain past its cap, a crash, a stalled turn) streams `status: queued`, then `status: running` with the next `attempt`, and the interrupted turn as a new revision (`state: interrupted`, or `unverifiable` when its worker was lost).
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
 "gate": "unchanged", "steps": 4,
 "tokens": {"input": 12, "output": 840, "cache_read": 41200, "cache_creation": 3100,
            "cache_hit_ratio": 0.929773, "num_turns": 3, "model": "claude-opus-5-5"},
 "prefix": {"sha": "hex", "parts": {"model": "hex16", "tools": "hex16", "mcp": "hex16", "schema": "hex16",
                                    "skills": "hex16", "system": "hex16", "workdir": "hex16", "charter": "hex16"},
            "previous_run": "uuid", "previous_sha": "hex", "changed": ["system"]},
 "agent_status": {"state": "done", "since": "...", "source": "run", "run_id": "uuid"},
 "changes": {"files": 3, "additions": 40, "deletions": 2}}
```

- Always present: `id`, `attempt`, `agent`, `spec_sha`, `trigger`, `prompt` (`""` when none), `status`, `created_at`, `steps`, `agent_status` and `changes` (`null` when it has none). Every other field is omitted when it has no value (not `null`).
- `trigger` and `status` are as in a thread run item. `trigger_index` is set on a `schedule` or `webhook` run: which trigger fired. `scheduled_for` is set on a `schedule` run: the fire time.
- `runner_kind`, `model` and `worker` are the latest attempt's backend, its model (absent when the runner names none) and the instance that claimed it. `spec_sha` is the spec the latest attempt ran under.
- `output` is the backend's raw structured output and `usage` its token usage, as it reported them. `cost_usd` is every attempt's cost together.
- `report` is the submitted `report`, except when the attempt's final assistant prose (its last run of top-level `message` items, as in The journal) holds more: the submitted one was empty, one line where the prose has several, or shorter. Then `report` is that prose, `summary` stays the submitted one, and `output` keeps what was submitted. A chat agent's `report` is its final message.
- `session_id` is the backend session the run ended in. `session_mode` (`fresh`, `resumed`, `live`), `session_parent` (the run whose session it continued) and `session_depth` are set for an agent with a session.
- `run_after`, `continues` (the run that asked) and `chain` (its place in the chain, from 1) are set on a `continue` run.
- `gate` names what ended a `skipped` run: an agent's gate (`unchanged`, `items_above`, `precheck`), `missed` (a schedule fire claimed past its trigger's `missed_grace`), `pushed` (a build), or a learning run's `learn:nothing-new`.
- `steps` is the number of tool calls in the latest attempt's timeline, counted as a thread's run item counts it.
- `tokens` is every attempt's tokens together (absent when none reported any): `input` is the prompt sent uncached, `cache_read` what was read from the prompt cache, `cache_creation` what was written to it, `output` what the model generated. `cache_hit_ratio` is `cache_read / (cache_read + input + cache_creation)`, six places, `null` with no prompt tokens: every turn sends the whole conversation again, so this is what decides a turn's cost. `num_turns` is the backend's own count (Claude Code's `num_turns`; 1 per call for Codex and the API). `model` is the latest reporting attempt's model, the one that did most of the work (by cost when Claude Code ran subagents on others). Claude Code's counts are its result line's per-turn `usage`, split by model from `model_usage_since_last_result` when present (subagents included); Codex counts cached tokens inside `input_tokens`, so its `input` is the difference and `cache_creation` 0. A build records tokens; a run finished before orbit recorded them got its latest attempt's counts from `usage`.
- `prefix` is the stable prefix the latest attempt sent, absent before one was recorded (a skipped run, a build): what comes before the conversation in every request, which the prompt cache reuses only when it is byte for byte the same. `parts` are 16-hex-digit digests of each piece: `model` (the backend and model), `tools` (the built-in tools named), `mcp` (orbit's tool names; a chat run's tools with their descriptions and schemas), `schema` (the output schema), `skills` (the skills plugin's digest), `system` (orbit's system contract), `workdir` (the CLI's working directory, which Claude Code writes into its own system prompt) and `charter` (which leads a fresh session's first message). `sha` covers every part but `charter`, which a resumed session sends after its transcript. `previous_run` is the newest earlier run of the same conversation that recorded a prefix, `previous_sha` its digest, and `changed` the parts that differ from it (`[]` when none did; `null`, with the other two absent, when there is no such run). A `sha` unlike `previous_sha` is a cache miss for everything after the changed part, the whole transcript on a resume.

An attempt (`store.Attempt`), oldest first:

```json
{"attempt": 2, "worker": "orbit/0", "runner_kind": "claude-code", "spec_sha": "hex",
 "started_at": "...", "ended_at": "...", "outcome": "succeeded",
 "session_id": "...", "recovered_from": 1, "recover_skipped": "...",
 "cost_usd": 0.01, "session_cost_usd": 0.04,
 "tokens": {"input": 12, "output": 840, "cache_read": 41200, "cache_creation": 3100, "cache_hit_ratio": 0.929773,
            "num_turns": 3, "model": "claude-opus-5-5",
            "by_model": {"claude-opus-5-5": {"input": 10, "output": 800, "cache_read": 41000, "cache_creation": 3000},
                         "claude-haiku-5-5": {"input": 2, "output": 40, "cache_read": 200, "cache_creation": 100}}},
 "prefix": {"sha": "hex", "parts": {"model": "hex16", "...": "hex16"}}}
```

`attempt`, `worker` and `started_at` are always present; the rest is omitted when unset. `outcome` is `succeeded`, `failed`, `skipped`, `handed_back` (stopped by its instance) or `lost` (reaped without a heartbeat), absent while the attempt runs. `recovered_from` is the attempt whose session it continued, `recover_skipped` why it started over instead. `cost_usd` is the attempt's share, absent when unknown (a killed process saved none); `session_cost_usd` is the backend's running total for the session after it. `tokens` is the attempt's, shaped as a run's with `by_model`, its counts per model when the backend named one (absent when it reported none: a killed process, a skip); `prefix` is its `sha` and `parts` (absent before it sent one).

`POST /runs/{id}/cancel` stops a run. A queued run is cancelled at once: `200 {"cancelled": true, "status": "cancelled"}`. A running run is asked to stop: `202 {"cancelled": false, "status": "cancelling"}`; its attempt sees the request within 2 s (`engine.cancelPoll`), interrupts the backend (a chat agent's turn through Claude Code's control channel, which ends the turn with what it has; any other run's backend is stopped), and ends the run `cancelled`, with the reply so far as its `report` and `error` `"stopped by its person"`. A stopped run notifies nothing. Any other state is 409.

### Events

`GET /runs/{id}/events?attempt=&after=&limit=` is `{"events": [<event>]}`, one attempt's transcript in order: `attempt` (default: the run's latest), the events with `seq` above `after` (default 0), at most `limit` (default 1000, at most 5000). Page by passing the last `seq` as `after`.

```json
{"attempt": 1, "seq": 12, "at": "...", "kind": "assistant", "payload": {}}
```

`payload` is one line of the backend's stream as it sent it (a line that was not JSON is stored as a JSON string). `kind` is the line's `type`, with `.<subtype>` when it has one (`system.init`, `result.success`, `assistant`, `user`, `rate_limit_event`; Codex's `item.started`, `item.completed`, ...), `response` for the Messages API, and `raw` for a line without a type. orbit's own events are `orbit.*`: `orbit.source_fetched` and `orbit.source_failed`, `orbit.transform` and `orbit.transform_failed`, `orbit.gate`, `orbit.session`, `orbit.recovery`, `orbit.stalled`, `orbit.figures_unverified`, a chat turn's `orbit.steer` (`{"message_id", "text", "author"}`: it took a steered message) and `orbit.steer_withdrawn` (`{"message_id", "confirmed"}`: a steered message written into the turn and not taken was withdrawn when it ended; `confirmed` false means the backend did not confirm it, and its process was closed), a build's `orbit.build`, `orbit.git`, `orbit.sandbox`, `orbit.permission` and `orbit.permission_denials`, and a run's connector steps, `orbit.connector` (Connectors, below), with `orbit.permission` for each permission prompt of a connector tool.

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
| `approval` | `approval:<request id>` | `{"request_id": "...", "tool": "Bash", "input": {...}, "state": "pending", "message": "..."}`; the backend asking orbit whether a tool call may run: a build's tool under its policy, or a connector tool that waits for approval (`mcp__<connector>__<tool>`). Revision 1 is the request (`pending`); the decision is the next, `allowed` or `denied`, with orbit's `message`. What a person approves is the run's pending `connector:` action (Connectors), not this item. |
| `question` | `question:<a>.<s>.<i>` (a question tool's: `question:<call id>.<n>`) | `{"text": "Which card?", "header": "...", "options": [{"label": "...", "description": "..."}], "multi_select": true}`; something the agent asked its person: a question in its final answer (`options` empty), or a question tool's, with what it offered. The thread's question messages are the ones a person replies to. |
| `steer` | `steer:<message id>` | `{"message_id": "uuid", "text": "...", "author": "kanishk"}`; a person's message the turn took while it worked (`delivery: "steer"`), from the point the backend started on it: what follows answers it too. `author` is absent when none was recorded. |
| `status` | `status:<a>.<s>.0` | `{"code": "orbit.source_fetched", "detail": <bounded>}`; something that happened to the run that is not the model's output: orbit's steps (`orbit.*`, as in Events), the backend's session start (`system.init`), a rate limit report (`rate_limit_event`), a line that was not JSON (`raw`). `detail` is the event's payload. |
| `turn` | `turn:<a>.<n>` | below |

**Turns.** A turn is one exchange with the backend, from the request to its result. It opens at the backend's first event (Claude Code's `system.init` or first message, Codex's `turn.started`, a Messages API response) and closes at its result; a live process answers several runs, one turn each.

```json
{"state": "completed", "outcome": "success",
 "started_at": "...", "completed_at": "...", "duration_ms": 4200,
 "cost_usd": 0.02, "usage": {}, "error": "..."}
```

- `state` is how the turn stands as orbit observed it: `running`; `completed` (the backend answered); `interrupted` (orbit saw its process end without a result: a timeout, a crash, a drain's hand-back, or orbit ended it, said by `reason`); `unverifiable` (its attempt was lost, reaped without a heartbeat, and nothing observed how it ended).
- `reason`, on an `interrupted` turn orbit ended itself, says why: `stalled` (the watchdog: the CLI wrote nothing, not even a keep_alive, for the agent's `session.stall` with no tool call running; an `orbit.stalled` event `{"idle_ms", "stall_ms"}` follows the turn's last line, and the run is handed back for its next attempt to continue the session, or fails on its last attempt). Absent otherwise.
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

`GET /actions?agent=&status=&run_id=&before=<cursor>&limit=<n>` is `{"actions": [<action>], "has_more": false, "next_before": null}`, newest first. `status` is as in a thread action; `run_id` narrows to one run's proposals (400 when it is not a UUID). Paging is as for a thread: `next_before` is an opaque cursor, `null` exactly when `has_more` is false; `limit` defaults to 100, at most 500. `GET /actions/{id}` is `{"action": <action>, "preview": {...}}`. `preview` is what a `pending` action would do if it executed now, computed by orbit for the person deciding: for the schedule actions, the shape in Defining agents above; for `agent:define`, a dry run of the change, `{"agent": "morning", "change": "create", "widens": ["tool WebFetch", "source news-example-com"], "diff": "+ {\n+   \"name\": \"morning\",\n..."}`, where `change` is `create` or `update`, `widens` is what it adds beyond the agent's current version (the same list that holds an `agent:define` for a person), and `diff` is every line of the spec as indented JSON, prefixed `  ` (unchanged), `- ` (removed) or `+ ` (added); it is omitted for a decided action and a type without one, and replaced by `preview_error` (a string) when it would fail now. The approval page shows it too.

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
- **Approve and always allow.** `POST /actions/{id}/approve` with `{"always": true}` (and optionally `client_request_id`) approves the action and also writes the next version of its agent with the action's type under `auto`, through the same path as `PUT /agents/{name}` (`change: "update"`, the decider as `author`, no `run_id`; reverted like any version). `200 {"ok": true, "always": {"agent": "news", "type": "webhook:home-assistant", "rule": "auto", "version": 5}}`. The new version is validated first: an agent in the configuration files is `409` and decides nothing, as is a type already `auto` for the agent (such an action waits for another reason: an `agent:define` that widens an agent always waits for a person) and an agent changed meanwhile. A retry with the same `client_request_id` returns `{"ok": true}` without `always` and changes nothing again. `always` on `/reject` is `400`. A client shows the button beside Approve for a pending action whose agent is not a file agent (`origin` on the agent row).

## Connectors

MCP servers orbit calls for agents (orbit.yaml `connectors`, docs/agents/configuration.md, Connectors). No response carries a credential.

`GET /connectors` is every configured connector:

```json
{"connectors": [{"name": "housing-watch", "transport": "http", "url": "https://.../mcp", "description": "...",
  "auth": "oauth", "write_tools": ["create_view"], "agents": ["housing"], "connected": true,
  "token": {"connector": "housing-watch", "version": 2, "expires_at": "...", "reason": "refresh",
            "created_by": "orbit", "created_at": "..."}}]}
```

`auth` is `none`, `header` or `oauth`. `connected` is true for `none` and `header`, and for `oauth` once a person has signed in; `token` is the newest stored version of an `oauth` connector's grant (`reason` `authorize`, a person's sign-in, `created_by` that person; or `refresh`, by `orbit`), `null` before the first sign-in and for other kinds. `expires_at` is omitted when the server gave no expiry.

`GET /connectors/{name}/tools` lists the server's tools now, `{"tools": [{"name": "find_listings", "description": "...", "inputSchema": {}, "read_only": true, "write": false}]}`; `read_only` is the server's `readOnlyHint`, `write` whether the tool is in `write_tools` (so it waits for approval unless an agent's rules say otherwise). `404` for an unknown connector, `502` when the server cannot be listed (unreachable, or not signed in).

`POST /connectors/{name}/authorize` takes no body and starts the caller's sign-in to an `oauth` connector (the operator token signs in as `operator`): orbit reads the server's protected resource and authorization server metadata, registers itself as a client unless `auth.client_id` is set, and returns `{"authorize_url": "https://..."}`, which the client opens in a browser within 15 minutes. The authorization server sends the browser to `GET /api/v1/connectors/oauth/callback`, which takes no token (its sealed `state` is the credential), stores the token as the connector's next version and shows a page saying it is connected, or why not. `404` for an unknown connector, `409` for one that is not `oauth`, `502` when discovery or registration fails or the host has no `ORBIT_CONNECTOR_KEY`.

**Calls.** A run's connector calls are events (`orbit.connector`, payload by `step`): `list` (`connector`, `tools`, `offered`, `approve`: the tools waiting for approval, `skipped`: names orbit cannot offer; or `error` when the connector was left out), `call` (`connector`, `tool`, `tool_use_id`, `rule`, `duration_ms`, `is_error`, `result`: the whole MCP result as a bounded value, `file` when it was written to the run's inputs; or `error`), `proposed` (`action_id`) and `waiting` (`action_id`, `tool_use_id`, a call waiting for its person in the chat). Each run stores the tools it was offered as the artifact `connectors/<name>.tools.json`, and each result written to a file as an artifact of that path.

**Actions.** A call to a tool that waits for approval is the action `connector:<connector>/<tool>`, recorded while the run is still running, with `params` `{"arguments": {...}}` and `rule` `approve`; it appears in the run's thread `actions` and in `/attention` at once. Approving it (`POST /actions/{id}/approve`) runs the call once: within the run's turn when the run answers its person's message and is still waiting, otherwise by the action loop. Approving with `always` makes the agent's `mcp:<connector>:<tool>` rule `auto`, not the action type. An `auto` call to a tool in `write_tools` is recorded the same way, `rule` `auto`, already `executing`, so it is a record rather than a decision. `result` is `{"content": [<MCP content blocks>], "is_error": false}`, or for a result over 1 MiB `{"is_error": false, "truncated": <bounded>}`; a call the tool reported as an error ends `failed`.

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

A ticket is a conversation with a lifecycle ([tickets.md](tickets.md)): `POST /tickets` (opens its conversation; returns `ticket_id`, `conversation_id`, `message_id`, `run_id`; `stack_on` files a request on top of another, tickets.md, Stacked tickets; `race` and `variants` build it two or three times, tickets.md, Races), `GET /tickets`, `GET /tickets/{id}` (the ticket, its plans and transitions; a ticket that reached merging carries `merge_action_id`, its `github:merge` action), `POST /tickets/{id}/plans/{version}/approve`, `POST /tickets/{id}/{abandon,retry,reopen}`, and `POST /tickets/{id}/checks/rerun` (`202`; the ticket loop reruns the PR's failed Actions jobs on its next tick, and a failed ticket goes back to review; `GET /tickets/{id}` lists every rerun in `reruns`), and review comments on its PR: `GET`/`POST /tickets/{id}/comments`, `POST /tickets/{id}/comments/{comment}/{edit,withdraw}` and `POST /tickets/{id}/review`, which sends the drafts as one message (tickets.md, Review comments). Its thread and messages are its conversation's (`/conversations/{conversation_id}/thread`, `/messages`); a message there reopens a `planned` ticket, and a `merged`, `done` or `abandoned` one refuses messages (409). A ticket's conversation carries `ticket_id` in the conversation list. Filing and approving take `client_request_id`; the author and approver are the token's person. A transition the ticket's state does not allow is 409.

`GET /tickets?status=&agent=&before=&limit=` is `{"tickets": [...]}`, newest first. `status` and `agent` match exactly; `before` is an RFC 3339 time and returns tickets created before it (400 if it does not parse), so the last ticket's `created_at` is the next page's cursor. `limit` defaults to 50, at most 500. Each ticket:

```json
{"id": "uuid", "kind": "request", "title": "...", "agent": "builder",
 "conversation_id": "uuid", "repo": "kensac/orbit", "status": "review",
 "author": "kanishk", "approved_plan": 1, "base_sha": "...", "branch": "orbit/1a2b3c4d",
 "pr_number": 61, "head_sha": "...",
 "created_at": "...", "updated_at": "...", "merge_action_id": "uuid"}
```

`repo`, `author`, `approved_plan`, `base_sha`, `branch`, `pr_number`, `head_sha`, `merge_sha`, `error`, `merge_action_id`, `stack_on`, `race` and `picked_lane` are omitted when unset; `error` is set only while the ticket is `failed`. `GET /tickets/{id}` is `{"ticket": <as above>, "plans": [...], "events": [...], "reruns": [...]}`, and for a race also `"race": {"lanes": [<lane>], "recommended": "b", "picked": "a"}` (below).

### Races

`POST /tickets` with `"race": 2` or `3` and optionally `"variants": [{"model": "opus", "effort": "high"}, ...]` (at most one per lane; lane `a` takes the first; `model` matches `^[a-z0-9][a-z0-9.-]{0,62}(\[[a-z0-9]{1,8}\])?$`, `effort` is one of `low`, `medium`, `high`, `xhigh`, `max`; an unset field keeps the build agent's runner's) files a request that builds its approved plan once per lane, each on `orbit/<id>-<lane>` with its own PR. 400 for a race outside `2..3`, more variants than lanes, a bad variant, variants without a race, a race on a question, or a race with `stack_on` (and nothing may stack on a race). **409 while the subscription is nearly spent**: any usage window not yet reset above `usage.DeferLowAbove` (0.8, `GET /usage`), whether or not `defer_low_priority` is on; the error names the window, its reading and its source. A plain request is not refused.

Until a person picks, the ticket has no `branch`, `pr_number` or `head_sha`; `race.lanes` is every lane, best first, each scored by code:

```json
{"lane": "b", "variant": {"effort": "high"},
 "run_id": "uuid", "run_status": "succeeded", "branch": "orbit/1a2b3c4d-b",
 "pr_number": 62, "base_sha": "...", "head_sha": "...",
 "added": 30, "deleted": 4, "files": ["internal/x/x.go", "internal/x/x_test.go"],
 "diff_policy": "passed", "cost_usd": 1.42,
 "checks": "green", "checks_head": "...", "pr_state": "open", "checks_at": "...",
 "delivered": true, "checks_green": true, "lines_changed": 34, "tests_touched": 1,
 "rank": 1, "picked": false, "why": "checks green"}
```

`run_*`, `branch`, `pr_number`, `base_sha`, `head_sha`, `added`, `deleted`, `diff_policy` (`passed` or `failed`) and `cost_usd` are the lane's newest build's and omitted until it has them; `files` is always a list. `checks` (`pending`, `green`, `failed`), `checks_head`, `pr_state` (`open`, `closed`, `merged`), `checks_at` and `pr_closed_at` are the ticket loop's latest reading of the lane's PR. `why` is the reason for its place in one phrase. `recommended` is the first lane when it delivered a PR, omitted otherwise; `picked` once a person picked.

`POST /tickets/{id}/pick` `{"lane": "a", "client_request_id": "..."}` is the person's pick (`by` counts only with the operator token): `{"ok": true, "action_id": "uuid"}`, the `ticket:pick` action recorded on the lane's build run with rule `approve`, approved and executed by that person in the same transaction (no rule makes it automatic and no agent is offered it). The ticket stays `review` and takes the lane's `branch`, `pr_number`, `base_sha` and `head_sha`, then goes on like any ticket (checks, fixes, revisions and the merge are that lane's); the loop closes the other lanes' PRs and keeps their branches. 400 for a lane that is not `a`, `b` or `c`; 409 unless the ticket is a race in `review` with no pick and the lane's newest build delivered a PR.

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
 "outcome": "succeeded", "outcome_reason": null,
 "attempt": 2, "failures": 1, "stalled_at": null,
 "attempts": [{"attempt": 1, "agent": "researcher", "conversation_id": "uuid", "brief": "Find the facts.",
               "error": "the agent could not do the work: ...", "finished_at": "...", "retry_action_id": "uuid"}],
 "attempt": 2, "failures": 1,
 "attempts": [{"attempt": 1, "agent": "researcher", "conversation_id": "uuid", "brief": "Find the facts.",
               "error": "the agent could not do the work: ...", "finished_at": "...", "retry_action_id": "uuid"}],
 "questions": [{"id": "uuid", "body": "Which year?", "open": true}],
 "created_at": "...", "updated_at": "...", "finished_at": "..."}
```

- `status`: `waiting` (a dependency is not done; a failed dependency holds it until that one is retried or this one cancelled), `blocked` (its decision is pending), `working` (a run in its conversation is queued or running), `needs_input` (its agent asked the delegating agent something, `questions`, each with `fields` when it has a form as in a thread, or said it is blocked, `outcome`), `done` (`report` is the reply of the run that finished it), `failed` or `cancelled` (`error` says why; a task whose run said it could not do the work keeps that run's reply in `report`). A message from the delegating agent moves a `done` task back to `working`; `report` keeps the previous reply until the next.
- `depends_on` is the keys of the tasks it waits for: of its batch (`action_id`), or of an earlier task its conversation delegated (the newest of that key when the task was created). `decision` is `null` without one; otherwise approve or reject its `action_id` like any action (`POST /actions/{id}/approve`), and the task starts or is cancelled within seconds.
- `outcome` and `outcome_reason` are what the newest run in its conversation said of its work: `outcome` is `succeeded`, `failed` (it could not do the work; the task fails) or `blocked` (it cannot go on until something outside its reach changes; the task needs input), `outcome_reason` why, or `null` when it gave none. Both are `null` while that run has not finished, and for a run that gave no outcome (a chat turn, which counts as succeeded).
- `attempt` is its current attempt: 1, and one more for each `task:retry` of the delegating agent, which reopens a failed task on the same row, with a new brief or another agent (`agent` and `conversation_id` are then the current attempt's). `attempts` are the attempts a retry ended, oldest first, as they were then. `failures` counts attempts that failed by the agent's work (not a run lost with its worker); from 3 a retry waits for a person's approval, which shows as a pending `task:retry` action.
- `stalled_at` is set while the task is open in a batch none of whose open tasks can move (each waits only on a failed dependency, or on a decision pending longer than `defaults.decision_stall`) and the delegating agent was woken for it, once; `null` otherwise.
- `attempt` is its current attempt: 1, and one more for each `task:retry` of the delegating agent, which reopens a failed task on the same row, with a new brief or another agent (`agent` and `conversation_id` are then the current attempt's). `attempts` are the attempts a retry ended, oldest first, as they were then. `failures` counts attempts that failed by the agent's work (not a run lost with its worker); from 3 a retry waits for a person's approval, which shows as a pending `task:retry` action.
- `run_id` and `run_status` are the newest run in its conversation; stream it with `/runs/{id}/stream` to show the task's work as it happens. `fresh` is a worker started from its agent's spec without its memory. `depth` is 1 for a task a top-level conversation delegated.

`POST /tasks/{id}/cancel` `{"reason": "optional"}`: `200 {"cancelled": true}`, `409` when the task is already over. Its queued runs are cancelled (a running one finishes, unheeded) and its dependents with it. The canceller is the token's person (with the operator token, `"by"` names it).

`GET /conversations/{id}/tasks/stream` is Server-Sent Events: a `tasks` event (`{"tasks": [<task>]}`, as the list) on connect and again whenever any task of the conversation changes, and `: ping` every 15 s. It never ends on its own; reconnect after a deploy (there is no `Last-Event-ID`: the first event is the whole state). A chat UI shows the delegating run's tasks from its thread item, then keeps them live from this stream, and opens a task's `conversation_id` to show its work.

## Workspace

**Partly built.** The contract the portfolio's workspace builds against (design: [workspace.md](workspace.md)). Built: finding a workspace, snapshot reads (W1), snapshot writes (W3), history, the change stream and per-reply changes (W6), the audit, and terminals in a box. Every part marked *(planned)* is not built yet and answers `404`; it loses the mark in the PR that implements it.

A workspace is a file tree a person and an agent share. Routes take the authentik token as above; writes, trash, uploads, terminals and git writes also require membership of `ORBIT_WORKSPACE_GROUP` (default `ORBIT_OIDC_GROUP`; `403`, `check: "group"`). Errors carry a `check` beside `error`, as authentication errors do:

| `check` | status | meaning |
|---|---|---|
| `request` | 400 | a malformed parameter: a cursor not from `next_cursor` or `next_before` |
| `path` | 400 | not clean (`a/`, `./a`, `a//b`), absolute, outside the tree, or through a symlink; the wrong kind for the route (a directory to `files`, a file to `tree`, a symlink to `files` or `download`) |
| `not_found` | 404 | no such path, ref, run, terminal or comment |
| `no_workspace` | 404 | a run whose conversation has no workspace (`GET /runs/{id}/changes`) |
| `exists` | 409 | the target of a create, rename, restore or upload exists (the body carries `current`, the entry there, for a write or an upload), or a signed upload URL was used |
| `version` | 412 | `If-Match` is not the current version; the body carries `current` (the entry, or `null` when it is gone) |
| `version_required` | 428 | a write to an existing file without `If-Match` |
| `group` | 403 | a write by a person outside `ORBIT_WORKSPACE_GROUP` |
| `read_only`, `no_shell`, `no_git` | 409 | the backend does not have that capability (`capabilities`, below) |
| `busy` | 409 | a snapshot write while a run of the conversation is running |
| `too_large` | 413 | over a size limit (workspace.md, Auth, scope and limits) |
| `limit` | 409 | a count limit (terminals, clients) |
| `rate` | 429 | over a rate, with `Retry-After` |
| `box_starting` | 503 | the box is starting, with `Retry-After` |

### Finding a workspace

`GET /conversations/{id}/workspace` and `GET /agents/{name}/workspace` return `{"workspace": <workspace>}`, one shape over both backends. `GET /agents/{name}/workspace` is the agent's box for an agent with one (Terminals, below), and otherwise its snapshot workspace. `GET /conversations/{id}/workspace` is the conversation's snapshot workspace, also for an agent with a box until its chat turns run in the box (W10).

```json
{"id": "uuid", "scope": "conversation", "conversation_id": "uuid", "agent": "orbit",
 "backend": "snapshot", "cwd": "",
 "capabilities": {"read": true, "write": true, "terminals": false, "git": false,
                  "watch": false, "presets": []},
 "as_of": {"snapshot_id": 4, "run_id": "uuid", "at": "..."},
 "box": null, "created_at": "..."}
```

- A conversation of a chat agent (`chat: true`) has a workspace; it is made the first time it is asked for, or with the conversation's first snapshot. For an agent without a box, `GET /agents/{name}/workspace` is the agent's default conversation's workspace, where its agent-level routes speak.
- `scope` is `conversation` (the snapshot backend: one per conversation) or `agent` (a box: one per agent, shared by its conversations). From a conversation, `cwd` is that conversation's directory in the tree (`""` on the snapshot backend, `conversations/<id>` in a box); a client opens there and lets the person go up. From an agent, `cwd` is `""`.
- `backend` is informative; a client decides from `capabilities`. `presets` lists the terminal presets (`shell`, `claude`). `capabilities` is the backend's, narrowed for the person asking: `write` is false for a person outside `ORBIT_WORKSPACE_GROUP`. On the snapshot backend it is `{"read": true, "write": true, "terminals": false, "git": false, "watch": true, "presets": []}`. In a box today it is `{"read": false, "write": false, "terminals": true, "git": false, "watch": false, "presets": ["shell", "claude"]}`, until the box serves files and git (W10, W13).
- `as_of` is the snapshot read (snapshot backend), `null` before the first one and in a box. Its `run_id` is `null` when a person's write stored it.
- `box` is `{"host": "oci", "arch": "arm64", "state": "running", "image": "sha256:..."}` in a box (`null` on the snapshot backend), `state` one of `starting`, `running`, `stopped`. A request to a stopped box starts it, waits up to 30 s, then answers `box_starting`. Built: `host` and `arch` are sandboxd's docker host's, `image` the reference the container runs (`ghcr.io/kensac/orbit-workstation@sha256:...`), `state` `running` or `stopped` (also before the box's first start); reading the workspace does not start the box, creating or attaching a terminal does, and `box_starting` is not used yet.
- A conversation of an agent without chat workspaces returns `{"workspace": null}`.
- Every conversation shape above carries `workspace_id` (the same id): set for each conversation of a chat agent and any conversation with a snapshot, `null` otherwise.

### Files

Every path is a query parameter, `path`, relative to the root. An entry:

```json
{"path": "src/main.go", "name": "main.go", "kind": "file", "bytes": 1342,
 "mode": "0644", "modified_at": "...", "version": "sha256:<hex>",
 "target": null, "ignored": false, "git": "M", "has_children": null}
```

`kind` is `file`, `dir` or `symlink` (`target` its link text, `null` for the others; a symlink is never followed, so a path through one is a `path` error). `bytes` is a file's size, `0` for the others. `mode` is the permission bits in octal. `version` is for files only, and left out of the entry otherwise. `ignored` is from the `.gitignore` files of the tree, as git decides (on the snapshot backend there is no global or `info/exclude` file). `git` is the porcelain status letters in a box (`null` outside a repository and on the snapshot backend). `has_children` is for directories, `null` for the others. orbit's own state (`.orbit/`, a run's `inputs/`) is not in the tree: such a path is `not_found`.

Reads are limited to 100 requests a second per person per workspace (per orbit instance), with a burst of as many; over it is `429` (`rate`) with `Retry-After` in seconds.

- `GET /workspaces/{id}/tree?path=&cursor=&limit=` lists one directory: `{"path", "entries": [<entry>], "next_cursor": null}`, directories first, then by name (bytewise); `limit` defaults to 500, at most 2000 (a larger one is clamped); `next_cursor` is opaque, `null` on the last page. `version` is omitted from tree entries (it costs a read of every file). An empty workspace's root is an empty list.
- `GET /workspaces/{id}/stat?path=` is `{"entry": <entry>}`, with `version`.
- `GET /workspaces/{id}/files?path=&ref=` is a file for the editor: `{"entry": <entry>, "text": "...", "reason": null}`. `text` is the whole file as UTF-8, byte for byte (line endings and a BOM kept). When it cannot be edited, `text` is absent and `reason` is `binary` (a NUL in the first 8000 bytes, or not UTF-8) or `too_large` (over 2 MiB, checked first). `ref` reads a past version: a `ref` from `history` or from a run's `before` or `after` (below); one that is not this workspace's is `not_found`. The response's `ETag` is the version, quoted.
- `PUT /workspaces/{id}/files?path=` writes the body (raw bytes, at most 8 MiB, else `too_large`) as the file. `If-Match: "<version>"` is required for an existing file (`version_required` without it; `If-Match: *` accepts any version); `If-None-Match: *` creates and fails `exists` if it is there; an `If-Match` on a path that is not there is `version` with `current: null`. Parent directories are created; a path under a file, a directory or a symlink at the path, the root and orbit's own state are `path` errors. An existing file keeps its mode, a new one is `0644`. `200` (or `201` created) `{"entry": <entry>}`.
- `POST /workspaces/{id}/mkdir` `{"path"}`: `201 {"entry"}`, parents created; `exists` when the path is there.
- `POST /workspaces/{id}/rename` `{"from", "to", "version"?}`: `200 {"entry"}` at `to`, with everything under a directory. Never overwrites (`exists`); `version` guards a file's content as `If-Match` does (given for a directory, it is `version`). A move into itself or of the root is a `path` error.
- `POST /workspaces/{id}/trash` `{"path", "version"?}` moves a file or directory to the trash: `200 {"trash": <item>}`; `version` as for rename. `GET /workspaces/{id}/trash?cursor=&limit=` lists items, newest first: `{"items": [...], "next_cursor"}` (`limit` defaults to 100, at most 500). `POST /workspaces/{id}/trash/{item}/restore` `{"to"?}` (the body is optional) puts it back at its path or `to`, with everything that was under it: `200 {"entry"}`, `exists` when the target is there. An item may be restored more than once (a copy each time); `restored_at` is the latest. There is no route that empties the trash: on the snapshot backend an item's content is the snapshot it was trashed from, kept like every snapshot. An item's `bytes` is the total of its files; `by` is the person, `null` for the operator token.

```json
{"id": "uuid", "path": "notes/old.md", "kind": "file", "bytes": 210,
 "trashed_at": "...", "by": "kanishk", "restored_at": null}
```

- `POST /workspaces/{id}/upload?dir=` takes `multipart/form-data`, any number of file parts, each at most 64 MiB (`too_large`), named by the part's file name (cleaned as conversation uploads are, without numbering), into `dir` (created when missing; default the root), all in one write. An existing name is `exists` unless `overwrite=1`, which trashes the old file first (a directory there is always `exists`); a name twice in one form is a `request` error. `201 {"entries": [<entry>]}`, in form order.
- `POST /workspaces/{id}/upload/url` `{"path", "bytes", "media_type"?}` returns a signed `PUT` URL shaped as `POST /conversations/{id}/uploads/url`'s (`{"url", "method": "PUT", "headers", "expires_at", "max_bytes"}`), for `bytes` up to 1 GiB, writing to `path`; `exists` applies when it is issued and when it is used. The URL is `/api/v1/workspaces/signed?...`: no `Authorization` header (the signature, under a key of its own derived from `ORBIT_API_TOKEN` (`secret.WorkspaceUploadURL`), names the workspace, the path, the type, `bytes` as the most it accepts, the person, who must be in the workspace group when it is issued, and an expiry 15 minutes ahead). It uploads once (a second `PUT` is `409` `exists`); an altered or expired URL is `403`, a larger body `413` `too_large`. `media_type` sets only the `Content-Type` the browser sends. It needs `public_url` in `orbit.yaml` (`503` without it).
- `GET /workspaces/{id}/download?path=&ref=` is a file as a download (`application/octet-stream`, `Content-Disposition: attachment`, `nosniff`, a CSP that runs nothing, its version as the `ETag`), or a directory as `application/gzip` (a tar.gz named `<directory>.tar.gz`, `workspace.tar.gz` for the root, whose paths start with the directory's name; `.orbit/` left out), at most 1 GiB. `inline=1` serves a raster image (not SVG, as its content sniffs) inline with its own type, for a preview; any other file stays an attachment. A symlink is a `path` error. `ref` is as for `GET /files`.

On the snapshot backend a write, mkdir, rename, trash, restore or upload stores a new snapshot (`source: person`, `run_id` null), made from the newest and applied again from the newest when another write got in first; it is refused with `busy` while a run of the conversation is running, since the run's own snapshot would follow it. `as_of` moves to it. The whole snapshot holds at most 256 MiB (`too_large`), so a signed upload larger than that is refused when it is asked for. Writes are limited to 20 a second per person per workspace (per instance), as reads are. A request's `X-Request-Id` (up to 128 printable characters; one is made when it has none) is echoed in the response and recorded with its events.

The next turn of the conversation's agent is told, by code, which files the person changed since the agent last saw the workspace (added, modified, removed, renamed; at most 100 lines), and its working directory has the person's version: a live session process, whose sandbox holds the old copy, is not reused, and the session resumes in a new one started from the workspace as it is.

### History

`GET /workspaces/{id}/history?path=&before=&limit=` is a file's versions, newest first: `{"versions": [{"ref": "opaque", "version": "sha256:<hex>", "at": "...", "by": {"kind": "run", "run_id": "uuid", "person": null, "terminal_id": null}, "source": "snapshot"}], "next_before": null}`. `source` is `snapshot`, `checkpoint` (a box's per-turn record) or `git` (a commit; `rev` is set). `by.kind` is `run`, `person`, `terminal` or `unknown` (a change seen between turns that nothing attributed). Read one with `GET /workspaces/{id}/files?path=&ref=`.

On the snapshot backend a version is each snapshot that left a file at `path` different from the one before it (created, modified, renamed there or restored), so a snapshot that changed only other paths adds none; `source` is always `snapshot`, and `by` is the run or the person that stored it (`person` `null` for the operator token). `path` is a file's, not the root (`path`); a path that never held a file has no versions. `limit` defaults to 50, at most 500; `next_before` is opaque, `null` on the last page.

### The change stream

`GET /workspaces/{id}/changes?path=` is Server-Sent Events for a subtree (default the root):

| event | id | data |
|---|---|---|
| `ready` | cursor | `{"backend": "box", "cursor": "..."}`, first on every connection |
| `change` | cursor | `{"path", "op": "create", "kind": "file", "from": null, "version": "sha256:<hex>", "by": {"kind", "run_id", "person", "terminal_id"}}`; `op` is `create`, `modify`, `trash`, `restore` or `rename` (`from` set) |
| `resync` | none | `{"reason": "cursor_lost"}` or `"overflow"`: re-read what is shown, then carry on |

`Last-Event-ID` resumes from a cursor; a cursor the serving instance no longer holds sends `resync`. `: ping` every 15 s. Changes made through this API and by runs are attributed; a change a terminal process made is `terminal` when one terminal was in the directory's session, else `unknown`.

On the snapshot backend the stream follows the conversation's snapshots (it looks for new ones every second): each snapshot's changes against the one before it, in path order, one event per path, every path under a directory that came or went included. `version` is a file's after the change, `null` for a trash and for directories and symlinks. A file that left one path and arrived at another with the same content in one snapshot is a `rename`; a path whose kind changed is a `trash` then a `create`; what a person's restore brought back is `restore`. `by` is the snapshot's run (`kind: "run"`) or person (`kind: "person"`, `person` `null` for the operator token). A connection without `Last-Event-ID` starts at the newest snapshot and replays nothing. A cursor is opaque; one of another workspace or not a cursor at all sends `ready` at the newest snapshot, then `resync` (`cursor_lost`); resuming more than 1000 changes behind sends `resync` (`overflow`) and carries on from the newest. `path` narrows to changes whose `path` or `from` is in that subtree. Opening the stream spends one read. It never ends on its own; reconnect with `Last-Event-ID` after a deploy.

### A reply's changes

Thread run items and runs carry `changes`: `{"files": 3, "additions": 40, "deletions": 2}`, `null` for a run with no workspace and for one not finished yet. It counts every regular file the run changed and every line added and removed, whatever the caps below leave out of the diff; it is computed once, the first time a finished run is read.

`GET /runs/{id}/changes?path=` is what the run changed in its workspace:

```json
{"run_id": "uuid", "workspace_id": "uuid",
 "before": {"ref": "opaque", "at": "..."}, "after": {"ref": "opaque", "at": "..."},
 "files": [<diff file>], "truncated": false}
```

`before` and `after` are the snapshots (snapshot backend) or checkpoints (box) at the run's start and end; read a file as it was with `GET /workspaces/{id}/files?path=&ref=`. On the snapshot backend `after` is the newest snapshot the run stored and `before` the one before its first; a run that stored none changed nothing, and both are the snapshot the workspace was at when it finished (for a running run, the newest). Either is `null` while the workspace has no snapshot. A run of an agent without a workspace is `404` (`check: "no_workspace"`); its journal's `diff` items are what it changed. An unknown run is `404` (`not_found`). `path` narrows to one file (its path after, or before for a deleted one) without the per-file line cap. The diff covers regular files, by path, as `changes` counts them; a request spends one read of the workspace.

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

`status` is `added`, `modified`, `deleted`, `renamed` or `mode`. `text` is the line without its newline. Line numbers are 1-based on each side (`old`, `new`; `null` on the side a line is not on), hunks carry 3 lines of context and merge when closer than 6, and `header` is git's default (the nearest line above the hunk starting with a letter, `_` or `$`, at most 80 bytes). A binary file (a NUL in its first 8000 bytes on either side) has `binary` true and no hunks; a rename on the snapshot backend is content-identical, so it has none either; `old_path` is set for a rename. Caps: 300 files and 10 MiB per response, 5000 lines per file (`truncated` on the file or the response); a truncated file is read whole with `path`.

### Git (box only)

*(planned)* `capabilities.git` false answers `no_git`. `repo` is a repository's path in the tree (a directory with `.git`, found up to three levels deep).

- `GET /workspaces/{id}/git/repos`: `{"repos": [{"path": "orbit", "branch": "main", "head": "sha", "upstream": "origin/main", "ahead": 1, "behind": 0, "remote": "https://github.com/kensac/orbit"}]}`, at most 50.
- `GET /workspaces/{id}/git/status?repo=`: `{"repo", "branch", "head", "files": [{"path", "orig_path": null, "index": "M", "worktree": ".", "conflict": false}]}` (porcelain v2 letters, `.` for unchanged).
- `GET /workspaces/{id}/git/diff?repo=&base=HEAD&head=worktree&path=&context=3&staged=false`: `{"base": {"rev": "HEAD", "sha"}, "head": {"rev": "worktree", "sha": null}, "files": [<diff file>], "truncated"}`. `base` is a revision or `merge-base:<rev>` (what the branch changed since it left `<rev>`); `head` is `worktree` (default), `index` or a revision; `path` narrows to one file or directory.
- `GET /workspaces/{id}/git/log?repo=&rev=HEAD&path=&before=&limit=`: `{"commits": [{"sha", "parents": [], "author": {"name", "email", "at"}, "subject", "body"}], "next_before"}`, `limit` default 50, at most 500.
- `GET /workspaces/{id}/git/show?repo=&rev=&path=`: the file at a revision, shaped as `GET /files`.
- `GET /workspaces/{id}/git/branches?repo=`: `{"branches": [{"name", "sha", "upstream", "current"}]}`.
- `POST /workspaces/{id}/git/commit` `{"repo", "message", "paths": [], "all": false}`: commits in the box as the person (author from the person's git identity), `paths` staged first, `all` as `git commit -a`. `201 {"sha"}`.
- `POST /workspaces/{id}/git/push` `{"repo", "remote": "origin", "branch"}`: pushes in the box with the box's GitHub token (direct mode; workspace.md, GitHub in the box). `200 {"pushed": true, "output": "..."}`; git's refusal is `409` with its output.

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

### Accounts (box only): built

A box holds Claude accounts besides its own profile: each is a directory, `/home/agent/.claude-accounts/<slot>`, on its own volume, which a terminal under the account has as `CLAUDE_CONFIG_DIR`. Its login (a full `claude auth login`, which claude.ai connectors need) lives only in that directory; orbit stores what a fixed script reads of it, never a token (docs/agents/security.md, Boxes and terminals). The shared profile in `~/.claude` (`settings.json`, `CLAUDE.md`, `skills`, `commands`, `agents`) is linked into every account's directory; the rest (credentials, `.claude.json` with its user MCP servers, sessions, history) is the account's own. A terminal without an account uses the box's own profile and the subscription token, as before.

```json
{"id": "uuid", "workspace_id": "uuid", "slot": "3c9ab5742fb5", "label": "personal",
 "dir": "/home/agent/.claude-accounts/3c9ab5742fb5", "logged_in": true,
 "email": "k@example.com", "account_uuid": "uuid", "org_id": "uuid", "org_name": "...",
 "subscription_type": "max", "rate_limit_tier": "default_claude_max_20x",
 "access_expires_at": "...", "refresh_expires_at": "...", "checked_at": "...",
 "last_used_at": "...",
 "usage": {"source": "statusline", "observed_at": "...",
           "windows": [{"window": "five_hour", "used_percentage": 12.5, "resets_at": "..."}]},
 "created_by": "kanishk", "created_at": "..."}
```

- `slot` names the account's volume (`orbit-box-<agent>-acct-<slot>`) and directory: 12 hex digits of its `id`.
- `logged_in`, `email`, `account_uuid`, `org_id`, `org_name`, `subscription_type`, `rate_limit_tier` and the expiries are what the account's files said when `checked_at` was: `logged_in` is whether its credentials hold a refresh token; `access_expires_at` is the access token's expiry (the CLI refreshes it itself), `refresh_expires_at` the refresh token's, past which the account needs a new login. A read that finds no login sets `logged_in` false and clears the expiries but keeps the identity fields, so a logged-out account still says whose it was. Each is `null` until read, or when the CLI did not write it.
- `last_used_at` is the newest attach of a terminal under the account, `null` before one.
- `usage` is the newest observation of the account's subscription windows, `null` before one. `source` is `statusline`: the box image's status line (`orbit-statusline`, which managed settings give every Claude Code in the box) records the `rate_limits` the CLI hands it into the account's `.orbit/usage.json` whenever an interactive Claude Code runs under the account, and orbit reads that file; or `poll`: orbit asked the undocumented `/api/oauth/usage` from inside the box with the account's access token, only when `ORBIT_ACCOUNT_USAGE_POLL` is set (off by default; docs/agents/deployment.md). `observed_at` is when the status line ran or the poll answered. `windows` are sorted by name: `window` is the CLI's or the endpoint's name (`five_hour`, `seven_day`, `seven_day_opus`, ...), `used_percentage` 0 to 100 (`null` when not reported), `resets_at` `null` when not reported. An older observation never replaces a newer one, whichever its source, and it is not refreshed by a stopped box: after a window's `resets_at` has passed, its `used_percentage` is out of date.
- An account (its login and its status line's usage record) is read from the box on every list and get while the box is running (a stopped box is not started for it), and when a terminal under it ends; otherwise the fields are as last read.

Routes require the workspace to be a box (`409`, `no_shell`, otherwise). Mutations and account login require membership of `ORBIT_WORKSPACE_GROUP` (`403`, `group`); account reads use the authenticated workspace access.

- `GET /workspaces/{id}/accounts`: `{"accounts": [<account>]}`, oldest first.
- `POST /workspaces/{id}/accounts` `{"label", "cols", "rows", "restart"?: false}`: records the account, mounts its volume, links the shared profile in, and opens a `login` terminal of that size under it (`claude auth login`: the person opens the URL it prints and pastes the code back). `201 {"account", "terminal"}`. `label` is 1 to 100 bytes, unique in the workspace (`409`, `exists`); at most 16 accounts a box (`409`, `limit`). A stopped box is started with the new mount. A running box gains a mount only by being recreated, which ends every session in it: with any terminal running that is `409` (`busy`, naming how many) unless `restart` is true, which records them `exited` with `end_reason` `box_restarted` and recreates the box (its volumes kept). A failure after the account is recorded is `502`; the account is kept, and the box mounts it when it next starts.
- `GET /workspaces/{id}/accounts/{aid}`: `{"account"}`.
- `PATCH /workspaces/{id}/accounts/{aid}` `{"label"}`: `200 {"account"}`; `exists` as above.
- `POST /workspaces/{id}/accounts/{aid}/login` `{"cols", "rows"}`: `201 {"terminal"}`, a new `login` terminal under the account, for a login past its `refresh_expires_at` or a switch to another login. `409` (`restart`) when the running box does not mount the account yet.

Accounts are never deleted. Attaching to a `login` terminal is as for any terminal; when it ends, the account is read again before the `exit` frame, so the client's next list shows the login.

### Review comments

*(planned)* Comments on lines, sent to the conversation's agent as one message, as a ticket's review comments are (tickets.md).

- `POST /conversations/{id}/comments` `{"path", "start_line", "end_line", "side": "new", "anchor": {"kind": "run", "run_id": "uuid"}, "body"}`: `201 {"comment"}`, a draft. `anchor.kind` is `run` (a reply's changes), `git` (`repo`, `base`, `head` as for a git diff) or `worktree` (the file as it is). `side` is `new` or `old`. Lines `0` and `0` mean the whole file; a body is at most 8 KB.
- `GET /conversations/{id}/comments?status=`: `{"comments": [...]}`. `status` is `draft`, `sent`, `superseded` or `withdrawn`.
- `POST /conversations/{id}/comments/{c}/edit` `{"body", "start_line"?, "end_line"?}` writes a new version that supersedes it; `POST .../withdraw` withdraws a draft.
- `POST /conversations/{id}/review` `{"note"?, "comments"?: ["ids"], "wake"?: true, "client_request_id"?}`: sends the drafts (all of the conversation's, or the ones named) as one message, formatted by code: each comment's file, lines and anchor, the code it is on quoted, then its body quoted line by line. `201 {"message_id", "run_id"}` as `POST /conversations/{id}/messages`.

### Audit

`GET /workspaces/{id}/events?before=&limit=`: `{"events": [{"id", "at", "person", "op", "path", "from", "version_before", "version_after", "bytes", "terminal_id", "run_id", "request_id"}], "next_before"}`, newest first. `id` is a number; `limit` defaults to 100, at most 500; `next_before` is opaque, `null` on the last page. Fields an operation has no value for are `null` (`person` for the operator token, `version_before` for a create, `version_after` for a trash, `bytes` for a mkdir). Built today: `write`, `mkdir`, `rename`, `trash` (also before an upload's overwrite), `restore` and `upload`, one row per file. Reads are not recorded. `op` is each write route's name (`write`, `mkdir`, `rename`, `trash`, `restore`, `upload`, `commit`, `push`, `terminal.create`, `terminal.attach`, `terminal.detach`, `terminal.kill`, `review`). Terminal input is never recorded.

## Memory

`GET /agents/{name}/memory?limit=`: the agent's memory versions, newest
first, `{"versions": [{"id": 12, "run_id": "uuid", "content": "<markdown>",
"created_at": "...", "author": null, "restored_from": null}]}`. `limit` defaults to 20; the maximum is 200. `author` is the person who wrote a version through the API (`null` for a run's, whose `run_id` is set); `restored_from` is the version a restore copied.

`POST /agents/{name}/memory` writes the next version: `{"content": "<markdown>"}`, or `{"restore": <version id>}` to copy an earlier version of this agent's memory. Exactly one of the two (400 otherwise); `content` at most the deployment's `memory_max_bytes` (400); an unknown version or one of another agent is 404. Returns `201 {"version": {...}}`, shaped as above. The author is the token's person (with the operator token, the body's `author`). Nothing is replaced: the new version is the newest, every earlier one stays.

## Memory entries

Beside each agent's memory document, orbit keeps **memory entries**: one durable fact each (a correction the person gave, a preference, a fact about their world, where something is), typed, versioned, and shared at one of three scopes:

- `profile`: the person; every agent reads it.
- `topic`: a domain such as `finance` or `homelab`; every agent that reads the topic (its spec's `topics`, or attached below). An agent whose `topics` is `["*"]` reads every topic; the built-in `orbit` agent does.
- `agent`: one agent's own.

Every run of an agent is shown an index of the active entries it reads (`GET /agents/{name}/context`, below) and the entries in full as an input file (docs/ARCHITECTURE.md, Memory entries).

An entry:

```json
{"id": "uuid", "scope": "topic", "topic": "finance", "key": "transfers-are-not-spending",
 "version": 2, "status": "active",
 "type": "feedback", "title": "Transfers are not spending",
 "description": "one line for the index", "body": "<markdown>",
 "why": "...", "how_to_apply": "...", "pinned": false,
 "superseded_by": null, "source": "person", "source_run_id": null,
 "source_conversation_id": null, "author": "kanishk", "note": "",
 "created_at": "...", "updated_at": "..."}
```

- `scope` with `topic` (scope `topic`) or `agent` (scope `agent`); neither for `profile`. Topic names are spec names (`^[a-z][a-z0-9-]{0,62}$`).
- `key` is a slug (`^[a-z0-9][a-z0-9-]{0,79}$`) unique within its scope, stable across versions; omitted on create, it is made from the title (`-2`, `-3`, ... when taken). A key once used stays the entry's, retired or not.
- `type` is one of Claude Code's memory types: `user` (who the person is), `feedback` (how they want work done), `project` (facts about their world and work), `reference` (where to find things).
- `pinned` (feedback only, else 400) repeats the entry to every turn of its readers' runs, so it holds however long a session runs (docs/ARCHITECTURE.md, Memory entries). An edit that makes a pinned entry another type unpins it.
- `title` (one line, at most 200 bytes) and `body` (at most 16 KB) are required; `description` is one line of at most 400 bytes; `why` and `how_to_apply` at most 4 KB each.
- `version` counts versions, the newest in force. `updated_at` is when it was written, `created_at` when the entry was. `status` is `active` or `retired`; `superseded_by` is the entry that replaced a retired one.
- `source` is who wrote this version: `person` (this API), `learned` (a learning run), `import` (`POST /memory/import`). `source_run_id` and `source_conversation_id` name what it was learned from. `author` is the token's person (with the operator token, the body's `author`, if any). `note` says why this version was written.

Nothing is deleted. Every change is a new version, kept:

- `GET /memory/entries?scope=&topic=&agent=&type=&status=&q=&run_id=&limit=`: `{"entries": [...]}`, current versions, by scope (profile, topics, agents), then name, then most recently changed. `topic` or `agent` (not both) selects one place. `status` is `active` (default), `retired` or `all`; `q` matches key, title, description, body, why and how_to_apply, case-insensitively. `run_id` selects entries that run wrote a version of. `limit` defaults to 200, at most 1000.
- `POST /memory/entries` `{"scope", "topic"|"agent", "key"?, "type", "title", "description"?, "body", "why"?, "how_to_apply"?, "pinned"?, "note"?}`: `201 {"entry": ...}`. An explicit `key` already used in the scope is 409; an `agent` that is not configured is 400.
- `GET /memory/entries/{id}`: `{"entry": <current>, "versions": [...]}`, every version newest first.
- `PATCH /memory/entries/{id}` `{"version"?, "type"?, "title"?, "description"?, "body"?, "why"?, "how_to_apply"?, "pinned"?, "status"?, "note"?}`: the next version, with absent fields kept. `version` is the version the client read; another current one is 409, so two edits never silently overwrite each other (omit it to write regardless). `status: "active"` restores a retired entry (clearing `superseded_by`). An edit that changes nothing writes no version and returns the current one.
- `POST /memory/entries/{id}/restore` `{"restore": <version>, "version"?, "note"?}`: the next version, a copy of an earlier one's content and status (and its `superseded_by` while that entry is still active). `restore` must be an earlier version (400); `version` is as for `PATCH`. `note` defaults to `restored version <n>`.
- `POST /memory/entries/{id}/retire` `{"version"?, "superseded_by"?, "note"?}`: the next version, retired. `superseded_by` must be another, active entry (400 otherwise). This is how an entry is deleted.
- `POST /memory/import` `{"dry_run": bool, "entries": [<entry as for create, key optional>], "note"?}`: matches each by scope and key. A new key creates an entry, a known one gets a new version when its content differs or it was retired, an identical one is left alone, so an import can be repeated. An import never changes a known entry's `pinned`. `{"dry_run", "results": [{"scope", "topic"|"agent", "key", "outcome": "created"|"updated"|"unchanged", "id", "version"}]}`, in request order. All of it is one transaction: one invalid entry fails the whole import (400, naming its index). `dry_run` reports and writes nothing (`id` is absent for an entry it would create). At most 500 entries. `orbit import claude-memory` calls this (deployment.md).
- `GET /memory/topics`: `{"topics": [{"name", "entries", "agents"}]}` by name: every topic that holds entries or is read by an agent. `entries` counts its active entries; `agents` are the agents that read it, from their spec (an agent reading every topic is on each) or attached.

### Entries agents write

An agent writes entries itself, without approval: every run's output carries `entries` (docs/ARCHITECTURE.md, Memory entries), applied with the run's outcome as versions with `source: "learned"` (they never pin or unpin), `source_run_id` the run and `source_conversation_id` its conversation. A person reverts one with `restore` or `retire` above. A write the store refuses (a stale version, a key taken, an invalid field, a ref the run was not shown) is skipped and the rest apply. Each run's writes and what became of them are its `entries.json` artifact (`GET /runs/{id}/artifacts`): `[{"op", "ref", "title", "result": {"outcome": "written"|"unchanged"|"refused", "error", "id", "key", "version"}}]`, in output order.

### Remembering from a conversation

A message from the person that starts with `/remember` writes an entry at once, in the run that reads it, and that run's reply says what was saved: `/remember [--agent | --topic <name>] [--feedback | --user | --project | --reference] <text>`.

- **Place:** the profile; `--agent` is the conversation's agent's own; `--topic <name>` (or `--topic=<name>`) a topic the agent reads.
- **Type:** the flag's, else inferred: a link or "can be found" is `reference`, "I am", "I live", "my job" `user`, "we", "our", a deadline `project`, anything else `feedback`.
- **Entry:** title the text's first sentence (at most 80 bytes), body the whole text, `source: "person"`, `author` the message's, `source_run_id` and `source_conversation_id` the run and its conversation, `note` `/remember in a conversation`.
- **Reply:** `Saved to memory: [<type>] <title> (<ref>), for every agent.` (or `for <agent> only.`, `for the agents that read <topic>.`); `Already in memory: ...` when the same text is in that place (nothing written); `Not saved: <why>.` for an unknown flag, no text, or a topic the agent does not read. A run woken only by `/remember` messages succeeds with that reply as its report and no backend call; with other messages beside them, the agent answers those and the confirmation follows its reply.
- Messages an agent wrote (a task's) are never commands. The learning pass leaves `/remember` messages out.

### Learning runs

After a conversation goes quiet, orbit runs a short learning pass over it (trigger `learn`) that reads what was said since the last pass and writes entries the same way. It runs in the agent's **learning conversation**: one per agent, `purpose: "learning"`, titled `Learning`, archived, so it is listed only with `archived=true`; its thread is every learning run, each report listing the writes it made. Conversations carry `purpose` (`null` for a person's). Nothing a learning run does appears in the conversation it read.

`POST /conversations/{id}/learn` queues a learning run of that conversation now, whatever the quiet, through its newest message from the person: `202 {"run_id": "uuid"}`. 409 when the conversation has no message from the person or a learning run for that point exists already (learned, or tried); 400 for the learning conversation itself.

### What an agent reads

- `GET /agents/{name}/context`: `{"agent", "topics": [{"name", "from": ["spec"|"api"]}], "entries": <count>, "index": "<markdown>"}`. For an agent reading every topic, `topics` is each topic there is, `from` `spec`. `index` is exactly what a run's prompt carries (with `inputs/_memory.md` for the run's own inputs path), `""` when the agent reads no entries; when it cannot list every entry within its 8 KB it keeps feedback first, then the most recently changed, and says how many it left out and how to find them.
- `PUT /agents/{name}/topics/{topic}`: attach a topic, `{"attached": true, "created": bool}` (`created` false when it was attached already). `DELETE /agents/{name}/topics/{topic}`: detach, `{"detached": true}`, 404 when not attached through the API (a topic the spec names stays until the spec changes). The agent must be configured (404).

## MCP

`/mcp` is an MCP server (Streamable HTTP, stateless) on both listeners, so the Claude apps and Claude Code can talk to orbit's agents. It takes what `/api/v1` takes on the same listener: an authentik access token (verified as above; the person is the author and decider), and on the internal listener the operator token. Without a valid token it answers `401` with `WWW-Authenticate: Bearer resource_metadata="<public_url>/.well-known/oauth-protected-resource/mcp"`. Every request stands alone (no `Mcp-Session-Id`), so a call may land on either instance of a rollout.

| tool | arguments | returns |
|---|---|---|
| `list_agents` | none | each configured agent: `name`, `description`, `enabled`, `needs_input`, `open_questions`, `pending_actions`, `last_run`, `agent_status` |
| `list_conversations` | `agent`, `archived`, `limit`, `before` | as `GET /agents/{name}/conversations`: `conversations`, `has_more`, `next_before` |
| `create_conversation` | `agent`, `title`, `message`, `wait_seconds` | `conversation_id`, `message_id`, `run_id`, and `reply` as `send_message` |
| `send_message` | `body`, and `agent` (its default conversation) or `conversation_id`; `reply_to`, `answers` (a question's form, as for messages), `wait_seconds` | `message_id`, `run_id`, `reply`: the run as a thread run item (`report`, `questions`, `actions`, `status`) |
| `wait_for_reply` | `run_id`, `wait_seconds` | `reply`, the run as a thread run item |
| `read_thread` | `agent` or `conversation_id`, `limit` (default 20, at most 200), `before`, `unfold` | as a thread page: `items`, `has_more`, `next_before`, `meter`; identical skips folded unless `unfold` (A conversation's thread, Folded skips) |
| `list_actions` | `agent`, `status` (default `pending`; `all` for every status), `run_id`, `limit`, `before` | as `GET /actions`: `actions`, `has_more`, `next_before` |
| `approve_action`, `reject_action` | `action_id`; `approve_action` also `always` | the decided `action`; a decided one is an error. With `always: true`, as `POST /actions/{id}/approve` with `always` (Actions), plus `always` |
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

### MCP servers and login

- `GET /workspaces/{id}/accounts/{aid}/mcp` runs `claude mcp list` as the selected account and returns `{"servers": [...]}`. Each server reports `name`, `target`, `transport`, `status`, optional `detail`, whether it is a `claude.ai` connector, and whether Orbit manages it. The command runs inside the box with that account's config directory; credential contents are not returned.
- To authenticate an MCP server, create an account-scoped terminal with `preset: "mcp-login"` and `mcp_server` set to the listed server name. The fixed command is `claude mcp login --no-browser <server>`; the server name is validated and cannot supply command options.
- `box.mcp_servers` config is synchronized by Orbit into each mounted account with `claude mcp add-json -s user`. Orbit records its own server names and removes only those when they leave configuration.

## Skills

orbit keeps a library of Claude Code skills: each a `SKILL.md` (frontmatter with `name` and `description`, then the instructions) and optional files beside it (references, scripts, assets), every version kept. An agent runs with the skills its spec names (`skills`) and those attached here; a run gets each at its current version (docs/ARCHITECTURE.md, Skills). Agents also write and retire their own skills, with the `skill:write` action under its rule (below).

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

**Skills agents write.** A skill steers every later run it is given to, so an agent writes one with the `skill:write` action, under its rule like any action (`auto`, `approve` or `deny`; unnamed, denied and not offered; the built-in `orbit` has it under `approve`). Its params are `{"op": "write"|"retire", "name", "content", "note"}`: `write` gives the whole `SKILL.md` of a new skill, or of one the run has, whose files stay; `retire` ends one the run has. At most 3 per run. Code resolves each when proposed and adds to its params `version` (the version the run had, which the write applies against; 0 for a new skill) and `changes`: `{"from_version", "lines_added", "lines_removed", "diff", "files", "scripts"}`, where `diff` is `SKILL.md`'s change, one `+` or `-` line each (at most 16 KB, then how many lines it left out), `files` the skill's other files, and `scripts` those of them that run (under `scripts/` or `bin/`, a script's extension, a `#!` line, or an executable's header). A retire of a skill the run lacks, a `SKILL.md` without a description, or a fourth write is `denied` with the reason. The approval notification leads with the change in words, `Writes skill deploy (version 1 to 2): SKILL.md +1 -1 lines; its 3 other files stay.`, then `Scripts and executables in this skill (...): scripts/go.sh, tools/check`, then the diff. Executed, the write is a `source: "agent"` version against `version` (a newer one fails the action, naming it), a written skill is attached to the agent, and the action's `result` is `{"skill", "op", "outcome": "written"|"unchanged", "version", "changes"}`. The versions the run was given are its `skills.json` artifact. A person reverts with `restore` or `retire`.

## Templates

orbit keeps a library of **agent templates**: an agent spec with `{{ name }}` slots and the typed parameters that fill them, every version kept (`internal/templates`, `internal/store/templates.go`). Creating an agent from one is code: the template's version is read, the values checked against its parameters, the slots filled, and the agent it gives dry-run through the same validation as `POST /agents` before anything is stored.

A template:

```json
{"name": "news", "version": 2, "status": "active",
 "description": "Morning headlines on topics you pick.",
 "sha256": "<hex>", "source": "person", "author": "kanishk", "note": "...",
 "restored_from": null, "created_at": "...", "updated_at": "...",
 "parameters": [
   {"name": "agent", "label": "Agent name", "type": "string", "mode": "required"},
   {"name": "topics", "label": "Topics", "type": "string", "mode": "user_prompt", "description": "What to follow"},
   {"name": "hour", "type": "number", "mode": "optional", "default": 8}],
 "spec": {"name": "{{agent}}", "charter": "Summarize the news about {{topics}}.",
          "triggers": [{"schedule": "0 {{hour}} * * *"}]}}
```

- `name` matches `^[a-z][a-z0-9-]{0,62}$`; `description` is required, one line of at most 400 bytes. The document (name, description, parameters, spec) is at most 64 KB. `version`, `status`, `sha256` (over the canonical document), `source` (`person`, or `import`), `author`, `note` and `restored_from` are as for skills.
- `parameters` are form fields (as a question's, A conversation's thread: `name`, `label`, `type`, `options`, `default`) with `mode` in place of `required` and an optional `description` (at most 400 bytes): `required` (the creator gives a value), `optional` (its `default`, or empty, when none is given) or `user_prompt` (the person gives it at creation: through this API it is required like `required`, and a client shows the parameters as the form; in a conversation, orbit asks the person for it with a form, below). At most 12.
- `spec` is the agent, exactly an agent file's fields as JSON (docs/agents/configuration.md), any string of which may hold slots, `{{name}}` or `{{ name }}`, each naming a parameter (another is a `400`). A string that is exactly one slot becomes the value itself, so a number or boolean parameter fills a number or boolean field; a slot inside a longer string becomes the value's text (a boolean as `true` or `false`). A `file` parameter's value is an upload id, and its slot becomes the file's text (UTF-8, at most 64 KB). An optional parameter without a value or default fills its slot with nothing. Values are never scanned for slots. A template is checked when written: its parameters, every slot, and the spec decoding as an agent with sample values; whether the rendered agent loads (its schedule, its sources, a restricted agent's limits) depends on the values, and is checked when it is rendered.
- `spec` is in the responses of `GET /templates/{name}` and `GET /templates/{name}/versions/{v}` only; lists carry `parameters`.

Routes:

- `GET /templates?status=`: `{"templates": [...]}` by name, current versions; `status` as for skills.
- `POST /templates` `{"template": {"name", "description", "parameters", "spec"}, "note"?}`: `201 {"template"}`; 409 when the name exists, 400 for an invalid template (unknown fields included).
- `GET /templates/{name}`: `{"template": <current, in full>, "versions": [<every version without spec>, newest first]}`.
- `GET /templates/{name}/versions/{version}`: `{"template": <that version, in full>}`.
- `PUT /templates/{name}` `{"template", "version"?, "note"?}`: the next version (`template.name` may be omitted; if present it must be `{name}`). `version` is the version the client read; another is 409. A document identical to the current active version writes nothing: `{"template", "changed": false}`.
- `POST /templates/{name}/restore` `{"restore": <version>, "version"?, "note"?}` and `POST /templates/{name}/retire` `{"version"?, "note"?}`: the next version, an active copy of an earlier one or retired. Nothing is created from a retired template.
- `POST /templates/{name}/agents` `{"parameters": {"<name>": <value>}, "version"?, "name"?, "conversation_id"?, "dry_run"?}` creates an agent from the template's current version (or `version`): each value is the JSON type its parameter takes, checked as a form's answers are; `name` replaces the name the spec renders; `conversation_id` is whose uploads a `file` value may name (without it, a file value is refused). It always dry-runs first. A `required` or `user_prompt` parameter without a value or default is `400 {"error", "needs": [<field>]}`, the fields to ask for; an invalid value, a rendered agent that does not load, or a retired template is a `400` naming it; a name already defined is `409`. `dry_run: true` stores nothing: `200 {"valid": true, "spec": <the agent as loaded>, "next_fires", "widens", "template": {"name", "version"}}`, where `widens` is what the agent holds beyond nothing (as an `agent:define` preview names it). Otherwise the agent is defined as `POST /agents` defines one (its `author` the token's person): `201` with the same fields plus `definition` and `agent`, as `POST /agents` returns them.

**In a conversation.** An agent whose rules enable `template:create` (the built-in `orbit` has it under `approve`) proposes `{"template", "version"?, "name"?, "parameters"}`. It is never executed as itself. When proposed, code renders it (the chat `act` tool answers at once with what will happen): a `user_prompt` value is the person's, taken from their `answers` to orbit's question for that template in this conversation and never from the proposal; when there are none, orbit asks it, a question with those parameters as its form (text `To create an agent from the template <name>, I need these from you.`), and the proposal is `denied` saying so. The person's answer reaches the agent's next run, which proposes again. A `required` value the proposal lacks, a parameter the template does not have, or a rendered agent that does not load denies it with the reason. A proposal that renders is recorded as an `agent:define` of the new agent, `pending` whatever the rules, its params the rendered `spec` and `template` (`{"name", "version"}`, the version it was rendered from); a person approves it like any action.

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
    "author": null, "tool": null, "external_session_id": null, "repo": null, "path": null,
    "skill": null, "skill_version": null, "at": "...", "rank": 0.083,
    "snippet": "The electricity bill rose because of a rate change ...",
    "snippet_parts": [{"text": "The ", "match": false}, {"text": "electricity", "match": true},
                      {"text": " ", "match": false}, {"text": "bill", "match": true},
                      {"text": " rose because of a rate change ...", "match": false}],
    "links": {"run": "/api/v1/runs/uuid", "run_page": "https://orbit.kanishksachdev.com/ui/runs/uuid",
              "conversation": "/api/v1/conversations/uuid/thread"}}], "repaired": null}
```

- **`kind`** is what matched: `message`, `question`, `report`, `error`, `narration`, `tool_call`, `memory`, `memory_entry`, `skill`, or `session`. A tool call includes its `repo` and `path` when present in its input. A skill has `skill`, `skill_version` and its name as `title`; only its current active version is returned. A session is one prose line from an imported session, with `external_session_id` and line number in `seq`. A line repeated by a resumed session is returned once, under the session it was first imported with. Of an agent's memory versions only the newest matching one is returned; a memory entry only as its current version, and never once retired. The `kind` parameter is a comma list of these.
- **`q`** is web-search syntax: words (all must match, stemmed: `bills` finds `bill`), `"a quoted phrase"`, `or` between alternatives, `-word` to exclude, plus the operators below. Required, with at least one search word beyond operators (`400` without). A query of only stop words matches nothing.
- **Identifiers** match by their pieces: punctuation and camelCase boundaries split a word (`getHTTPResponse` becomes get, HTTP, Response), so `parseConfig`, `parse_config`, `parse-config` and `"parse config"` match each other, `search.go` finds `internal/store/search.go`, and `config` finds `loadConfig`. Pieces remain unstemmed. A match found only through code pieces may have no highlighted snippet match.
- **Operators** are whole unquoted `key:value` tokens; values may be quoted. `agent:<name>` applies the agent filter (a different `agent` parameter is `400`); `kind:<kinds>` intersects with the `kind` parameter (no overlap is `400`); `repo:<owner/name or name>` keeps matching tool-call repositories, case-insensitive; `path:<text>` keeps tool-call paths containing the text, case-insensitive. Operators can be combined with the corresponding parameters. Unknown `key:value` tokens are treated as text.
- **Typo repair:** if the first page has no results, each wanted word piece of 3 to 32 ASCII letters absent from all documents is compared with `search_terms` using Postgres `pg_trgm`. If the corrected query finds results, those are returned and `repaired` contains the query searched and each changed word; otherwise it is `null`. Excluded words are never corrected.
- **Ranking** is the better of the English and code `ts_rank_cd` scores normalised by document length (`rank`, comparable only within one response), ties newest first. A memory entry's title weighs more than its body, and a tool's name more than its input.
- **`snippet`** is up to two fragments of the matching text joined by ` ... `; `snippet_parts` is the same text split into runs, `match` true on matched words, so a client highlights without parsing markup (the text is never escaped or marked up).
- **Filters:** `agent` (that agent's documents; a profile or topic entry belongs to none), `conversation_id`, `run_id` (a UUID, else `400`); `from` and `to` are a calendar day in the deployment's timezone (`to` includes its day) or an RFC 3339 time (`to` excluded), by the document's `at` (a message's or memory version's creation, a run's finish, a transcript line's time). `limit` defaults to 20, at most 100; `offset` pages, at most 1000. `total` counts every match within the filters, whatever the page.
- **`links`** are where the result is read in full: the run (`run`, and `run_page` when `public_url` is set) and its conversation's thread, or for a session line the session (`session`). A memory version names its run's conversation when it has one.
- Documents are derived by triggers as rows are written, and existing rows were indexed by the migration that added search, so a result is there as soon as its row is.

## Imported sessions

Sessions run outside orbit, a person's own Claude Code sessions on their laptop, are imported so search covers them (above) and orbit can learn from them. `orbit import claude-sessions` sends them (deployment.md). A session:

```json
{"id": "uuid", "source": "claude-code", "session_id": "<Claude Code's session id>",
 "cwd": "/Users/p/work/widget", "branch": "main", "repo": "someone/widget",
 "title": null, "first_prompt": "Why is the widget flaky?",
 "generation": 1, "lines": 120, "offset": 482113, "prefix_sha256": "<hex>", "content_sha256": "<hex>",
 "first_at": "...", "last_at": "...", "created_at": "...", "updated_at": "..."}
```

- `source` is `claude-code`; `session_id` is the source's id, unique per source. `cwd` and `branch` are the newest a line named; `repo` is the repository the client found `cwd` to be a checkout of (its origin remote, `owner/name` on GitHub). `title` is a title the session was given (a custom title, the CLI's summary line), `null` when none; `first_prompt` is the person's first typed message (not a tool result, a meta line or a command the CLI wrapped in tags).
- `lines` counts the current generation's lines; `offset` is how many bytes of the file they came from and `prefix_sha256` the SHA-256 of those bytes, which is how an import knows what to send next. `content_sha256` chains the line digests in order (`sha256(previous || digest)` in hex, from the SHA-256 of nothing): two sessions with the same lines have the same hash. `first_at` and `last_at` are the earliest and newest line timestamps (`last_at` is the import time for a session whose lines have none).
- A line is stored in a canonical form: a JSON object with its keys sorted and without `sessionId`, credentials replaced by `[REDACTED]` (the values of Claude Code's credential variables and of any secret-named assignment, Authorization and bearer credentials, passwords in URLs, provider-format tokens, private key blocks), and a line over 1 MiB cut to its skeleton: its placing fields (`type`, `uuid`, `parentUuid`, `timestamp`, `cwd`, ...), `orbitOmittedBytes`, and its message with each content block replaced by a note, a tool call keeping its id and name and a tool result its call's id. Each line's content is stored once, by the SHA-256 of that form, so a session resumed or forked from another (which starts with its parent's lines under a new id) adds only its own.
- Nothing is deleted. A file whose start no longer matches what orbit holds is imported again as the session's next `generation`; the earlier generation's lines stay stored.

Routes:

- `POST /sessions/import` `{"dry_run"?, "source"?, "sessions": [{"session_id", "repo"?, "from_offset", "from_prefix_sha256", "rewrite"?, "to_offset", "to_prefix_sha256", "lines": [<line object>, ...]}]}`: each item is a stretch of one session's file, its complete lines from `from_offset` (where orbit's copy ends, `from_prefix_sha256` the hash of the bytes before it; `0` and `""` for a session orbit does not have) to `to_offset` (`to_prefix_sha256` the hash of the bytes before it). `rewrite: true` (with `from_offset` 0) replaces a session whose prefix changed with a new generation. Items apply in order in one transaction, so several stretches of one session may follow each other. `{"dry_run", "results": [{"session_id", "id", "outcome", "added", "new", "lines", "offset", "generation", "error"?}]}` in request order: `outcome` is `created`, `appended`, `rewritten`, `unchanged` (no lines, nothing moved) or `conflict` (the item does not continue orbit's copy: `error` says where orbit is, `lines`, `offset` and `generation` are orbit's, and nothing of it is written). `added` counts the lines added, `new` those whose content was not stored before. `dry_run` reports and writes nothing (`id` absent for a session it would create). `source` defaults to `claude-code`. At most 200 items and 20,000 lines; a body over 24 MiB is 400, as is a line that is not a JSON object (naming its item and line) or a `session_id` that is not a UUID.
- `GET /sessions?source=&repo=&before=&limit=`: `{"sessions": [...], "has_more", "next_before"}`, newest `last_at` first, paged as conversations are (`limit` defaults to 100, at most 1000).
- `GET /sessions/{id}`: `{"session": {...}}`; 404 for an unknown id.
- `GET /sessions/{id}/lines?after=&limit=`: `{"lines": [{"n", "digest", "line": <the stored line>}]}`, the current generation's lines after line `after` (default 0; lines count from 1), at most `limit` (default 200, at most 1000).

## Webhooks

`POST /hooks/{agent}/{trigger}` (both listeners, no token: not under `/api/v1`) is a delivery to one of an agent's webhook triggers (docs/agents/configuration.md, Webhooks). The body is anything up to the trigger's `max_bytes` (default 1 MiB), signed under the trigger's secret by its scheme:

| scheme | signature | delivery id | timestamp |
|---|---|---|---|
| `github` | `X-Hub-Signature-256: sha256=<hex HMAC-SHA256 of the body>` | `X-GitHub-Delivery` | none; `X-GitHub-Event` is recorded as the event |
| `standard` | `webhook-signature: v1,<base64 HMAC-SHA256 of <id>.<timestamp>.<body>>` (several, space-separated, any may match) | `webhook-id` | `webhook-timestamp` (Unix seconds), within 5 minutes of orbit's clock |

| status | body | when |
|---|---|---|
| `202` | `{"run_id": "uuid", "duplicate": false}` | a new delivery: a `webhook` run is queued in the agent's default conversation |
| `200` | `{"run_id": "uuid", "duplicate": true}` | a delivery id already recorded for this agent and trigger: the first run, nothing new |
| `200` | `{"ping": true}` | a `github` `ping` event: the secret is right, no run |
| `400` | error | a header the scheme requires is missing or does not parse (a delivery id is at most 200 bytes) |
| `401` | error | the signature does not match, or a `standard` timestamp is outside the tolerance |
| `404` | error | no such agent, or it has no webhook trigger of that name |
| `409` | error | the agent is disabled |
| `413` | error | the body is larger than `max_bytes` |
| `503` | error | the trigger's secret is not set in orbit's environment |

The run's `prompt` is the trigger's prompt followed by `Webhook <trigger>: delivery <id>, event <event>.`; its `webhook` sources are the body, as received (`application/json` when it parses as JSON, else `text/plain`). Deliveries are counted in `orbit_webhook_deliveries_total{agent, trigger, outcome}`.

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

## Prometheus

An agent with a `prometheus` source (docs/agents/configuration.md, Prometheus queries) also has `prometheus_query` (`mcp__orbit__prometheus_query` in a Claude Code run, an SDK tool of that name in a chat), run by orbit against that source's URL with its credentials; the model writes only the PromQL and the times. An unknown argument, a source that is not one of the agent's prometheus sources, or a query outside the bounds is a tool error the model reads, and nothing is sent.

| argument | meaning |
|---|---|
| `query` | PromQL (required, at most 4000 bytes), sent as written: never rendered, so a `${VAR}` or `{{today}}` in it reaches Prometheus as text |
| `source` | the prometheus source's name; may be left out when the agent has one |
| `time` | an instant query's time: RFC 3339, `now`, or `now-<duration>` (`s`, `m`, `h`, `d`); default now |
| `start`, `end`, `step` | a range query (`start` makes it one; `time` is then refused): `end` defaults to now, `step` (a duration) to the range over 250 points; at most 31 days and 1000 points per series |

The answer is Prometheus's `data`, `{"resultType", "result"}`, with `warnings` when Prometheus sent any. Every query asks Prometheus for at most 500 series (`limit`, which it reports as a warning when it cuts) and a 20 s evaluation timeout; an answer over 256 KiB is an error asking for a narrower query, never a cut document. In a report run its answers are in the figures ledger, so a number read from Prometheus backs a figure as a `query` result does.

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
   "by_day": [{"date": "2026-10-03", "api_equivalent_usd": 0.000000, "runs": 0}]},
 "tokens": {
   "total": {"input": 1200, "output": 52000, "cache_read": 4100000, "cache_creation": 310000,
             "cache_hit_ratio": 0.929515, "attempts": 40, "runs": 39},
   "by_agent": [{"agent": "finance", "input": 800, "...": 0, "attempts": 29, "runs": 29}],
   "by_model": [{"model": "claude-opus-5-5", "input": 1000, "...": 0, "attempts": 38, "runs": 37}],
   "by_day": [{"date": "2026-10-03", "input": 0, "output": 0, "cache_read": 0, "cache_creation": 0,
               "cache_hit_ratio": null, "attempts": 0, "runs": 0}]},
 "skills": [{"skill": "tidy-ledger", "runs": 4, "last_used_at": "...", "versions": [2, 3]}]}
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
- **Tokens** are run attempts' token counts (Runs, above) over the same range, by their run's `created_at`. Each entry has `input`, `output`, `cache_read`, `cache_creation`, `cache_hit_ratio` (`cache_read / (cache_read + input + cache_creation)` over the entry, six places, `null` with no prompt tokens), `attempts` (attempts that reported tokens) and `runs` (their runs). `by_agent` lists agents with a reporting attempt in the range, by name; `by_model` models, by name, from each attempt's per-model split (all of an attempt under its `model` when the backend did not split; `""` when it named none); `by_day` every day of the range, oldest first, zero or not.
- **Skills** are the skills runs created in the range read through orbit's skill tool (`mcp__orbit__skill`, from the journal's tool calls, each call counted whether or not it succeeded), by name: `runs` that called it for the skill, `last_used_at` the newest call, and `versions` the versions those runs were given (their `skills.json`), ascending. A run that only loaded a skill's stub, or read it without orbit's tools (the whole `SKILL.md` in the plugin), is not counted. Always an array.

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
              "heartbeat_age_seconds": 12.4,
              "agent_status": {"state": "working", "since": "...", "source": "hooks", "run_id": "uuid"}}],
 "pending_actions": 2,
 "holds": [{"id": "uuid", "runner": "claude", "kind": "auth",
            "reason": "claude-code success/: Not logged in · Please run /login",
            "run_id": "uuid", "opened_at": "...", "resets_at": null,
            "next_probe_at": "...", "probes": 2, "last_probe_at": "...",
            "last_probe_error": "..."}],
 "agents": [{"name": "finance", "enabled": true, "priority": "normal", "timezone": "America/New_York",
             "last_success": {"run_id": "uuid", "finished_at": "..."},
             "next_fires": [{"trigger_index": 0, "schedule": "0 8 * * *",
                             "at": "2026-11-02T13:00:00Z"}],
             "held": false}]}
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
- `holds` lists the runners held now (docs/ARCHITECTURE.md, Reliability), by runner; `[]` when none. `kind` is `auth` (the backend refused orbit's credential) or `rate_limit` (a subscription window was rejected; `resets_at` is its reset when reported). While a runner is held, the unattended runs of every agent on it (schedule, webhook, continue, task, delegation, learn, build) wait queued; a person's message or manual run is not held. `next_probe_at` is when orbit tries the runner again (every 15 minutes, or at `resets_at`), `probes` how often it has, `last_probe_error` the newest probe's failure. A hold leaves the list when a probe or a person's run on the runner succeeds.
- `agents` is every agent in the configuration in force, by name, with its `priority` (`normal` or `low`). `held` is true while its runner is held.
  `last_success` is its newest succeeded or skipped run (a gate finding
  nothing to judge is not a stale agent; a fire skipped as `missed` did nothing, so it is not counted), `null` if none.
  `next_fires` is each schedule trigger's next fire after now, read in the
  agent's timezone, soonest first, in UTC; `[]` for a disabled agent or one
  without triggers. A fire is enqueued by the scheduler's leader, so it is
  when the run is due, not when it starts. A cron trigger carries `schedule`; an RRULE trigger carries `rrule` and `start` instead, and one whose series has ended is not listed.
