# Spec field reference

Every field of `orbit.yaml` and `agents/<name>.yaml`, from `internal/spec` (`spec.go`, `build.go`, `mail.go`). An agent defined through the API is the same fields as JSON. Unknown fields fail the load; durations are Go strings (`"15m"`); `${VAR}` and `*_env` name environment variables, and an unset one is an error, never an empty string. The rules behind the fields are in [configuration.md](configuration.md). An agent defined in the database is limited further than a file agent: see [security.md](security.md). `TestReferenceCoversSpec` fails when a struct's yaml fields and its table here differ.

## orbit.yaml

| Field | Type | Default | What it does |
|---|---|---|---|
| `timezone` | IANA zone | `UTC` | The zone schedules and source templates are read in, unless an agent names its own. A zone the tz database does not have fails the load. |
| `public_url` | URL | none | Where a person reaches the API; links in notifications and search results. |
| `defaults` | map | | Values every agent gets unless it sets its own (below). |
| `channels` | map of name to channel | none | Notification destinations an agent's `notify.channels` names. |
| `endpoints` | map of name to endpoint | none | HTTP targets the `webhook:<name>` action may call. |
| `runners` | map of name to runner | none | Model backends an agent's `runner` names. |
| `github` | map | absent | The GitHub App orbit acts as. Without it, tickets naming a repository and `github:merge` fail validation. |
| `build` | map | absent | How approved tickets become PRs. Without it no ticket is built. |
| `sandbox` | map | `build`'s sandboxd | The sandboxd sandboxed chat agents use (below). |
| `extensions` | map | absent | Off-by-default, digest-pinned native source operations executed by sandboxd; package pins and explicit agent grants are below. |
| `mail` | map | absent | orbit's mailbox. Without it orbit reads no mail and its mails carry no reply address. |
| `telegram` | map | absent | Two-way Telegram bot reader; updates from explicitly allowed private users start or continue conversations. |
| `credentials` | map of variable to credential | none | The variables sources and endpoint headers may send, each only to URLs under its prefix (below); the only `${VAR}`s a database or built-in agent may reference. |
| `connectors` | map of name to connector | none | MCP servers agents may be given; orbit calls them on the agents' behalf and holds their credentials (below). |
| `google_actions` | map | absent | Fixed connector tools for typed Gmail draft and Calendar create actions (below). |
| `expires` | map of variable to `YYYY-MM-DD` | none | When a credential orbit cannot inspect stops working (the Claude Code subscription token), exported as `orbit_credential_expiry_timestamp_seconds{credential}` at midnight UTC of the date so an alert fires before it lapses. Nothing else reads it. |
| `webhook_secrets` | map of variable to webhook secret | none | The signing secrets inbound webhooks are verified with, each bound to the agents whose webhook triggers may name it (below). |
| `usage_poll` | map: `every` (default 5m, at least 1m), `token_env` (default `CLAUDE_CODE_OAUTH_TOKEN`) | absent | Poll the subscription's usage windows (configuration.md, Subscription usage and low-priority agents). |
| `defer_low_priority` | bool | `false` | Hold `priority: low` agents' scheduled runs while a usage window is above `usage.DeferLowAbove`. |
| `board` | map | absent | Tunes `GET /board` (below). |
| `titles` | map | absent | How a conversation is titled once its first message names it (below). Without it the title is that message's first line. |

### `titles`

| Field | Type | Default | What it does |
|---|---|---|---|
| `runner` | runner name | required | The runner (a cheap model) that titles each new conversation once from its first message, unless a person named or renamed it; the first line stays when it fails. One call per conversation, ever. |

### `defaults`

| Field | Type | Default | What it does |
|---|---|---|---|
| `runner` | runner name | none | An agent's runner when it names none. An agent with neither fails the load. |
| `timeout` | duration | `15m` | An agent's run timeout when it names none. |
| `channels` | list of channel names | none | An agent's `notify.channels` when it names none. Each must exist. |
| `memory_max_bytes` | int | `16384` | The largest memory document a run or the API may write. |
| `decision_stall` | duration | `24h` | How long a delegated task's decision may wait for a person before, with nothing else in its batch able to move, its parent is woken (docs/ARCHITECTURE.md, Delegation). |

### `channels.<name>`

Names match `^[a-z][a-z0-9-]{0,62}$`.

| Field | Type | Default | What it does |
|---|---|---|---|
| `kind` | `ntfy`, `smtp`, `webhook`, `telegram`, `log` | required | The adapter. `log` writes to orbit's log and takes no other field. |
| `url` | URL | required for ntfy, webhook | The ntfy server, or the URL a webhook channel POSTs to. |
| `topic` | string | | ntfy topic. One of `topic` or `topic_env` is required for ntfy. |
| `topic_env` | env var | | ntfy topic read from the environment, so it stays out of git. Wins over `topic`. |
| `token_env` | env var | none | ntfy bearer token. |
| `host` | string | required for smtp | SMTP server. |
| `port` | int | required for smtp | SMTP port. |
| `username_env` | env var | none | SMTP user; without it orbit sends unauthenticated. |
| `password_env` | env var | none | SMTP password. |
| `from` | address | required for smtp | Sender. |
| `to` | list of addresses | required for smtp | Recipients; also the mail allowlist when `mail.allow` is empty. |
| `chat_id` | Telegram chat id | required for telegram | Default recipient for outbound Telegram notifications. |
| `headers` | map | none | Webhook channel headers; values may use `${VAR}`. |

