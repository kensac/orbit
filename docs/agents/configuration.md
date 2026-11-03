# Configuration

A deployment is a directory: `orbit.yaml` plus `agents/<name>.yaml`
(`examples/` is complete and loaded by tests). Unknown fields fail the load;
so does any invalid cross-reference. `orbit validate <dir>` checks a
directory and lists the environment variables it references. Every field, its type and default is in [spec-reference.md](spec-reference.md); this file is the rules behind them.

## Agent names

An agent's name is its file's (`^[a-z][a-z0-9-]{0,62}$`), and may not be
one of `spec.ReservedAgentNames`: the UI's own top-level routes under
`/orbit/` (`admin`, `api`, `c`, `new`, `components`, `lib`) and
`orbit-admin`. Such an agent fails the load, naming the reason.

## Agents in the database

Agents are also defined through the API (docs/api.md, Defining agents),
stored as versions in `agent_definitions` and loaded with the files by
`spec.LoadAll`: the same decoding (unknown fields fail), defaults and
validation. A name defined both in a file and in the database fails the
load. The built-in agents (`spec.Builtins`: `orbit`) yield to a file or
database agent of their name. Each instance reloads when the directory's
content or the newest definition changes, so a definition written on one
instance is in force on all within the 30 s poll.

An agent changes agents with the `agent:define` action (`internal/engine/define.go`, the API's own path): enable it per agent (`actions: {agent:define: auto}`, as the built-in `orbit` has it). Each change records the run and the person it answered; `POST /agents/{name}/definitions/{version}/revert` undoes one. A change that widens the agent (`spec.Widening`: a tool, source, credential, delegate target, need, connector or webhook trigger it adds, a source it changes, a move to a sandbox profile, an action type allowed more readily) waits for a person's approval whatever the rule.

In a conversation, `agent:define` lists the names of connectors already configured in `orbit.yaml` and accepts them in the target agent's `connectors` field. Adding one always waits for the person's approval, even when `agent:define` is `auto`. This only grants the target agent access to an existing connector; it does not add a server or start OAuth sign-in. A person signs in separately through `POST /api/v1/connectors/{name}/authorize` when that connector uses OAuth. Agents in configuration files remain read-only and must be changed in their YAML file.

Database and built-in agents are held to what orbit.yaml offers (`spec.validateTrust`): no `file` source, no `${VAR}` that is not in `credentials:`, without a `sandbox`, only the tools in `spec.UnsandboxedTools` (Read, Glob, Grep, WebSearch, WebFetch, Task, TodoWrite), no precheck and no `run` hook. A spec breaking one fails validation, naming it.

### Saving a conversation as an agent

`agent:save {name, description, charter, starters}` (the built-in `orbit` has it, `approve`) turns the conversation it is proposed in into a new agent (`internal/engine/save.go`). The model writes only those words; everything that decides the agent's reach comes from the conversation's record (`store.ConversationFacts`) and is no broader than what happened there:

- `tools`: the tools its runs called (orbit's own `mcp__` tools are given to every run and are not listed). Without a sandbox, a tool outside `spec.UnsandboxedTools` is dropped; with one, the new agent gets the same `sandbox` and `needs`.
- `sources`: the pages fetched with WebFetch, as `http` sources named by host (at most 5; a URL with `${` or `{{` is skipped).
- `actions`: each type a person approved in the conversation (not one a rule approved), as `approve`, never `auto`. Delegation's types are left out: the record does not say which targets were meant.
- `triggers`: every `schedule:add` proposed in the conversation that no person rejected, compiled as `schedule:add` compiles it; a `schedule:add` in the same reply with `agent` set to the new name becomes its schedule instead of an action of its own.
- `runner`, `chat`, `session` and `timezone` from the agent it was saved from, `wake_on_message: true`, and the model's `starters`.

It is proposed as an `agent:define` with `rule: "approve"`, whatever the agent's rules, so a person approves the whole agent, seen as a dry-run diff (`preview`, docs/api.md, Actions). A name already in use is refused at once.

### Approve and always allow

A person approving any action can also make its type `auto` for that agent ("approve and always allow", docs/api.md, Actions): a new revertible version, like an API change.

## Agents from templates

A template is an agent spec with `{{ name }}` slots and typed parameters, kept in a versioned library (docs/api.md, Templates). `POST /templates/{name}/agents` renders one with a person's values, dry-runs it and defines the agent. An agent with `actions: {template:create: approve}` (the built-in `orbit` has it) proposes the same in a conversation: code renders it, asks the person with a form for any `user_prompt` parameter, and records the result as an `agent:define` a person approves whatever the rule (`auto` and `approve` both only offer the type). The agent it creates is a database agent, held to the same limits as any (above).

## Secrets

Never put secret values in YAML. Channels name variables with `*_env`; source urls, headers, query values, Prometheus expressions, lake credential fields and endpoint headers use `${VAR}`. An unset variable is an error, never an empty string.

`credentials:` in `orbit.yaml` names the variables sources and endpoint headers may send, each bound to the URLs it may go to. Lake access-key ID, secret key and session token references are checked against the same endpoint prefix before each signed request:

```yaml
credentials:
  SURE_API_KEY:
    url_prefix: https://sure.example.com/api/
```

`url_prefix` is an http(s) URL with a host and a path ending in `/`, without user, query or fragment. A source sends a credential only in a request whose URL starts with it (redirects included), and an endpoint only to a rendered URL that does; where the URL has no template or placeholder, a mismatch fails the load. A database or built-in agent may reference only credentials; a file agent may also name any other variable (docs/agents/security.md, What a model can define).

## Endpoints

`endpoints:` in `orbit.yaml` are the HTTP targets of `webhook:<name>` actions. The model writes the action's `body` and, when the endpoint has a `path`, the values of its `{name}` placeholders (`path` in the params); the URL, method and headers are the configuration's. Each placeholder is declared in `params` with a `type` (`string` or `integer`); a string also needs a `pattern` (RE2, matching the whole value) or an `enum`, since that is what keeps the path the configuration's, and is at most `max_length` (default and limit 200). Values are checked before the action is stored and again when it is sent, then escaped as one path segment (`/` becomes `%2F`; `.` and `..` are refused). `body_schema` (a JSON Schema) is what the body must satisfy, checked the same two times and shown to the model. A credential in a header is sent only to a rendered URL under its `url_prefix`, and no redirect is followed: a 3xx is the result and fails the action.

Sure (the finance app) categorizes a transaction with `PUT /api/v1/transactions/{id}` and `{"transaction": {"category_id": "<uuid>"}}`, with an API key of read-write scope in `X-Api-Key` (Sure's `Api::V1::TransactionsController#update`); `user_modified: true` keeps the next bank sync from overwriting the category. On hetzner orbit reaches it as `http://plaid:3000` on the docker network:

```yaml
credentials:
  ORBIT_SURE_API_KEY:
    url_prefix: http://plaid:3000/api/
endpoints:
  sure-categorize:
    url: http://plaid:3000/api/v1
    method: PUT
    path: /transactions/{id}
    description: Set one Sure transaction's category. id is the transaction's id, category_id one of the categories in inputs/categories.json.
    params:
      id: {type: string, pattern: "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", description: the transaction's id}
    headers:
      X-Api-Key: ${ORBIT_SURE_API_KEY}
    body_schema:
      type: object
      additionalProperties: false
      required: [transaction]
      properties:
        transaction:
          type: object
          additionalProperties: false
          required: [category_id, user_modified]
          properties:
            category_id: {type: string, pattern: "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"}
            user_modified: {const: true}
```

An agent gives it a rule (`actions: {webhook:sure-categorize: approve}`) and reads the categories as a source (`GET http://plaid:3000/api/v1/categories`, with the same header), and proposes `{"type": "webhook:sure-categorize", "params": {"path": {"id": "<transaction id>"}, "body": {"transaction": {"category_id": "<category id>", "user_modified": true}}}}`. The key must have Sure's read-write scope; a read-only key answers 403 and the action fails.

`expires:` records when a credential orbit cannot inspect stops working, so an alert fires before it does. The Claude Code subscription token (`claude setup-token`) lasts a year and gives no warning; record the day it lapses when you set it:

```yaml
expires:
  CLAUDE_CODE_OAUTH_TOKEN: 2027-10-31
```

Each entry is `orbit_credential_expiry_timestamp_seconds{credential="CLAUDE_CODE_OAUTH_TOKEN"}` (midnight UTC of the date) on every instance that loads the configuration, refreshed on reload; nothing else reads it. Update it with the token.

## S3 lake sources

A `lake` source reads one bounded prefix of gzip newline-delimited JSON from an S3-compatible bucket. It signs only `ListObjectsV2` and `GET` requests with SigV4, returns a JSON array in object-key order, and can feed the existing transforms. Configure a read-only key as environment variables and bind both to the endpoint in `credentials:`; the key values never enter an agent definition or model process.

```yaml
credentials:
  LAKE_ACCESS_KEY_ID: {url_prefix: http://garage:3900/}
  LAKE_SECRET_ACCESS_KEY: {url_prefix: http://garage:3900/}

# In agents/<name>.yaml:
sources:
  - name: aircraft
    kind: lake
    url: http://garage:3900
    bucket: adsb
    prefix: positions/date={{today}}/
    region: garage
    access_key_id: ${LAKE_ACCESS_KEY_ID}
    secret_access_key: ${LAKE_SECRET_ACCESS_KEY}
    max_objects: 100
    max_bytes: 8388608
```

The prefix may use the source date templates. `max_objects` defaults to 100 and is capped at 1000. More objects fail the fetch rather than producing partial data. Compressed object bytes, decompressed NDJSON and resulting JSON are each bounded by `max_bytes`. Lake sources do not run SQL or execute a model-written query; reduce their records with `transforms:`.

## GitHub

`github:` in `orbit.yaml` names the App (`app_id`), the variable holding its
private key (`private_key_env`: PEM, or base64 of the PEM for a one-line env
file), and the `repositories` tickets and `github:merge` may target. Without
it, a request ticket or a `github:merge` rule fails validation.

`read_repositories` (optional) lists the repositories agents may read (`repos`, below). It is separate from `repositories`: reading a repository never permits a ticket or a merge there, and a repository in `repositories` is not readable unless it is listed here too.

`comment_repositories` (optional) lists where `github:comment` may post: `{"repo", "number", "body"}` on an issue or a pull request, as the App, with an `issues: write` token minted for the comment and revoked after it. An agent with a `github:comment` rule fails validation without it. A comment is public to everyone who can see the repository, so give the action `approve` unless the agent's inputs are its own. The App needs the Issues write permission.

`google_actions:` in `orbit.yaml` binds typed Gmail and Calendar actions to fixed write tools on authenticated connectors. Orbit builds Google API arguments from the action's typed fields, calls the configured connector itself and keeps its credential out of the model process. Each binding's tool must be in that connector's `write_tools`; agents that enable the action must also name its connector. An action's rule defaults to `deny`, and set it to `approve` when a person must review each draft or event.

## Repositories

`repos: [{repo: owner/name, ref: main}]` on an agent gives every run the tree of each repository at `ref` (a branch, a tag or a full commit sha; empty is the default branch), fetched by orbit just before the run (`internal/repos`, docs/ARCHITECTURE.md, Repositories):

- Each `repo` must be in `github.read_repositories`; each repository's name (the part after the owner) must be unique within the agent, because its tree is `inputs/repos/<name>/`.
- The agent must list `github` in `needs` (only a host holding the App's key fetches; docs/agents/deployment.md, Capabilities), and its runner must read files: `claude-code` or `codex`, not `anthropic`.
- Claude Code runs also get the read-only repository tools (`git_log`, `git_show`, `git_diff`, `git_blame`, `pr_list`, `pr_view`; docs/api.md, Repositories), so history needs no Bash and GitHub needs no WebFetch.
- `_repos.json` (repo, ref, sha) is an input, so the `unchanged` gate skips a scheduled run only while every repository's commit is the one the last succeeded run read.

## Build

`build:` in `orbit.yaml` turns approved tickets into PRs: `sandboxd` (the service's tailnet `host:port` or `unix:/path`), `token_env` (default `ORBIT_SANDBOX_TOKEN`), `agent` (whose claude-code runner, timeout and `github:merge` rule a build uses; timeout at most `spec.MaxBuildTimeout`), `author` (`"Name <email>"`, the App's bot user), and per repository the `commands` a build may run (argv prefixes of plain words), `protected` globs, `max_diff_lines`, and `trusted_scripts`: the sha256 (lower-case hex) of each setup and cleanup script in the repository's `.orbit/build.yaml` a person reviewed (docs/tickets.md, Setup and cleanup scripts). Each repository must be in `github.repositories`, and today must be `kensac/orbit` (docs/tickets.md, Open decisions). Without the section no ticket is built; approved tickets wait. orbit also needs `CLAUDE_CODE_OAUTH_TOKEN`, the one credential the sandbox receives. `orbit validate` lists both variables.

## Mail

`mail:` in `orbit.yaml` gives orbit a mailbox, so a person can answer its mails and write to it (`internal/mail`, docs/ARCHITECTURE.md, Mail):

```yaml
mail:
  address: orbit@example.com      # bare, no plus part; replies go to its plus form
  imap: {host: mail.example.com, port: 993, username_env: SMTP_USER, password_env: SMTP_PASS, mailbox: INBOX}
  allow: [me@example.com]         # default: every smtp channel's `to`
  agent: assistant                # a mail that is not a reply opens a conversation here
  poll: 1m
```

`port` defaults to 993 (implicit TLS); any other port must offer STARTTLS. `mailbox` defaults to `INBOX` and must be orbit's own: every unseen mail in it is read and marked seen. The server must deliver `local+anything@domain` to the mailbox (Postfix's `recipient_delimiter`). Without `agent`, a mail that is not a reply is recorded and ignored. `poll` is at least 10s. With the section, every mail orbit sends carries a reply address and Message-ID naming its run's conversation.

## Telegram

`telegram:` enables a private Telegram bot inbox. The named channel holds the bot token environment variable; user ids must come from trusted Telegram account data, and group chats are always ignored:

```yaml
channels:
  telegram-main:
    kind: telegram
    token_env: ORBIT_TELEGRAM_BOT_TOKEN
telegram:
  channel: telegram-main
  agent: assistant
  allow_users: [123456789]
  poll: 5s
```

Only private text messages from `allow_users` are acted on. Polling is idempotent by Telegram update id. The first accepted message creates a conversation, later messages continue it, and `/new` opens a separate conversation. Replies and failures go directly to the sending chat even when `notify.when` is `never`. The bot token stays in orbit's environment and is never passed to a model.

## Subscription usage and low-priority agents

```yaml
usage_poll:                          # off without this block
  every: 5m                          # default 5m, at least 1m
  token_env: CLAUDE_CODE_OAUTH_TOKEN # default; the token claude-code runners use
defer_low_priority: true             # hold priority: low agents' scheduled runs while the subscription is nearly spent
```

`usage_poll` makes control ask the endpoint Claude Code's own `/usage` screen reads (`spec.DefaultUsageURL`) with the subscription's OAuth token, and store each answer (`internal/usage`). **Best effort**: the endpoint is undocumented, so a change to it shows as failed polls in `/usage` (`subscription.poll.last_error`) and the windows fall back to what runs' streams reported. Two instances during a rollout share one poll per interval. The token stays on control and is sent only to that URL.

`priority: low` on an agent (default normal) marks work that can wait. With `defer_low_priority`, while any subscription window not yet reset is above `usage.DeferLowAbove` (80%), that agent's scheduled runs are held in the queue, rechecked every `usage.DeferRecheck` (10m) and let go within `scheduler.DeferCheckEvery` (1m) of every window dropping below; each hold is recorded (`run_deferrals`, `/usage` `deferral.held`). Manual runs and a person's messages are never held. The reading is the freshest of polled and stream-reported values, so deferring works without `usage_poll`, on readings only as fresh as the last run.

`budget:` on an agent can set `period: day|week|month`, `max_cost_usd`, and/or `max_runs`. Periods use the agent's timezone and week begins Monday. Usage includes runs that started an attempt, from every trigger, and excludes gate-skipped runs. A scheduled run whose budget is already used is skipped before source fetching or a backend call; manual runs and a person's messages are never blocked. The first blocked run in each period creates one pushable attention alert. `GET /usage` reports each configured budget and marks it `alert` at 80% and `exceeded` at its limit. Costs are backend-reported API-equivalent estimates, so a run in progress can finish above a cost limit and runs with no reported cost are not counted as known spend.

## Capabilities

`needs: [<capability>, ...]` on an agent says what a host must have to run it; a host advertises its own in `ORBIT_CAPABILITIES` (comma-separated; `*` alone is every agent, control's default when unset; a worker's default is none). A capability is `name` or `name:qualifier`, lower case (`spec.CapabilityRE`). An agent with no needs runs on any host. Give an agent a need for anything only some hosts have: a network its sources are on (`docker-network:hetzner`), a source credential that lives only on some hosts (`sure`), the GitHub App's key (`github`, required with `repos` or a `github` source), a tool (`browser`). The table of capabilities and the hosts that have them is docs/agents/deployment.md, Worker fleet. A run whose agent needs what no host has waits queued; nothing fails it.

## Transforms

`transforms:` in an agent runs code over a source before the model sees
it (`internal/transforms`): each is `{name, source, kind, params,
keep_source}`, and its output is the input `inputs/<name>.json`, stored
as its own artifact. A source a transform reads is withheld from the model
(stored as `sources/<name>.<ext>`, never written to the workdir) unless
one of its transforms sets `keep_source`. Kinds and params, all validated
at load (unknown params fail):

| kind | params | output |
|---|---|---|
| `json-filter` | `items`, `where` (required) | the matching records, whole |
| `json-project` | `items`, `where`, `fields` (required) | each matching record's `fields`, keyed by dot path |
| `json-group-sum` | `items`, `where`, `group_by` (required), `sum` | `{records, matched, rows: [{group, count, sum(<field>)}]}`, the query tool's shape |
| `sure-monthly` | `items` | Sure transactions per month and category: income, spending, net, transfers left out |

`items` is the dot path to the records (empty: the document is the array,
as a paginated `http` source is; `.`: the document itself is the one record, as a webhook delivery's event is); `where` is the query tool's conditions
(`field`, `op`, `value`). Sums are exact decimals. A transform that fails
fails the run, naming it. A new kind is a function in
`internal/transforms`, its parameters in `parse`, and a test.

## Prometheus queries

An agent with a `prometheus` source can also query that Prometheus while it works: `prometheus_query` (docs/api.md, Prometheus), instant or range, read-only, bounded (at most 31 days, 1000 points and 500 series per query, 256 KiB per answer), sent to the source's URL with its credentials. The source's own `expr` is still fetched before the run, so a gate or a transform can decide on it with no model call; the tool is for what the run finds it needs to look at. Only `/api/v1/query` and `/api/v1/query_range` are ever requested.

## Gates

`gates:` in an agent are checks, in code, that end a run as `skipped` (naming the gate in `runs.gate`) before any backend call, so a quiet day costs no tokens and sends nothing. They apply only to scheduled and webhook runs with no person's message waiting: a message, a manual run, a continuation, a build and a recovered attempt always reach the model. They are checked in order, after sources and transforms:

| kind | fields | skips when |
|---|---|---|
| `unchanged` | none | every input the model would see (by file name and sha256) is what the agent's newest succeeded run saw, under the same spec |
| `items_above` | `input` (a source or transform), `items`, `n` | `input` has at most `n` records at `items` |
| `precheck` | `command`, `timeout` (default 30s, at most 2m) | `sh -c <command>` exits 1 |

A precheck runs in the run's workdir with the inputs written (`$ORBIT_INPUTS` is their directory), with only `PATH`, `HOME` (the workdir), `LANG`, `TZ`, `ORBIT_AGENT` and `ORBIT_RUN_ID` in its environment. Exit 0 goes on to the model; exit 1 skips, as grep's no match does, and the first line it printed is the skip's reason; any other status, a timeout or a command that cannot start fails the run, naming the status and the last line of stderr. Every precheck is an `orbit.precheck` event with its status, duration and output (4 KB of each). It runs as orbit, so only an agent in the configuration files may have one; a database agent with a precheck fails validation (docs/agents/security.md, Prechecks).

A skipped run counts as a success for staleness (docs/agents/store.md).

## Triggers

`triggers:` in an agent are when it wakes on its own. Each is one of:

- `schedule`: a five-field cron expression or a descriptor (`@daily`), read in the agent's `timezone`.
- `rrule` and `start`: an RFC 5545 recurrence rule (`internal/rrule`), anchored at `start`, a local time in the agent's timezone (`2026-11-03T09:00`). For what cron cannot say: `FREQ=WEEKLY;INTERVAL=2;BYDAY=TU` (every other Tuesday), `FREQ=MONTHLY;BYDAY=-1FR` (the last Friday), `FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=2` (the second weekday), and series that end (`COUNT`, `UNTIL`). `start` sets the first period and the time of day the rule does not name. Supported: `FREQ` from `MINUTELY` to `YEARLY`, `INTERVAL`, `COUNT` (at most 10000), `UNTIL`, `BYMONTH`, `BYMONTHDAY`, `BYDAY` (ordinals in `MONTHLY` and `YEARLY`), `BYHOUR`, `BYMINUTE`, `BYSECOND`, `BYSETPOS`, `WKST`; anything else fails the load. Days and longer are wall-clock (09:00 stays 09:00 across daylight saving); hours and minutes are elapsed time.
- `webhook`: a signed delivery to `POST /hooks/<agent>/<webhook>` (Webhooks, below).

Every kind takes `prompt`, the reason for the wake. `schedule` and `rrule` go through the same scheduler (one fire per trigger and time, `runs_one_per_fire`; after downtime only the newest missed fire), and an ended series never fires again.

`quiet_hours: {start: "22:00", end: "07:00"}` on an agent suppresses scheduled runs in that local wall-clock interval. The start is included, the end excluded, and an end earlier than the start crosses midnight. A run is skipped before source fetches or backend startup if its scheduled fire or first claim is during quiet hours; manual runs and messages remain available. `GET /agents/{name}` and next-fire responses show the next configured fire outside quiet hours. Quiet-hour boundaries remain local across daylight saving changes.

**Missed fires.** A fire that starts too late is not run hours late: a schedule run claimed more than its trigger's `missed_grace` after its fire time (after downtime, or behind a long queue) ends `skipped` with gate `missed`, before anything is read or fetched, and notifies nothing. The default grace is the time from that fire to the trigger's next one, at most an hour (`spec.DefaultMissedGraceMax`): a five-minute trigger's fire may start up to five minutes late, a daily one's up to an hour. Set `missed_grace` (1m to 168h) for a job that must still run late, such as a monthly close. Only a run's first attempt is judged; a missed fire is no success for staleness.

## Webhooks

A webhook trigger starts a run when a sender (GitHub, anything speaking Standard Webhooks) POSTs a signed delivery to `/hooks/<agent>/<webhook>` (docs/api.md, Webhooks). Its secret is an environment variable `orbit.yaml` binds, like a credential, to the agents that may name it, with the sender's signature scheme:

```yaml
# orbit.yaml
webhook_secrets:
  ORBIT_HOOK_GITHUB:
    scheme: github        # X-Hub-Signature-256; or standard (webhook-id, webhook-timestamp, webhook-signature)
    agents: [triage]
```

```yaml
# agents/triage.yaml
triggers:
  - webhook: issues               # POST /hooks/triage/issues
    secret: ORBIT_HOOK_GITHUB
    max_bytes: 262144             # default 1 MiB, at most 8 MiB
sources:
  - name: event                   # the delivery's body, as received
    kind: webhook
```

- **Verified before anything is stored.** The body's HMAC-SHA256 under the secret must match the sender's signature (constant-time). `github` reads `X-Hub-Signature-256` and the delivery id from `X-GitHub-Delivery`; `standard` signs `<webhook-id>.<webhook-timestamp>.<body>` (a `whsec_` secret is base64 after the prefix) and refuses a timestamp more than `spec.WebhookTolerance` (5 minutes) from orbit's clock, which is its replay protection. GitHub signs no timestamp, so there the delivery id is.
- **Once per delivery.** The run and the delivery are one transaction, unique per agent, trigger and delivery id (`webhook_deliveries_once`): a sender's retry, or a second instance receiving it, answers the first run and starts nothing.
- **The body is a source.** A `webhook` source is the body of the delivery that started the run, stored as received (`webhook_deliveries`), so transforms, gates (`items_above` with `items: .` counts one event), the `unchanged` gate and withholding apply as to a fetched source. On a run no delivery started (manual, schedule, message) it is absent: its transforms are skipped and an `items_above` gate on it skips the run. The run's prompt names the delivery id and the event.
- GitHub's `ping` (sent when a hook is created) is answered `200` without a run. A disabled agent refuses deliveries (`409`), and a secret unset in the environment is `503`, never an unauthenticated route.

A database agent may have a webhook trigger only under a secret bound to its name, and adding one widens it (waits for a person). Binding a name no agent has yet lets whoever defines that agent receive the deliveries.

`paused: true` stops a trigger firing; with `paused_until` (a local time, as `start`) it fires again from then, and the fires in between are skipped, not caught up (`spec.TriggerSchedule` applies it, so the scheduler, `next_fires` and validation agree).

### Changing schedules by talking

The actions `schedule:add`, `schedule:pause`, `schedule:resume` and `schedule:remove` change a database or built-in agent's triggers without rewriting its spec (`internal/actions/schedule.go`, `internal/engine/schedule.go`): each applies as the next version through `agent:define`'s path (`DefineAgent`, with the run and the person it answered, refused if the agent changed since the action read it), so it is listed in the agent's definitions and reverted like any version. The target is the agent itself, or one it may delegate to (`agent`).

- `schedule:add {when, prompt}`: `when` is the person's words, compiled by `internal/scheduletext` in the agent's timezone: `daily`, `every day`, `every weekday`, `every monday` (any full weekday), `every other monday`, `the 1st` or `the 1st of every month` (any ordinal), each `at` a time (`8` and `8:00` are the 24-hour clock, `9am` and `9:30pm` explicit). A cron expression is accepted as written. Anything else is refused with what is supported.
- `schedule:pause {trigger, until}`: `trigger` is the index `next_fires` reports (absent: every trigger); `until` is `tomorrow`, a weekday (`monday`, `next monday`: the next one after today), a date (`2026-11-09`, `nov 9`), any of those `at` a time, a local `2026-11-09T08:00`, or `in N hours|days|weeks`. A date alone is its midnight. Absent, the pause lasts until resumed.
- `schedule:resume {trigger}`, `schedule:remove {trigger}` (later indexes move down one).
- `run:now {agent, prompt}` starts a manual run; it changes no spec. A run that no person's message woke cannot start its own agent (that is `continue`, which is bounded).

The model never computes a time. Each proposal is compiled when it is made: a phrase code cannot read, or a trigger that is not there, is denied at once with the reason, and a chat agent's `act` call answers with the compiled rule and the next three fires for it to tell the person (the action's result, when it executes during the turn). The executed action's `result` echoes the same (`rule`, `timezone`, `triggers`, `paused_until`, `next_fires` as local times, `version`), and a pending one shows it as `preview` (docs/api.md, Actions). In a run answering the person's message an `approve` rule on these applies directly, as delegation's does; unattended runs follow the rules as written. The built-in `orbit` has all five under `approve`.

## GitHub and Loki sources

A `github` source reads one repository of `github.read_repositories` as the App: `resource: issues | pulls | checks | releases`, with `state` for issues and pulls, `ref` for checks, `limit` for lists. orbit mints a token of only that resource's permissions for the one fetch and revokes it after (docs/tickets.md, GitHub access), so the model never holds one; the agent needs `github` in `needs`. Lists are projected to the fields a review uses (number, title, state, author, labels, dates, URL, body cut at 4000 bytes), so a quiet repository is a small input and the `unchanged` gate sees only what changed.

A `loki` source runs one LogQL `query_range` (`expr`) ending at the fetch, `since` back (default 1h, at most 24h), at most `limit` lines (default 1000, at most 5000), newest first. Count with a metric query (`sum by (container) (count_over_time({...} |= "error" [1h]))`) rather than reading lines when a number is the answer. Headers render like an http source's, under the credential binding.

```yaml
needs: [github, docker-network:hetzner]
sources:
  - {name: issues, kind: github, repo: kensac/orbit, resource: issues, state: open, limit: 50}
  - {name: main-checks, kind: github, repo: kensac/orbit, resource: checks, ref: main}
  - {name: errors, kind: loki, url: "http://loki:3100", expr: '{container=~"orbit|traefik"} |= "error"', since: 6h, limit: 500}
```

## Templates in sources

URLs, query values, headers and Prometheus expressions may use these, as `YYYY-MM-DD` in the agent's timezone (`sources.Vars.Expand`):

| Template | Is |
|---|---|
| `{{now}}` | the run's start, RFC 3339 |
| `{{today}}` | the run's day |
| `{{month_start}}`, `{{prev_month_start}}`, `{{prev_month_end}}` | the first day of this month, and the first and last day of the previous one |
| `{{days_ago N}}` | N days before today, N from 0 to 3660 (`{{days_ago_N}}`, N from 1 to 366, is the same) |
| `{{months_ago_start N}}`, `{{months_ago_end N}}` | the first and last day of the month N months before this one, N from 0 to 120: `{{months_ago_start 1}}` is `{{prev_month_start}}`, `{{months_ago_end 0}}` this month's last day |

Anything else in `{{...}}`, or a count out of range, fails the fetch naming it.

**Per-trigger windows.** An http source's `by_trigger.<trigger>.query` replaces query values for runs of that trigger, so a person's message can read a wider window than the schedule's review:

```yaml
query:
  start_date: "{{prev_month_start}}"
  end_date: "{{today}}"
by_trigger:
  message:
    query:
      start_date: "{{months_ago_start 3}}"
```

A run's trigger is the run's own (`schedule`, `manual`, `message`, `continue`, `task`, `delegation`, `webhook`); a trigger without an override fetches the source's query. An attempt that recovers an interrupted one reads the inputs that attempt read.

## Adding a kind

Each kind is listed once in `internal/spec` (`channelKinds`, `sourceKinds`,
`runnerKinds`) and implemented once in its package. Add both, plus the
validation of its required fields, plus a test, plus the table in
docs/ARCHITECTURE.md and the kind's row in spec-reference.md. A kind validation accepts that no package implements
fails at runtime, not at load.

## Chat agents

`chat: true` (claude-code runners only) makes an agent a Claude Code session whose reply is its final message, with memory, questions and actions as orbit's tools (docs/ARCHITECTURE.md, Chat agents); each conversation gets its own workspace. `sandbox: workstation` (needs `chat`) runs it in a sandboxd container with Claude Code's full tool set, a kept home and egress to any public host; `sandbox: build` with the tools in its `tools` and the build allowlist; sandboxd is `sandbox: {sandboxd: <host:port | unix:/path>, token_env: ORBIT_SANDBOX_TOKEN}` in orbit.yaml, or the `build:` section's when there is no `sandbox:`. The CLI there needs `CLAUDE_CODE_OAUTH_TOKEN`. Give a sandboxed agent `needs: [sandbox]` so only hosts that reach sandboxd claim it (the built-in has it).

## Permissions

`permissions: {level, ceiling}` is how a chat agent's Claude Code session asks before a tool runs, named as Orca names them and mapped onto Claude Code's permission modes: `locked` is `dontAsk` (the spec's allowlist, everything else denied unasked), `ask` is `default` with the stdio permission prompt, `edits` is `acceptEdits`, `auto` is `auto` where the CLI and model offer it (else `ask`, recorded as an `orbit.permissions` event), `full` is `bypassPermissions`. A level decides which calls run without a person; it never adds a tool (`tools` and the sandbox profile do).

- **Defaults.** A chat in a `sandbox` is `full` with ceiling `full`: the container is the boundary (security.md, Sandboxed chats). Every other agent is `locked`, and an agent that is not a chat runs unattended, so it can be nothing else.
- **Limits.** `level` is at most `ceiling`. Without a `sandbox` the widest is `edits`: `auto` and `full` let a call run without a person on orbit's host, beside its secrets. A level that is not one of the five, or one the agent cannot run, resolves down at run time, never up (`Agent.ResolveLevel`); at load it is an error.
- **What `tools` names is allowed at every level.** A local chat's `tools` are given by name (`--allowedTools`), so no level asks for them; a level decides everything else. A workstation lists none, so only orbit's own tools are pre-allowed there.
- **A conversation may set its own level** within the ceiling (docs/api.md, Conversation permissions), for the runs its person's messages start; unattended runs keep the agent's level, or the conversation's when it is narrower.
- **What asks goes to the person.** At `ask`, `edits` and `auto` a call the level does not let through is an approval in the conversation (docs/api.md, Approvals): answered by its person, denied if nobody answers in 10 minutes. A run its person's message did not start asks nobody: such a request is denied at once.
- **A process can be raised.** Every chat process has the stdio permission prompt, and one whose ceiling is `full` is launched with `--allow-dangerously-skip-permissions` (without it the CLI refuses `set_permission_mode bypassPermissions`).

## Web allowlist

`web: {allow: [example.com, "*.wikipedia.org"]}` limits an agent's `WebFetch` to those hosts when it runs on orbit's host: the bare `WebFetch` in `--allowedTools` becomes one `WebFetch(domain:<d>)` rule per entry, and the private names are denied (spec-reference.md, `web`). It needs `WebFetch` in `tools`; a sandboxed agent goes through its egress profile instead and may not have one. Adding a domain, or dropping the list, widens the agent (`agent:define` waits for a person).

## Connectors

`connectors:` in `orbit.yaml` names MCP servers; an agent's `connectors: [<name>, ...]` gives its runs their tools, as `mcp__<name>__<tool>` (claude-code runners only; docs/ARCHITECTURE.md, Connectors). orbit is the gateway: it lists the tools before each run and makes every call itself with the connector's credential, so neither the CLI nor a sandbox ever holds one.

A signed-in person can also add a public OAuth connector by URL at `POST /api/v1/connectors/catalog` (docs/api.md). Its URL, description, scopes, and creator are stored as configuration; OAuth grants remain separately sealed in `connector_tokens`. These records join the deployment's connector map and are offered to `agent:define`. Every tool of a catalog connector defaults to `approve`; an agent must explicitly set a tool rule to change that behavior.

```yaml
credentials:
  HOUSING_MCP_TOKEN: {url_prefix: https://housing.example.com/}
connectors:
  housing:
    url: https://housing.example.com/mcp
    description: NYC affordable-housing listings.
    auth: {kind: header, header: Authorization, value: "Bearer ${HOUSING_MCP_TOKEN}"}
    write_tools: [create_view, delete_view]
  sheets:
    url: https://sheets.example.com/mcp
    auth: {kind: oauth}
```

- **Auth.** `none`; `header`, whose value may carry only `credentials:` variables bound to a prefix the connector's URL starts with (the same binding as a source's, checked at load and on every call; orbit follows no redirect); or `oauth`, where a person signs in once through `POST /api/v1/connectors/{name}/authorize` (docs/api.md, Connectors) and orbit keeps the token, sealed under `ORBIT_CONNECTOR_KEY` (at least 32 characters, on every host that calls the connector), a new version per sign-in and per refresh, none deleted. An oauth connector nobody has signed in is left out of runs, and the run's prompt says so.
- **Rules.** A tool in `write_tools` waits for a person (`approve`); any other runs when called (`auto`), the server's `readOnlyHint` notwithstanding, since the deployment says what changes the world. An agent overrides one tool with `mcp:<connector>:<tool>: auto | approve | deny` in `actions`; `deny` is not offered at all. An `approve` call in a run answering its person's message is asked for in the chat: it becomes a pending action `connector:<connector>/<tool>` the person approves or rejects (`POST /actions/{id}/approve`), and the call runs once they approve, its result going back to the model in the same turn. In any other run it is recorded as that pending action and the model is told it waits; orbit executes it at most once after approval, from the action loop, with the result on the action. An `auto` call to a `write_tools` tool also leaves an executed action as its record. Unlike an `act` action in a chat, which a run answering its person executes on their message, a connector call waits for the person to approve its exact arguments even then: they never saw them. Approving with `always` (docs/api.md, Actions) sets that tool's `mcp:<connector>:<tool>` to `auto`.
- **Results.** A result over 32 KiB is written into the attempt's inputs (`inputs/.../connectors/<connector>-<tool>-<n>.json`) and stored as an artifact; the model is given its path. Every call is an `orbit.connector` event with its bounded result.
- **Hosts.** A worker that runs an agent with connectors needs their credentials (`ORBIT_CONNECTOR_KEY`, the header variables, a `client_secret_env`); give such an agent a capability only those hosts advertise (docs/agents/deployment.md, Capabilities). `orbit validate` lists the variables.
- `transport: stdio` is refused: a local server would run in sandboxd, which is not built.

### Typed Google writes

`google_actions.gmail_draft` fixes the connector and tool for `gmail:draft`.
That tool receives Google `users.drafts.create` arguments with `userId: "me"`
and a base64url encoded RFC 2822 plain text message. The action accepts `to`,
optional `cc` and `bcc`, `subject` and `body`; it cannot send the draft.

`google_actions.calendar_create` fixes the connector, tool and `calendar_id`
for `calendar:create` (default `primary`). The tool receives Google
`events.insert` arguments with the configured calendar and a code-built event.
Start and end must be RFC 3339 timestamps with an end after the start; an
optional IANA `time_zone` and at most 50 attendees are supported. Neither
action accepts a connector, tool, URL, account or calendar destination from
the agent. The connector must authenticate and list the fixed tool in
`write_tools`. OAuth scopes must allow the requested Gmail or Calendar write.

## Sessions and continuations

`session` (claude-code runners only) is `{resume: true, idle: <=24h, live:
<=24h}`; `idle` defaults to 6h, `live` to 0 (no live process) and may not exceed
`idle`. Linux closes idle live processes when memory is pressured. A process
whose credential has a shorter expiry is forcibly stopped at that deadline.
Reloading configuration keeps a live process when its invocation and session
are still compatible. The persistent chat-box integration passes its proxy
grant expiry as the process hard deadline and preserves the box and workspace
when stopping the CLI; that integration is tracked separately by W11. `live`
is refused with a runner's
`max_budget_usd`, because the CLI counts that budget over a process's whole
life, every run fed into it. An agent with `session` needs `ORBIT_DATA_DIR`
on a persistent volume; without one every run after a restart starts fresh
(it does not fail).

`continue` is an action type like any other: absent means denied. Its bounds
are constants in `internal/actions` (`ContinueMinDelay`, `ContinueMaxDelay`,
`MaxContinueChain`).

## Delegation

`delegates: [<agent>, ...]` (or `["*"]`, every configured agent, itself included) names the agents an agent may hand tasks to; an unknown name fails the load. `delegate_templates: [<template>, ...]` (or `["*"]`, every active template) lets `delegate` instantiate typed workers from those active templates. A template worker is fresh, and its connectors are the intersection of the parent agent and rendered template; a task may narrow that set further. `delegate` needs at least one of `delegates` or `delegate_templates` and is denied unless named, like any action. `task:message`, `task:cancel` and `task:retry` require `delegates` and follow the `delegate` rule unless named themselves. In a run answering the person's message, `approve` on these applies directly (docs/ARCHITECTURE.md, Delegation). Bounds are constants in `internal/store` (`MaxTasksPerDelegation`, `MaxTaskDepth`, `MaxConversationTasks`, `MaxTaskFailures`).

## Checks

`verify: [figures]` makes every money amount (`$` followed by a number) in an
agent's summary, notification title and report traceable to its inputs or to
a result of orbit's query or calc tool (docs/ARCHITECTURE.md). Use it for
agents whose reports are numbers; prose with incidental amounts from the
charter or memory gets those amounts sent back and then noted.

## Notifications

`notify.when` (`always`, `agent`: when the run asks, or `never`) applies to
unattended runs: schedule, manual, continue, build. A run answering a
person's message never notifies, whatever it says: the person is reading
the chat. Failures and pending approvals of unattended runs always notify, except a failure that holds its runner (the backend refused orbit's login, or the subscription window is spent): that sends one notification for the runner (or, for a run a person started, the run's own failure report), and the runner's unattended runs wait until it answers again (docs/ARCHITECTURE.md, Reliability).
Another exception is a run that read a mail (`mail:`): its report, or a short note when it fails, is mailed to the sender as a reply, through the agent's first `smtp` channel (else the deployment's first by name) and no other channel, whatever `notify.when` is.

## Memory topics

`topics: [finance]` names the shared memory topics whose entries every run of the agent reads, beside the profile's and its own (docs/ARCHITECTURE.md, Memory entries). `topics: ["*"]` (alone) is every topic, as the built-in `orbit` agent has it. A topic exists as soon as an entry or an agent names it; the API can attach more (`PUT /agents/{name}/topics/{topic}`).

## Hooks

`hooks` adds Claude Code hooks to an agent's runs (claude-code runners only; at most 20), beside orbit's own path guard, figure check and pinned entries (UserPromptSubmit). Each names an `event` (`PreToolUse`, `PostToolUse`, `UserPromptSubmit`, `SessionStart`, `Stop`), for a tool event optionally a `matcher` (tool names joined by `|`, as Claude Code reads them), and exactly one of:

- `say: <text>` (at most 2 KB): tells the model the text as context at that event; at `Stop`, sends the model back once with it (a second stop in the same turn passes, so it cannot loop).
- `deny: <reason>` (`PreToolUse` only): refuses the matching tool with the reason, which the model reads.
- `run: [argv]` with `timeout` (default 10s, at most 1m): runs the command, no shell, with the event's JSON on stdin, in the run's workdir, with an environment of only `PATH`, `HOME` (the workdir) and `LANG`, killed with its process group at the timeout. Its stdout, stderr and exit code go to Claude Code as a command hook's do (exit 2 blocks, with stderr shown to the model).

```yaml
hooks:
  - event: UserPromptSubmit
    say: Lead every report with the total.
  - event: PreToolUse
    matcher: WebFetch
    deny: This agent works from its inputs only.
  - event: PostToolUse
    matcher: Write|Edit
    run: [gofmt, -l, .]
```

orbit's binary runs each (`orbit agent-hook`, its definition a flag orbit writes), so a hook is in the run's arguments: a changed hook starts a new live process. `SessionStart` fires once per Claude Code process, so in a live process not for every run.

## Skills

`skills: [monthly-close]` names skills from orbit's library the agent runs with (docs/ARCHITECTURE.md, Skills). A name no skill has is skipped at run time and shown as `missing` in `GET /agents/{name}/context`; the API attaches more. An agent writes skills only with `actions: {skill:write: approve}` (or `auto`): unnamed, it writes none.

## Learning

Every agent is learned from (docs/ARCHITECTURE.md, Memory entries): a conversation quiet for `learn.idle` (default 30m, between 5m and 24h) with something new from the person gets one learning run. `learn: {enabled: false}` turns it off; `learn.runner` names a runner for learning runs instead of the agent's own (a cheaper model).

## Changing a running deployment

orbit polls its config directory and reloads on any content change. A change
that fails to load is refused and the previous configuration stays in force:
check `orbit_config_valid` and the log after a deploy.
