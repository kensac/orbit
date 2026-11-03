Warning: truncated output (original token count: 48285)
Total output lines: 1530

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

### Approvals

In a conversation at `ask`, `edits` or `auto`, what the level leaves as "ask" is put to its person: Claude Code sends orbit a permission request (`can_use_tool`) and the turn waits. orbit stores it as an approval, shows it in the run's journal (`approval` items, above), lists it in the inbox and pushes it to the person's devices (a `permission` attention row, written with the approval; Attention and push), and answers the CLI with the person's decision. A plan the session presents (`ExitPlanMode`) is a plan approval; once it is allowed the session is back at its level. A request in a run nobody attends (any trigger but `message`) is denied at once, as is one at `locked`.

`GET /conversations/{id}/approvals?state=&limit=` is `{"approvals": [<approval>]}`, newest first; `state` is one of the states below (400 otherwise), `limit` defaults to 50, at most 200. `GET /approvals/{id}` is `{"approval": <approval>}`:

```json
{"id": "uuid", "run_id": "uuid", "attempt": 1, "conversation_id": "uuid", "agent": "orbit",
 "request_id": "...", "kind": "tool", "tool": "Bash", "input": {"command": "npm test"}, "level": "ask",
 "decision_reason": "...", "decision_reason_type": "rule", "blocked_path": "", "matched_rule": null,
 "suggestions": [{"type": "addRules", "rules": [{"toolName": "Bash", "ruleContent": "npm test:*"}], "behavior": "allow", "destination": "localSettings"}],
 "title": "", "description": "", "conversation_grant": true,
 "state": "pending", "scope": null, "message": "", "decided_by": null, "decided_at": null,
 "expires_at": "...", "created_at": "..."}
```