### `endpoints.<name>`

| Field | Type | Default | What it does |
|---|---|---|---|
| `url` | absolute http(s) URL | required | Where the action sends its body, before `path`. The model never chooses it. |
| `method` | `POST`, `PUT`, `PATCH` | `POST` | HTTP method. |
| `headers` | map | none | Request headers; values may use `${VAR}`. A credential is sent only to a URL under its `url_prefix`. |
| `description` | string | none | What the endpoint does, shown to the model with the action. |
| `path` | `/...` with `{name}` placeholders | none | Appended to `url`; each placeholder is filled from the action's `path` params, escaped as one path segment. No query, fragment, `%`, backslash, space or dot segment; with it, `url` has no query or fragment. |
| `params` | map of placeholder to param | none | One entry per placeholder of `path`, and no other (below). |
| `body_schema` | JSON Schema | any object | What the action's `body` must satisfy, checked before the action is stored and again before it is sent; shown to the model. |

### `endpoints.<name>.params.<name>`

| Field | Type | Default | What it does |
|---|---|---|---|
| `type` | `string`, `integer` | required | An integer is a non-negative JSON integer. |
| `pattern` | RE2 regular expression | none | The whole value must match. A string needs `pattern` or `enum`. |
| `enum` | list of strings | none | The only values allowed. |
| `max_length` | int | `200` | A string's longest value, at most 200. |
| `description` | string | none | What the value is, shown to the model. |

### `runners.<name>`

| Field | Type | Default | What it does |
|---|---|---|---|
| `kind` | `claude-code`, `codex`, `anthropic` | required | The backend. Only `claude-code` takes `tools`, `session`, skills and orbit's MCP tools. |
| `model` | string | the CLI's default | Model passed to the backend; required for `anthropic`. |
| `max_turns` | int | none | claude-code `--max-turns`, per message. |
| `max_budget_usd` | number | none | claude-code `--max-budget-usd`, per process; refused with `session.live`. |
| `binary` | path | `claude`, `codex` | The CLI to run. |
| `api_key_env` | env var | required for anthropic | The API key's variable. |
| `max_tokens` | int | `8192` | anthropic `max_tokens`. |

### `github`

| Field | Type | Default | What it does |
|---|---|---|---|
| `app_id` | int | required | The App's id. |
| `private_key_env` | env var | required | The App's PEM private key, or base64 of it. Read by orbit, never by a model. |
| `api_url` | URL | `https://api.github.com` | GitHub's API (tests, Enterprise). |
| `repositories` | list of `owner/name` | required | The repositories tickets and `github:merge` may target; every token is limited to one. |
| `read_repositories` | list of `owner/name` | none | The repositories an agent's `repos` may name: orbit mirrors them and mints read tokens for them. Reading one never permits a ticket or a merge there (configuration.md, Repositories). |
| `comment_repositories` | list of `owner/name` | none | Where `github:comment` may post, with an `issues: write` token per comment. Commenting never permits a push, a ticket or a merge. |

### `build`

| Field | Type | Default | What it does |
|---|---|---|---|
| `sandboxd` | `host:port` or `unix:/path` | required | The build sandbox service. |
| `token_env` | env var | `ORBIT_SANDBOX_TOKEN` | sandboxd's bearer token. |
| `agent` | agent name | required | Whose claude-code runner, timeout (at most 50m) and `github:merge` rule a build uses. |
| `author` | `Name <email>` | required | Commit identity: the App's bot user. |
| `repositories` | map of `owner/name` to policy | required | Repositories built; each must be in `github.repositories`, and today must be `kensac/orbit`. |

### `build.repositories.<owner/name>`

| Field | Type | Default | What it does |
|---|---|---|---|
| `commands` | list of argv prefixes | required | What a build's Bash calls may start with (`go test`, `make check`), plain words only. |
| `protected` | list of globs | none | Paths a build may not change (`**` for any depth); `.github/**` always is. |
| `max_diff_lines` | int | `2000` | Added plus deleted lines a build may produce, at most 20000. |
| `trusted_scripts` | list of sha256 (lower-case hex) | none | The setup and cleanup scripts in the repository's `.orbit/build.yaml` a person reviewed; any other script fails the build. |

### `sandbox` (orbit.yaml)

| Field | Type | Default | What it does |
|---|---|---|---|
| `sandboxd` | `host:port` or `unix:/path` | `build.sandboxd` | The sandbox service for chat agents with `sandbox`. |
| `token_env` | env var | `ORBIT_SANDBOX_TOKEN` | sandboxd's bearer token. |

### `extensions`

| Field | Type | Default | What it does |
|---|---|---|---|
| `enabled` | bool | `false` | Enables native, pure-source extension operations; requires sandboxd. |
| `packages` | map of package id to pin | none | Deployment-approved package directory and exact package digest. |

### `extensions.packages.<id>`

| Field | Type | Default | What it does |
|---|---|---|---|
| `path` | absolute path | required | Directory containing the validated package bytes. |
| `sha256` | lowercase sha256 | required | Exact package digest; no tag or floating version is accepted. |

### `credentials.<VAR>`

| Field | Type | Default | What it does |
|---|---|---|---|
| `url_prefix` | http(s) URL, path ending in `/` | required | Every request the variable is rendered into (a source's, redirects included, or an endpoint's), must start with it; no user, query or fragment (configuration.md, Secrets). |

### `connectors.<name>`

Names match `^[a-z][a-z0-9-]{0,62}$`; `orbit` is taken. The model calls a connector's tools as `mcp__<name>__<tool>` (configuration.md, Connectors).

| Field | Type | Default | What it does |
|---|---|---|---|
| `transport` | `http` or `stdio` | `http` | MCP streamable HTTP, or a package-pinned local stdio server in sandboxd's networkless extension profile. |
| `url` | http(s) URL | required for `http` | The HTTP server's MCP endpoint. Stdio connectors cannot set it; orbit follows no redirect from HTTP endpoints. |
| `package` | extension package ID | required for `stdio` | Exact package pin in `extensions.packages`; stdio packages may declare only local `tool` operations without input slots or endpoint bindings. |
| `description` | string | none | What the server is for, shown to the model. |
| `auth` | map | `kind: none` | How orbit signs the server's requests (below). |
| `write_tools` | list of tool names | none | The server's tools that change the world: each waits for a person unless the agent's rules say otherwise. Every other tool runs at once. |
| `default_tool_rule` | `auto` or `approve` | none | Internal deployment setting used for person-added connectors; an explicit agent tool rule takes precedence. |

### `connectors.<name>.auth`

| Field | Type | Default | What it does |
|---|---|---|---|
| `kind` | `none`, `header`, `oauth` | `none` | `header` sends one header; `oauth` sends a token a person signed in for (docs/api.md, Connectors). |
| `header` | header name | required for `header` | The header, such as `Authorization`. |
| `value` | string | required for `header` | Its value; each `${VAR}` must be in `credentials` with a `url_prefix` the connector's `url` starts with. |
| `scopes` | list of strings | the server's `scopes_supported` | What an `oauth` sign-in asks for. |
| `client_id` | string | none | An `oauth` client registered by hand; without it orbit registers itself (dynamic client registration). |
| `client_secret_env` | env var | none | That client's secret, for a confidential client; needs `client_id`. |

### `google_actions`

| Field | Type | Default | What it does |
|---|---|---|---|
| `gmail_draft` | map | absent | Fixed connector and write tool used by `gmail:draft`. |
| `calendar_create` | map | absent | Fixed connector, write tool and calendar used by `calendar:create`. |

### `google_actions.gmail_draft`

| Field | Type | Default | What it does |
|---|---|---|---|
| `connector` | connector name | required | Connector must authenticate and be named by any agent enabling `gmail:draft`. |
| `tool` | tool name | required | Must be listed in that connector's `write_tools`; receives Google `users.drafts.create` arguments. |

### `google_actions.calendar_create`

| Field | Type | Default | What it does |
|---|---|---|---|
| `connector` | connector name | required | Connector must authenticate and be named by any agent enabling `calendar:create`. |
| `tool` | tool name | required | Must be listed in that connector's `write_tools`; receives Google `events.insert` arguments. |
| `calendar_id` | string | `primary` | Fixed destination calendar. The agent cannot choose it. |

### `webhook_secrets.<VAR>`

| Field | Type | Default | What it does |
|---|---|---|---|
| `scheme` | `github`, `standard` | required | How the sender signs: GitHub's `X-Hub-Signature-256`, or Standard Webhooks (`webhook-id`, `webhook-timestamp`, `webhook-signature`; a `whsec_` secret is base64 after the prefix) (configuration.md, Webhooks). |
| `agents` | list of agent names | required | The agents whose webhook triggers may name the secret; any other agent naming it fails the load. |

### `mail`

| Field | Type | Default | What it does |
|---|---|---|---|
| `address` | bare address | required | orbit's address, with no plus part; replies go to `local+<token>@domain`. |
| `imap` | map | required | How the mailbox is read (below). |
| `allow` | list of addresses | every smtp channel's `to` | Senders whose mail orbit acts on. Must end up non-empty. |
| `agent` | agent name | none | Receives a mail that is not a reply, in a new conversation. Without it such mail is recorded and ignored. |
| `poll` | duration | `1m` | How often the mailbox is read; at least `10s`. |

### `mail.imap`

| Field | Type | Default | What it does |
|---|---|---|---|
| `host` | string | required | IMAP server. |
| `port` | int | `993` | 993 is implicit TLS; any other port must offer STARTTLS. |
| `username_env` | env var | required | IMAP user. |
| `password_env` | env var | required | IMAP password. |
| `mailbox` | string | `INBOX` | Folder read. Every unseen mail in it is read and marked seen, so it must be orbit's own. |

### `telegram`