- `kind` is `tool`, or `plan` for `ExitPlanMode` (the plan is `input.plan`). `level` is the run's level when the CLI asked.
- `decision_reason`, `decision_reason_type` (`rule`, `mode`, `workingDir`, `safetyCheck`, `classifier`, ...), `blocked_path`, `matched_rule` (the `permissions.ask` rule that forced the prompt: `{"source", "tool_name", "rule_content"}`), `suggestions` (the CLI's `permission_suggestions`), `title` and `description` are the CLI's own, as it sent them, empty or `null` when it sent none. They may carry terminal escapes: escape before display.
- `conversation_grant` is false for an ask that may be allowed only once: a plan, or one the CLI marks as granting more than the call itself.
- `state` is `pending`, `allowed`, `denied`, `expired` (nobody answered by `expires_at`) or `withdrawn` (the CLI gave the request up: an interrupt, the turn ended). Expired and withdrawn requests were denied. A pending approval past `expires_at` reads `expired`.
- `expires_at` is 10 minutes after the request, or the run's deadline when that is sooner. orbit never allows a request on its own: unanswered is denied.

`POST /approvals/{id}` `{"allow": true, "scope": "once", "message": "optional"}` decides it and returns `200 {"approval": ...}`.

- `allow` is required. `scope` is `once` (default) or `conversation`: allow this call, and answer later requests of the same conversation that the CLI would cover with the same suggestion (or that repeat the same call) without asking, for runs its person's messages start. `conversation` with `allow: false`, or for an approval whose `conversation_grant` is false, is `400`.
- `message` (at most 2000 bytes) is what the model is told with a deny; with an allow it is kept on the approval.
- `409` when it was decided, expired or withdrawn first; `404` for an unknown id. The decider is the token's person; only with the operator token, `"by"` names it (default `operator`).
- The waiting run reads the decision within a second, on whichever instance holds the CLI.

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

## Attention and push

Something waiting on a person is an **attention** row, written in the same transaction as the thing itself, one per thing (`key`): a pending approval (`approval`, key `action:<id>`), a run's question (`question`, `question:<message id>`), a failed run (`failure`, `failure:<run id>`) and a decision: a ticket plan to approve (`decision`, `plan:<ticket id>:<version>`) or a delegated task blocked on a person (`decision`, `action:<task:decision action id>`), and a chat run's permission request (`permission`, `approval:<approval id>`; Approvals). Whether it is still open is read from the thing at query time: the action still `pending`, the question unanswered, the plan the ticket's newest while it is `planned`, a task's decision action still `pending`, a failure until a later run of its conversation succeeds or the person writes there, a permission request while its approval is `pending` and before its `expires_at` (a decision, a withdrawal or a timeout closes it). Dismissing closes any of them for the inbox only (a dismissed approval stays pending).

`GET /attention?limit=<n>` is everything open, newest first (`limit` defaults to and is at most 500):

```json
{"items": [{
  "id": "uuid", "key": "action:uuid", "kind": "approval", "agent": "finance",
  "run_id": "uuid", "conversation_id": "uuid", "action_id": "uuid",
  "message_id": null, "ticket_id": null, "plan_version": null, "approval_id": null,
  "title": "finance wants to webhook:pay", "detail": "<the action's reason>",
  "push": true, "created_at": "...", "link": "/orbit/c/<conversation_id>#action-<action_id>"}]}
```

`title` is the agent's name and a fixed phrase, never anything a run wrote (a permission request: `<agent> needs permission`, or `<agent> has a plan to approve` for a plan; never the tool or its input). `detail` is the question, the action's reason, the run's error, the ticket's title or the permission request's tool, for a signed-in inbox only. `approval_id` is set on a permission request, `null` otherwise. `push` is false for a run a person's message woke (`engine.Attended`): listed, not pushed. A permission request is the exception, `push: true` although only such a run asks one: its turn waits until the person answers, so it is pushed unless the person is looking at the conversation (Presence, below). `link` is where to act, under `ORBIT_PUSH_LINK_BASE`: the conversation, with `#action-<id>` for an approval or a task's decision, `#message-<id>` for a question, `#approval-<approval id>` for a permission request. `POST /attention/{id}/dismiss` is `{"ok": true}` (repeating it is fine; 404 for an unknown id).

**Web Push.** Control sends each attention row with `push` set to every registered device, except those of a person looking at its conversation (Presence, below), straight to the browser's push service (RFC 8291 encryption, RFC 8292 VAPID; `internal/webpush`). The payload is only `{"title", "url", "tag", "kind", "count"}`, plus `actions` on a single pending action or permission request (Push actions, below): the title above and the link. Rows that arrive within 20 s of the previous push (`push.Gap`) wait and go out together as one summary (`"3 things need you: finance, ops"`, url `<base>/inbox`, tag `orbit-inbox`); a row that closed before its turn, or waited more than a day (`push.MaxAge`), is not sent. `tag` and the `Topic` header are the row's key, so a device and the push service each keep one notification per thing. A send that fails is not retried; the inbox still lists it.

**Push actions.** A push of one pending action (`kind: "approval"`) or one permission request (`kind: "permission"`) carries buttons; a summary, a question, a failure or a decision carries none:

```json
{"title": "finance wants to webhook:pay", "url": "/orbit/c/<conversation_id>#action-<action_id>", "tag": "action:<action_id>",
 "kind": "approval", "count": 1,
 "actions": [{"action": "approve", "title": "Approve", "url": "https://orbit.kanishksachdev.com/api/v1/push/act?t=<token>"},
             {"action": "reject", "title": "Reject", "url": "https://orbit.kanishksachdev.com/api/v1/push/act?t=<token>"},
             {"action": "open", "title": "Open"}]}
```

A permission request's buttons are `approve` (Approve: allowed once, `scope: "once"`), `deny` (Deny) and `open`. The service worker shows them as `showNotification(title, {tag, data: payload, actions: payload.actions.map(({action, title}) => ({action, title}))})`; on `notificationclick`, for `open` or a click on the body it opens `url`, and for any other action it POSTs to that action's `url` with no body and no headers (a CORS simple request: no preflight, and the response carries `Access-Control-Allow-Origin: *`), with no session or bearer token, then closes the notification. Buttons are absent when `public_url` is unset.

- `POST /api/v1/push/act?t=<token>` decides as the person whose device was pushed (the subscription's `author`): `200 {"ok": true, "decision": "approve" | "reject" | "deny"}`. The token is signed (keyed from the operator token, `secret.PushAction`), names one action or approval, the decision, the person and an expiry a day after the push (`push.MaxAge`), and carries a nonce every button of one notification shares: the first use of any of them consumes it, so one notification decides at most once. `403` for a token that is not valid or has expired, `409` `"this notification was already acted on"` for a used nonce, and `409` (`"no longer in a state that allows this"`) when the thing was decided, withdrawn or expired first (the nonce is then used too). An approved action is executed as one approved in the app.
- **iOS and iPadOS Safari show no action buttons** on web notifications: a tap opens `url`, so the person decides in the app. macOS Safari shows at most the first two actions in a menu. Chrome and Edge show up to two buttons (`Notification.maxActions`) next to the body, so `approve` and `reject` or `deny`; `open` is the body's click.

**Close pushes.** When a row that was pushed closes (decided anywhere, answered, withdrawn, expired, dismissed), control sends `{"title": "", "url": "", "tag": "<the same tag>", "kind": "close", "count": 0}` with the same `Topic`, to the devices of each person it went to: the worker closes `getNotifications({tag})`, and a push service still holding the original for an offline device replaces it. A row pushed as itself closes with its key's tag; a summary (`orbit-inbox`) is closed once every row summarised to that person has closed. Each notification is closed once, and only within a day of its push (`push.MaxAge`). This is best effort: **devices on Apple's push service (`*.push.apple.com`: iOS, iPadOS and macOS Safari) get no close push**, because Safari counts a push that shows no notification against the subscription and revokes it after a few, so there the notification stays until it is tapped; Firefox counts such pushes against a quota that a visit to the app resets; Chrome may show its own "updated in the background" notice once its allowance of silent pushes is spent.

- `GET /push` is `{"enabled": true, "public_key": "<VAPID public key, base64url>"}` (`enabled: false`, `public_key: null` without keys).
- `POST /push/subscriptions` takes a browser's `PushSubscription.toJSON()` (`{"endpoint", "expirationTime", "keys": {"p256dh", "auth"}}`; `expirationTime` is ignored) and an optional `label`, and returns `201 {"id": "uuid"}`. The endpoint must be https on a known push service (FCM, Apple, Windows, Mozilla; `push.AllowedEndpoint`), else 400. The same endpoint again refreshes its row. 503 when push is not configured.
- `GET /push/subscriptions` is the caller's active devices (`id`, `endpoint`, `author`, `label`, `created_at`, `last_sent_at`, `last_error`, `failures`; never the keys); every device for the operator token.
- `DELETE /push/subscriptions` with `{"endpoint": "..."}` stops pushing to it (404 when none is active). A push service answering 404 or 410 revokes it too.
- `POST /push/test` sends `orbit notifications are on` to the caller's devices now, ignoring the gap: `{"sent": <devices that accepted it>}`.

**Presence.** `POST /presence` `{"conversation_id": "uuid"}` says the caller's app is showing that conversation now; `{"conversation_id": null}` says it shows none. It returns `{"ok": true}`; a `conversation_id` that is not a UUID is `400`, an unknown one `404`, and the body is required (`{}` is the same as `null`). A push for a conversation skips every device of the person whose latest heartbeat named it less than 60 s ago (`push.Presence`); in a summary only that conversation's rows are left out, and the person's inbox and thread still show them. Unknown or stale presence never silences a push: a person who never sent one, whose latest named another conversation or `null`, or whose latest is 60 s old or more, is pushed as usual. The person is the token's `preferred_username` (`operator` for the operator token), the same as a subscription's `author`.

- A client sends it every 30 s while a page is visible (`document.visibilityState === "visible"`), with the conversation it shows or `null`, and at once when that changes; when the page is hidden or closes, it sends `null` (`navigator.sendBeacon` cannot carry the bearer token, so a `fetch` with `keepalive: true`). A missed heartbeat costs nothing: two in a row and pushes resume.
- Presence is one row per person, the latest heartbeat replacing the one before, stored in Postgres so every instance (a rollout runs two) reads the same. Two devices of one person showing different conversations overwrite each other, so a push can reach a person who is looking at its conversation on another device; it is never held back from one who is not.

| variable | meaning |
|---|---|
| `ORBIT_VAPID_PUBLIC_KEY`, `ORBIT_VAPID_PRIVATE_KEY` | the VAPID pair, base64url (`orbit vapid` prints a new one). Both unset: push is off, the inbox still works. Set but invalid or mismatched: `serve` refuses to start. Rotating the pair invalidates every subscription (browsers bind them to the key), so devices subscribe again. |
| `ORBIT_VAPID_SUBJECT` | the contact push services may use, `mailto:` or `https:`. Required with the keys. |
| `ORBIT_PUSH_LINK_BASE` | the app path links start with, default `/orbit` (the portfolio's chat). |


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

- **Read point.** `read_through` beside `items` is the conversation's read point as it was before this request (`null` before the person first read it): a client draws its "New" divider above the…28285 tokens truncated…ame}`) are the agents that run with it, from their spec or attached.

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

### Sharing templates

A template travels as a **bundle**: `{"format": "orbit.templates/1", "templates": [<document>]}`, each document `{"name", "description", "parameters", "spec"}` as a template is written. At most 100 templates and 1 MiB.

- `POST /templates/share` `{"templates": [{"name", "version"?}], "link"?, "expires_in_hours"?}`: `200 {"bundle"}`, the named versions (the current one when `version` is absent, which must be active). With `link: true`, also `"link"` and `"expires_at"`: a URL under `public_url`, `<public_url>/api/v1/templates/shared/<token>`, that serves the bundle to anyone holding it, until it expires (`expires_in_hours`, 1 to 168; default 24). The token names those exact versions and the expiry, signed with a key derived from `ORBIT_API_TOKEN`, so the link always serves the same bundle and rotating the token voids every link. `503` without `public_url`.
- `GET /templates/shared/{token}` takes no `Authorization`: the signature is the credential, as for signed uploads. `200` is the bundle; an altered or expired link is `403`.
- `POST /templates/import` `{"bundle": {...}}` or `{"link": "<another orbit's link>"}`, with `dry_run`, `note`: `200 {"dry_run", "results": [{"name", "outcome": "created"|"updated"|"unchanged", "version", "lowered": [...]}]}`, in bundle order, as `source: "import"` versions with the token's person as `author`. All of it is one transaction; one bad template refuses the import (`400`, naming its index). A link is fetched with one GET: `https` only, a path under `/api/v1/templates/shared/`, no credential sent, no redirect followed, a public address only (checked on the connection, so a name resolving to a private, loopback or tailnet address is refused), within 10 s and 1 MiB.

**What an import is held to**, all by code (`internal/share`) before anything is written:

- **The scan.** Every key and string of the bundle, after JSON escapes are decoded, is checked for characters that hide or reorder text: Unicode tag characters (U+E0000 to U+E007F) and bidirectional controls (U+061C, U+200E, U+200F, U+202A to U+202E, U+2066 to U+2069). One anywhere refuses the import, naming where (`/templates/0/spec/charter: a hidden character U+202E ...`).
- **It grants nothing.** Every action rule in a template's spec that is not `deny` arrives as `approve`: an `auto`, and a slot that could render one, so nothing an import brings runs without a person. `connectors` are removed. `lowered` lists each change (`actions.notify: auto arrives as approve`). An agent created from an imported template is then held to everything any database agent is (Defining agents).


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
 "budgets": [{"agent": "finance", "period": "month",
              "period_start": "2026-11-01T04:00:00Z", "resets_at": "2026-12-01T05:00:00Z",
              "api_equivalent_usd": 0.811000, "runs": 30, "runs_with_cost": 29,
              "max_cost_usd": 5, "max_runs": 40,
              "cost_utilization": 0.1622, "run_utilization": 0.75,
              "alert": false, "exceeded": false}],
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
- `budgets` lists configured agent budgets for the current local calendar period, with API-equivalent cost, runner-started runs, limit utilization, reset time and whether an alert threshold or limit has been reached. Reaching a limit skips the next scheduled run before source fetching or a backend call and raises one pushable attention alert per agent and period; manual runs and messages remain available.
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