| Field | Type | Default | What it does |
|---|---|---|---|
| `channel` | channel name | required | A configured `channels.<name>` with kind `telegram`; its `token_env` names the bot token. |
| `agent` | agent name | required | Agent for a Telegram message that is not a reply to a known Orbit message. |
| `allow_users` | list of Telegram user ids | required | Only these users in private chats may message Orbit. |

### `board`

| Field | Type | Default | What it does |
|---|---|---|---|
| `idle` | duration | `30m` | How long a conversation with nothing waiting stays on the board after its last activity; after it the row is `idle`, left out unless asked for (docs/api.md, The board). |

## Agent

`agents/<name>.yaml`, or the `spec` of `POST /api/v1/agents`.

| Field | Type | Default | What it does |
|---|---|---|---|
| `name` | `^[a-z][a-z0-9-]{0,62}$` | required | The agent's name; a file's must match its file name, and it may not be reserved (configuration.md, Agent names). |
| `description` | string | none | One line shown in agent lists and to MCP clients. |
| `charter` | string | required | The standing instruction every run starts from. |
| `starters` | list of strings | none | Prompts a client offers for a new conversation; at most 6, each at most 500 bytes. "Save this conversation as an agent" drafts them. |
| `enabled` | bool | `true` | `false` stops its triggers firing; manual runs still work. A database agent's comes from its definition, not its spec. |
| `timezone` | IANA zone | `orbit.yaml` timezone | Zone for triggers and source templates; parsed once at load (`Agent.Location`), and one the tz database does not have fails it. |
| `runner` | runner name | `defaults.runner` | The backend. |
| `timeout` | duration | `defaults.timeout` | How long one run may take. |
| `budget` | map (below) | none | API-equivalent cost and runner-started run limits per local period; scheduled runs are skipped once a limit is reached. |
| `quiet_hours` | map (below) | none | Suppress a scheduled run if its fire or claim time is within the local interval. |
| `calendar_skip` | map (below) | none | Check a named read-only connector tool for an overlapping event before a scheduled run. |
| `triggers` | list | none | When the agent wakes on its own (below). |
| `sources` | list | none | Data orbit fetches before a run (below). |
| `transforms` | list | none | Code over a source before the model sees it (below). |
| `gates` | list | none | Checks that skip a scheduled or webhook run before any backend call (below). |
| `tools` | list of tool names | none | Extra Claude Code tools (below). |
| `web` | map (below) | absent | Limits `WebFetch` to named domains on orbit's host. |
| `actions` | map of type to rule | every type `deny` | The writes the agent may propose and what happens to each (below). |
| `notify` | map | | Where unattended runs report (below). |
| `wake_on_message` | bool | `false` | A person's message starts a run (the API's `wake` overrides it per message). |
| `session` | map | absent | Keep the Claude Code session between runs (below). |
| `verify` | list: `figures` | none | Every money amount in the summary, title and report must trace to an input or a query or calc result. |
| `topics` | list of names | none | Shared memory topics every run reads, beside the profile's and its own; `["*"]` (alone) is every topic. |
| `skills` | list of names | none | Skills from orbit's library the agent runs with; an unknown one is skipped and shown as `missing`. |
| `learn` | map | on | Learning runs over quiet conversations (below). |
| `hooks` | list | none | Claude Code hooks for the agent's runs, beside orbit's own; claude-code only, at most 20 (below). |
| `chat` | bool | `false` | A Claude Code session whose reply is its final message, with memory, questions and actions as orbit's tools; a workspace per conversation. claude-code only. |
| `sandbox` | `workstation`, `build` | none | Needs `chat` and a sandboxd. Runs the CLI in a sandboxd container per conversation: `workstation` has every Claude Code tool and egress to any public host; `build` has its `tools` and the build allowlist (security.md, Sandboxed chats). A `workstation` agent also has a box for terminals (docs/workspace.md). |
| `permissions` | map (below) | full in a `sandbox`, else locked | How a chat session asks before a tool runs, and how far a conversation may raise it. |
| `needs` | list of capabilities | none | What a host must have to run the agent (`docker-network:hetzner`); without any, every host. |
| `delegate_templates` | array of template names or `*` | none | Active templates this agent may use for typed delegate tasks. Template workers are fresh and their connectors are narrowed to the intersection of the parent and rendered template, optionally further narrowed per task. |
| `delegates` | list of agent names, or `["*"]` | none | Agents this one may hand tasks to; the `delegate` action also needs a rule. |
| `connectors` | list of connector names | none | `orbit.yaml` connectors whose tools the agent's runs may call; claude-code only. Each tool's rule is `mcp:<connector>:<tool>` in `actions`. Adding one widens the agent. |
| `repos` | list (below) | none | Repositories every run reads: the tree at a commit under `inputs/repos/<name>/`, plus orbit's history and pull request tools. Each must be in `github.read_repositories`; needs `github` in `needs` and a claude-code or codex runner. |
| `priority` | `normal`, `low` | `normal` | `low` is work that can wait: with `defer_low_priority`, its scheduled runs are held while the subscription is nearly spent. |

### `budget`

| Field | Type | Default | What it does |
|---|---|---|---|
| `period` | `day`, `week`, `month` | required | Local calendar period in the agent's timezone; weeks start Monday. |
| `max_cost_usd` | number | none | API-equivalent cost limit for runner-started runs in the period. |
| `max_runs` | integer | none | Runner-started run limit in the period. |

At least one limit is required. Once either limit is reached, scheduled runs
are skipped before sources or a model are called; manual runs and messages
still run. The first blocked run per agent and period creates one dismissible
attention alert and Web Push notification. `/usage` reports current use and
alerts at 80% of either limit. A run already underway may finish past a cost
limit; cost is the backend's reported API-equivalent estimate, and runs with
no cost report are visible in `runs_with_cost` coverage.

### `triggers[]`

| Field | Type | Default | What it does |
|---|---|---|---|
| `schedule` | cron (five fields) or descriptor | | When to fire, in the agent's timezone. Exactly one of `schedule`, `rrule` or `webhook`. |
| `rrule` | RFC 5545 rule | | A recurrence cron cannot say (configuration.md, Triggers). |
| `start` | local time `2026-11-03T09:00` | required with `rrule` | The rule's first period and its time of day; not allowed with `schedule`. |
| `prompt` | string | none | The reason for this wake, appended to the charter. |
| `paused` | bool | `false` | The trigger does not fire (`schedule:pause` writes it). |
| `paused_until` | local time `2026-11-09T00:00` | none | Needs `paused`: the pause ends there; fires before it are skipped, never caught up. |
| `webhook` | `^[a-z][a-z0-9-]{0,62}$` | | A signed delivery to `POST /hooks/<agent>/<webhook>` starts a run (configuration.md, Webhooks); unique per agent. |
| `secret` | variable | required with `webhook` | The signing secret: one of `webhook_secrets`, listing this agent. |
| `max_bytes` | int | `1048576` (1 MiB) | `webhook` only: the largest body accepted, at most 8 MiB; a larger one is `413`. |
| `missed_grace` | duration, `1m` to `168h` | the time to the trigger's next fire, at most `1h` | `schedule` and `rrule` only: a fire claimed later than this is skipped as `missed` instead of run (configuration.md, Triggers). |

### `sources[]`

Fetched by orbit with orbit's credentials; the model sees only the data, as `inputs/<name>.<ext>`.

| Field | Type | Default | What it does |
|---|---|---|---|
| `name` | `^[a-z][a-z0-9-]{0,62}$` | required | The input's name; unique among sources and transforms. |
| `kind` | `http`, `prometheus`, `loki`, `github`, `lake`, `file`, `webhook`, `extension` | required | How it is fetched (below). `webhook` takes only `name` and `max_bytes`. |
| `extension` | map | required for extension | The explicitly granted package operation and its parameter/input bindings (below). |
| `url` | URL | required for http, prometheus, loki, lake | Target; may use `${VAR}` and `{{today}}`-style templates except for lake, which requires a fixed S3-compatible endpoint. A credential in any field of the source is sent only under its `url_prefix`. |
| `headers` | map | none | http and loki request headers; values may use `${VAR}`. |
| `query` | map | none | http query values; may use `${VAR}` and templates. |
| `paginate` | map | absent | Page-numbered http pagination (below). |
| `expr` | PromQL or LogQL | required for prometheus, loki | prometheus: an instant query sent to `<url>/api/v1/query`; the input is its `data`. loki: a `query_range` sent to `<url>/loki/api/v1/query_range` ending now, newest first; the input is `{resultType, result}` (Loki's stats left out). |
| `since` | duration | `1h` | loki: how far back the query reads, at most `24h`. |
| `limit` | int | loki `1000`, github `100` | loki: the most lines, at most 5000. github: the most items of a list, at most 1000 (300 for pulls). |
| `repo` | `owner/name` | required for github | One of `github.read_repositories`. |
| `resource` | `issues`, `pulls`, `checks`, `releases` | required for github | What is read (below). |
| `ref` | branch, tag or sha | required for github checks | The commit whose check runs and statuses are read. |
| `state` | `open`, `closed`, `all` | `open` | github issues and pulls: which are listed. |
| `bucket` | S3 bucket | required for lake | Bucket to read. |
| `prefix` | object-key prefix | required for lake | Lake partition prefix; supports date templates such as `positions/date={{today}}/`. Objects are read in key order and must be gzip NDJSON files. |
| `region` | string | required for lake | S3 signing region, such as `garage`. |
| `access_key_id` | `${VAR}` | required for lake | Environment variable holding the read-only S3 access key ID. |
| `secret_access_key` | `${VAR}` | required for lake | Environment variable holding the S3 secret key. |
| `session_token` | `${VAR}` | none | Optional environment variable holding the S3 session token. |
| `max_objects` | int | `100` | lake: most objects listed and read, at most 1000; overflow fails instead of returning partial data. |
| `path` | path | required for file | A file on orbit's host, read as orbit's user (relative to orbit's working directory). |
| `max_bytes` | int | `8388608` (8 MiB) | The most one source may fetch; more fails the run. |
| `by_trigger` | map of trigger to override | none | http only: query values for runs of one trigger (`schedule`, `manual`, `message`, `continue`, `task`, `delegation`, `webhook`) over the source's own (below). |

### `sources[].extension`

| Field | Type | Default | What it does |
|---|---|---|---|
| `package` | pinned package id | required | Package id in `extensions.packages`. This source entry is the agent's grant. |
| `operation` | operation name | required | A package operation declared with kind `source`. |
| `parameters` | object | `{}` | Static JSON parameters validated against the pinned operation schema. |
| `inputs` | map of slot to source name | none | Binds every operation input slot to an earlier source in this agent. |

| Kind | Reaches | Risk |
|---|---|---|
| `http` | any URL with `GET`, with the headers given | Whatever the URL returns is model input, so it may carry instructions. |
| `prometheus` | a Prometheus API | As http. |
| `loki` | a Loki API, `query_range` only | As http; log lines can carry anything a service logged, instructions included. |
| `github` | GitHub as the App, one repository of `github.read_repositories`, with a token minted for the fetch alone and revoked after it: `issues` (`issues: read`; pull requests left out; each body cut at 4000 bytes, `body_truncated` set), `pulls` (`pull_requests: read`), `checks` (`checks: read`, `statuses: read`: `{ref, check_runs, statuses}`), `releases` (`contents: read`) | Issue, PR and release text is written by anyone who can open one on that repository. Needs `github` in `needs`. |
| `lake` | S3-compatible object store with signed ListObjectsV2 and GET requests only | Objects are untrusted input. The key must be read-only and its environment variables must be declared under `credentials:` with a `url_prefix` for this endpoint; for a private HTTP endpoint, keep the service on a trusted private network. The compressed objects, decompressed rows and final JSON are all bounded by `max_bytes`. |
| `file` | any file orbit's user can read | Exposes orbit's host files to the model: name only files meant for it. Only a file agent may have one. |
| `webhook` | the body of the delivery that started the run, as received; absent on any other run | The sender writes it: it is untrusted input, as http's is. Needs a webhook trigger. |
| `extension` | a derived JSON source produced by a pinned native package operation in sandboxd's network-disabled profile | Package code is untrusted. It receives only declared earlier source inputs, no credentials or network, and cannot write outside its invocation workdir. |

### `sources[].paginate`

| Field | Type | Default | What it does |
|---|---|---|---|
| `page_param` | string | required | Query parameter set to 1, 2, ... |
| `items_path` | dot path | required | Where each page's array is; pages are merged into one array. An empty page stops. |
| `total_pages_path` | dot path | none | Where the page count is; stops at it. |
| `max_pages` | int | `1000` | A guard: more pages fail the fetch. |

### `sources[].by_trigger.<trigger>`

What a run of that trigger fetches differently, for a window a scheduled review and a person's question need differently (a message run comparing months while the monthly review reads one).

| Field | Type | Default | What it does |
|---|---|---|---|
| `query` | map | required | Replaces the source's query values by key and adds keys it lacks; may use `${VAR}` and templates; may not set `paginate.page_param`. |

### `transforms[]`

| Field | Type | Default | What it does |
|---|---|---|---|
| `name` | `^[a-z][a-z0-9-]{0,62}$` | required | The output input's name (`inputs/<name>.json`); unique among sources and transforms. |
| `source` | source name | required | The source it reads. |
| `kind` | `json-filter`, `json-project`, `json-group-sum`, `sure-monthly` | required | The reducer (configuration.md, Transforms). |
| `params` | map | none | The kind's parameters, validated at load. |
| `keep_source` | bool | `false` | Show the source to the model as well; otherwise a source a transform reads is withheld. |

### `gates[]`

Checked in order on scheduled and webhook runs with no message waiting (configuration.md, Gates).

| Field | Type | Default | What it does |
|---|---|---|---|
| `kind` | `unchanged`, `items_above`, `precheck` | required | `unchanged` takes no other field. |
| `input` | source or transform name | required for items_above | What is counted. |
| `items` | dot path | the document | Where the records are. |
| `n` | int | `0` | items_above skips when there are at most `n` records. |
| `command` | shell | required for precheck | `sh -c` in the run's workdir; exit 1 skips, 0 runs, else fails. File agents only. |
| `timeout` | duration | `30s` | precheck bound, at most `2m`. |

### `tools`

Every claude-code run has `Read`, `Glob` and `Grep`, confined to the run's workdir; with orbit's harness, `mcp__orbit__query`, `mcp__orbit__calc` and `mcp__orbit__search` (the agent's own records); with a `prometheus` source, `mcp__orbit__prometheus_query` (read-only PromQL against that source, docs/api.md, Prometheus); with skills, `Skill`. `tools` adds Claude Code tool names, passed as given to `--tools` and `--allowedTools` (names are not checked at load; a spec with `tools` on a codex or anthropic runner fails validation, since neither has a per-tool grant). A listed tool is allowed by name with no path rule, and orbit's path guard covers only Read, Glob and Grep. The table is for runs on orbit's host; in a `sandbox` container the container is the boundary instead (a workstation has every tool and ignores `tools`). A database or built-in agent without a `sandbox` may list only `spec.UnsandboxedTools` (Read, Glob, Grep, WebSearch, WebFetch, Task, TodoWrite).

| Tool | Grants | Risk |
|---|---|---|
| `WebSearch` | web searches | Low reach; results are untrusted input. |
| `WebFetch` | `GET` of any URL | Network egress: a steered run can send what it read in a URL. Grant only where the inputs may leave. |
| `Bash` | any command as orbit's user, with the run's allowlisted environment | Reads and writes anywhere that user can and reaches the network; outside every read rule and the path guard. Against AGENTS.md rule 3: do not grant. |
| `Write`, `Edit`, `NotebookEdit` | writing files | Not confined to the workdir by orbit. Against AGENTS.md rule 3: do not grant. |
| others (`Task`, `TodoWrite`, ...) | the CLI tool of that name | Judge each by what it reaches; a new grant is a security change (security.md). |


### `web`

For an agent that runs on orbit's host (no `sandbox`; a sandboxed one's network is its egress profile). Needs `WebFetch` in `tools`.

| Field | Type | Default | What it does |
|---|---|---|---|
| `allow` | list of domains, at most 50 | required | The hosts WebFetch may fetch: `example.com`, or `*.example.com` for its subdomains. Each is a `WebFetch(domain:...)` allow rule in place of the bare tool, so any other host is denied (or asked, at a level that asks). Lowercase names only: an address, a URL or a private name (`localhost`, `*.local`, `*.internal`, `*.lan`, `*.home.arpa`, `*.ts.net`) fails the load. |

With `web`, the private names (`spec.PrivateWebNames`: those suffixes, loopback, `169.254.169.254`, `metadata.google.internal`) are also denied with `--disallowedTools`, which beats any allow, a person's included. Rules match the URL's host, not the address it resolves to (security.md, What the model's process can reach).

### `actions`

A map of action type to rule: `auto` (executed when the run finishes), `approve` (held for a person), `deny` (recorded, never executed; the default for any type not named). Only types not denied are offered to the model.

| Type | Does | Needs |
|---|---|---|
| `notify` | Sends a message to the agent's channels. | |
| `continue` | Wakes the agent again in 1m to 24h, at most 12 in a row. | |
| `delegate` | Hands a task to an agent in `delegates`. | `delegates` |
| `task:message`, `task:cancel`, `task:retry` | Messages, cancels or retries a delegated task; follow the `delegate` rule unless named. A message to a task a chat agent's run is working on goes into that run's turn (result `delivered: steered`), otherwise it waits for the task's next run (`delivered: queued`). A retry past a task's third failure waits for a person whatever the rule. | `delegates` |
| `webhook:<endpoint>` | Sends a body to an `orbit.yaml` endpoint, with `path` params filling its path's placeholders; never a URL, method or header of the model's. No redirect is followed. | the endpoint |
| `github:merge` | Squash-merges a PR once its checks are green; `kensac/orbit` merges always wait for a person. | `github` |
| `github:comment` | Posts a comment on an issue or a pull request as the App: `{repo, number, body}`, body at most 20000 bytes, attribution lines scrubbed. | `github.comment_repositories` |
| `gmail:draft` | Creates a plain text Gmail draft with recipients, subject and body; never sends it. | `google_actions.gmail_draft`, the connector in `connectors` |
| `calendar:create` | Creates one timed event with summary, start and end, and optional description, location and attendees. | `google_actions.calendar_create`, the connector in `connectors` |
| `agent:define` | Creates or changes database agents, itself included, as new revertible versions; a change that widens the agent waits for a person whatever the rule. | |
| `mcp:<connector>:<tool>` | The rule for one connector tool, overriding its default: `approve` for a tool in the connector's `write_tools`, `auto` for any other. A `deny` tool is not offered. An `approve` call is recorded as the action `connector:<connector>/<tool>` (configuration.md, Connectors). | the connector in `connectors` |
| `schedule:add`, `schedule:pause`, `schedule:resume`, `schedule:remove` | Changes one trigger (or, pausing and resuming, every trigger) of this agent or one in `delegates`, as a new revertible version; times are the person's words, parsed by code (configuration.md, Changing schedules by talking). `approve` applies directly in a run answering the person's message. | a database or built-in target |
| `agent:save` | Saves the conversation as a new agent: the model writes its name, description, charter and starters; orbit adds what the conversation did and proposes it as an `agent:define` that always waits for a person (configuration.md, Saving a conversation as an agent). | |
| `run:now` | Starts a manual run of this agent or one in `delegates`; a run no message woke cannot start its own agent. `approve` applies directly in a run answering the person's message. | |
| `skill:write` | Writes or retires one of the agent's skills as the next version; the approval shows the SKILL.md diff and the skill's scripts. | |
| `template:create` | Creates an agent from a template in the library: code renders and dry-runs it and records an `agent:define` that waits for a person whatever the rule; a `user_prompt` parameter is asked of the person with a form. | |

### `notify`

| Field | Type | Default | What it does |
|---|---|---|---|
| `channels` | list of channel names | `defaults.channels` | Where unattended runs report. |
| `when` | `always`, `agent`, `never` | `always` | `agent`: only when the run asks. Failures and pending approvals of unattended runs always notify; a run answering a message never does. |

### `repos[]`

| Field | Type | Default | What it does |
|---|---|---|---|
| `repo` | `owner/name` | required | One of `github.read_repositories`; its name (after the owner) is unique within the agent. |
| `ref` | branch, tag or commit sha | the default branch | What the run's tree is; resolved to a commit in orbit's mirror before each run. |

### `permissions`

Levels, narrowest first: `locked` (Claude Code's `dontAsk`: only what the spec allows by name runs, the rest is denied unasked), `ask` (`default`: the CLI's prompts go to the person), `edits` (`acceptEdits`: file edits in the workspace run unasked), `auto` (the CLI's classifier decides and asks the person when it cannot; `ask` where the model does not offer it), `full` (`bypassPermissions`). A level never adds a tool: `tools` and the sandbox profile decide which exist (configuration.md, Permissions).

| Field | Type | Default | What it does |
|---|---|---|---|
| `level` | level | `full` for a chat in a `sandbox`, else `locked` | What every run starts at. |
| `ceiling` | level | the default, or `level` when that is wider | The widest level a conversation may set; at least `level`. |

Only a `chat` agent may be above `locked`, and only one with a `sandbox` above `edits`. A raised level or ceiling widens an agent (`agent:define` waits for a person).

### `session`

claude-code runners only.

| Field | Type | Default | What it does |
|---|---|---|---|
| `resume` | bool | `false` | Continue the previous run's session when it succeeded under the same spec within `idle`. |
| `idle` | duration | `6h` | The resume window, at most `24h`. |
| `live` | duration | `0` (none) | Keep one process open this long after a message-started run and feed later runs into it; at most `24h` and at most `idle`. Linux memory pressure closes idle processes sooner. |
| `stall` | duration | `10m` | A chat or live turn with no line from the CLI (keep_alive included) for this long, and no tool call running, is interrupted and the run handed back to continue the session (ARCHITECTURE.md, Stall watchdog); `1m` to `24h`. Needs no `resume`. |

### `quiet_hours`

Uses the agent's `timezone`. Start is inclusive and end is exclusive. If end is earlier than start, the interval crosses midnight. Scheduled runs whose fire time or first claim time is within the interval are skipped before sources or a backend are called; manual runs and messages are unaffected. Times are wall-clock across daylight saving changes.

| Field | Type | Default | What it does |
|---|---|---|---|
| `start` | local time `HH:MM` | required | The first minute in the quiet interval. |
| `end` | local time `HH:MM` | required | The first minute outside the quiet interval; must differ from start. |

### `calendar_skip`

Uses the configured connector credential in orbit's process. Checked only on the first attempt of a scheduled run, after quiet hours, missed-fire and budget checks, before source fetches or a backend call. Manual, message, webhook and recovered attempts are unaffected. The connector must be named by the agent, and the selected tool must be read-only according to its MCP metadata and not listed in `write_tools`. A query failure or malformed response fails the run closed.

| Field | Type | Default | What it does |
|---|---|---|---|
| `connector` | connector name | required | Configured connector to query. |
| `tool` | tool name | required | Read-only event listing tool. |
| `window_before` | duration | `0` | Time before the scheduled fire included in the overlap window. |
| `window_after` | duration | required | Positive time after the scheduled fire; combined window may not exceed 24h. |
| `arguments` | JSON object | required | Fixed tool arguments containing both `{{window_start}}` and `{{window_end}}`; these are rendered as UTC RFC3339 values. |
| `events_path` | dotted JSON path | `items` | Array of event objects in the result. |
| `start_path` | dotted JSON path | `start` | Event start, as RFC3339 text or a Google Calendar `dateTime`/`date` object. |
| `end_path` | dotted JSON path | `end` | Event end, parsed the same way as the start. |

An event overlaps when its start is before the window end and its end is after the window start. Canceled and transparent events are ignored. Event data stays in orbit and is reduced to a skip decision; the skip summary contains only the window.

### `learn`

| Field | Type | Default | What it does |
|---|---|---|---|
| `enabled` | bool | `true` | `false` turns learning off. |
| `idle` | duration | `30m` | Quiet before a conversation is learned from, `5m` to `24h`. |
| `runner` | runner name | the agent's | The runner learning runs use (a cheaper model). |

### `hooks[]`

| Field | Type | Default | What it does |
|---|---|---|---|
| `event` | `PreToolUse`, `PostToolUse`, `UserPromptSubmit`, `SessionStart`, `Stop` | required | When the hook fires. `SessionStart` fires once per Claude Code process, so not for every run of a live one. |
| `matcher` | tool names joined by `\|` | every tool | Tool events only: which tools it fires for. |
| `say` | text, at most 2 KB | none | Tells the model the text as context; at `Stop`, sends it back once with it. |
| `deny` | text, at most 2 KB | none | `PreToolUse` only: refuses the tool with this reason. |
| `run` | argv | none | Runs the command (no shell) in the workdir with the event on stdin and only `PATH`, `HOME` and `LANG` set; stdout, stderr and the exit code go to Claude Code (exit 2 blocks). |
| `timeout` | duration | `10s` | `run` only, at most `1m`; the command's process group is killed at it. |

Exactly one of `say`, `deny` and `run`. Only a file agent may have a `run` hook.
