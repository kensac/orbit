# Spec field reference

Every field of `orbit.yaml` and `agents/<name>.yaml`, from `internal/spec` (`spec.go`, `build.go`, `mail.go`). An agent defined through the API is the same fields as JSON. Unknown fields fail the load; durations are Go strings (`"15m"`); `${VAR}` and `*_env` name environment variables, and an unset one is an error, never an empty string. The rules behind the fields are in [configuration.md](configuration.md). An agent defined in the database is limited further than a file agent: see [security.md](security.md). `TestReferenceCoversSpec` fails when a struct's yaml fields and its table here differ.

## orbit.yaml

| Field | Type | Default | What it does |
|---|---|---|---|
| `timezone` | IANA zone | `UTC` | The zone schedules and source templates are read in, unless an agent names its own. |
| `public_url` | URL | none | Where a person reaches the API; links in notifications and search results. |
| `defaults` | map | | Values every agent gets unless it sets its own (below). |
| `channels` | map of name to channel | none | Notification destinations an agent's `notify.channels` names. |
| `endpoints` | map of name to endpoint | none | HTTP targets the `webhook:<name>` action may call. |
| `runners` | map of name to runner | none | Model backends an agent's `runner` names. |
| `github` | map | absent | The GitHub App orbit acts as. Without it, tickets naming a repository and `github:merge` fail validation. |
| `build` | map | absent | How approved tickets become PRs. Without it no ticket is built. |
| `sandbox` | map | `build`'s sandboxd | The sandboxd sandboxed chat agents use (below). |
| `mail` | map | absent | orbit's mailbox. Without it orbit reads no mail and its mails carry no reply address. |
| `credentials` | map of variable to credential | none | The variables sources may send, each only to URLs under its prefix (below); the only `${VAR}`s a database or built-in agent may reference. |
| `board` | map | absent | Tunes `GET /board` (below). |

### `defaults`

| Field | Type | Default | What it does |
|---|---|---|---|
| `runner` | runner name | none | An agent's runner when it names none. An agent with neither fails the load. |
| `timeout` | duration | `15m` | An agent's run timeout when it names none. |
| `channels` | list of channel names | none | An agent's `notify.channels` when it names none. Each must exist. |
| `memory_max_bytes` | int | `16384` | The largest memory document a run or the API may write. |

### `channels.<name>`

Names match `^[a-z][a-z0-9-]{0,62}$`.

| Field | Type | Default | What it does |
|---|---|---|---|
| `kind` | `ntfy`, `smtp`, `webhook`, `log` | required | The adapter. `log` writes to orbit's log and takes no other field. |
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
| `headers` | map | none | Webhook channel headers; values may use `${VAR}`. |

### `endpoints.<name>`

| Field | Type | Default | What it does |
|---|---|---|---|
| `url` | URL | required | Where the action sends its body. The model never chooses it. |
| `method` | `POST`, `PUT`, `PATCH` | `POST` | HTTP method. |
| `headers` | map | none | Request headers; values may use `${VAR}`. |

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

### `credentials.<VAR>`

| Field | Type | Default | What it does |
|---|---|---|---|
| `url_prefix` | http(s) URL, path ending in `/` | required | Every request the variable is rendered into, redirects included, must start with it; no user, query or fragment (configuration.md, Secrets). |

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
| `enabled` | bool | `true` | `false` stops its triggers firing; manual runs still work. A database agent's comes from its definition, not its spec. |
| `timezone` | IANA zone | `orbit.yaml` timezone | Zone for triggers and source templates. |
| `runner` | runner name | `defaults.runner` | The backend. |
| `timeout` | duration | `defaults.timeout` | How long one run may take. |
| `triggers` | list | none | When the agent wakes on its own (below). |
| `sources` | list | none | Data orbit fetches before a run (below). |
| `transforms` | list | none | Code over a source before the model sees it (below). |
| `gates` | list | none | Checks that skip a scheduled run before any backend call (below). |
| `tools` | list of tool names | none | Extra Claude Code tools (below). |
| `actions` | map of type to rule | every type `deny` | The writes the agent may propose and what happens to each (below). |
| `notify` | map | | Where unattended runs report (below). |
| `wake_on_message` | bool | `false` | A person's message starts a run (the API's `wake` overrides it per message). |
| `session` | map | absent | Keep the Claude Code session between runs (below). |
| `verify` | list: `figures` | none | Every money amount in the summary, title and report must trace to an input or a query or calc result. |
| `topics` | list of names | none | Shared memory topics every run reads, beside the profile's and its own. |
| `skills` | list of names | none | Skills from orbit's library the agent runs with; an unknown one is skipped and shown as `missing`. |
| `learn` | map | on | Learning runs over quiet conversations (below). |
| `hooks` | list | none | Claude Code hooks for the agent's runs, beside orbit's own; claude-code only, at most 20 (below). |
| `chat` | bool | `false` | A Claude Code session whose reply is its final message, with memory, questions and actions as orbit's tools; a workspace per conversation. claude-code only. |
| `sandbox` | `workstation`, `build` | none | Needs `chat` and a sandboxd. Runs the CLI in a sandboxd container per conversation: `workstation` has every Claude Code tool and egress to any public host; `build` has its `tools` and the build allowlist (security.md, Sandboxed chats). |
| `needs` | list of capabilities | none | What a host must have to run the agent (`docker-network:hetzner`); without any, every host. |
| `delegates` | list of agent names, or `["*"]` | none | Agents this one may hand tasks to; the `delegate` action also needs a rule. |
| `repos` | list (below) | none | Repositories every run reads: the tree at a commit under `inputs/repos/<name>/`, plus orbit's history and pull request tools. Each must be in `github.read_repositories`; needs `github` in `needs` and a claude-code or codex runner. |

### `triggers[]`

| Field | Type | Default | What it does |
|---|---|---|---|
| `schedule` | cron (five fields) or descriptor | | When to fire, in the agent's timezone. Exactly one of `schedule` or `rrule`. |
| `rrule` | RFC 5545 rule | | A recurrence cron cannot say (configuration.md, Triggers). |
| `start` | local time `2026-11-03T09:00` | required with `rrule` | The rule's first period and its time of day; not allowed with `schedule`. |
| `prompt` | string | none | The reason for this wake, appended to the charter. |

### `sources[]`

Fetched by orbit with orbit's credentials; the model sees only the data, as `inputs/<name>.<ext>`.

| Field | Type | Default | What it does |
|---|---|---|---|
| `name` | `^[a-z][a-z0-9-]{0,62}$` | required | The input's name; unique among sources and transforms. |
| `kind` | `http`, `prometheus`, `file` | required | How it is fetched (below). |
| `url` | URL | required for http, prometheus | Target; may use `${VAR}` and `{{today}}`-style templates. A credential in any field of the source is sent only under its `url_prefix`. |
| `headers` | map | none | http request headers; values may use `${VAR}`. |
| `query` | map | none | http query values; may use `${VAR}` and templates. |
| `paginate` | map | absent | Page-numbered http pagination (below). |
| `expr` | PromQL | required for prometheus | Instant query sent to `<url>/api/v1/query`; the input is its `data`. |
| `path` | path | required for file | A file on orbit's host, read as orbit's user (relative to orbit's working directory). |
| `max_bytes` | int | `8388608` (8 MiB) | The most one source may fetch; more fails the run. |

| Kind | Reaches | Risk |
|---|---|---|
| `http` | any URL with `GET`, with the headers given | Whatever the URL returns is model input, so it may carry instructions. |
| `prometheus` | a Prometheus API | As http. |
| `file` | any file orbit's user can read | Exposes orbit's host files to the model: name only files meant for it. Only a file agent may have one. |

### `sources[].paginate`

| Field | Type | Default | What it does |
|---|---|---|---|
| `page_param` | string | required | Query parameter set to 1, 2, ... |
| `items_path` | dot path | required | Where each page's array is; pages are merged into one array. An empty page stops. |
| `total_pages_path` | dot path | none | Where the page count is; stops at it. |
| `max_pages` | int | `1000` | A guard: more pages fail the fetch. |

### `transforms[]`

| Field | Type | Default | What it does |
|---|---|---|---|
| `name` | `^[a-z][a-z0-9-]{0,62}$` | required | The output input's name (`inputs/<name>.json`); unique among sources and transforms. |
| `source` | source name | required | The source it reads. |
| `kind` | `json-filter`, `json-project`, `json-group-sum`, `sure-monthly` | required | The reducer (configuration.md, Transforms). |
| `params` | map | none | The kind's parameters, validated at load. |
| `keep_source` | bool | `false` | Show the source to the model as well; otherwise a source a transform reads is withheld. |

### `gates[]`

Checked in order on scheduled runs with no message waiting (configuration.md, Gates).

| Field | Type | Default | What it does |
|---|---|---|---|
| `kind` | `unchanged`, `items_above`, `precheck` | required | `unchanged` takes no other field. |
| `input` | source or transform name | required for items_above | What is counted. |
| `items` | dot path | the document | Where the records are. |
| `n` | int | `0` | items_above skips when there are at most `n` records. |
| `command` | shell | required for precheck | `sh -c` in the run's workdir; exit 1 skips, 0 runs, else fails. File agents only. |
| `timeout` | duration | `30s` | precheck bound, at most `2m`. |

### `tools`

Every claude-code run has `Read`, `Glob` and `Grep`, confined to the run's workdir; with orbit's harness, `mcp__orbit__query`, `mcp__orbit__calc` and `mcp__orbit__search` (the agent's own records); with skills, `Skill`. `tools` adds Claude Code tool names, passed as given to `--tools` and `--allowedTools` (not checked at load; codex and anthropic runners fail the run if it is set). A listed tool is allowed by name with no path rule, and orbit's path guard covers only Read, Glob and Grep. The table is for runs on orbit's host; in a `sandbox` container the container is the boundary instead (a workstation has every tool and ignores `tools`). A database or built-in agent without a `sandbox` may list only `spec.UnsandboxedTools` (Read, Glob, Grep, WebSearch, WebFetch, Task, TodoWrite).

| Tool | Grants | Risk |
|---|---|---|
| `WebSearch` | web searches | Low reach; results are untrusted input. |
| `WebFetch` | `GET` of any URL | Network egress: a steered run can send what it read in a URL. Grant only where the inputs may leave. |
| `Bash` | any command as orbit's user, with the run's allowlisted environment | Reads and writes anywhere that user can and reaches the network; outside every read rule and the path guard. Against AGENTS.md rule 3: do not grant. |
| `Write`, `Edit`, `NotebookEdit` | writing files | Not confined to the workdir by orbit. Against AGENTS.md rule 3: do not grant. |
| others (`Task`, `TodoWrite`, ...) | the CLI tool of that name | Judge each by what it reaches; a new grant is a security change (security.md). |

### `actions`

A map of action type to rule: `auto` (executed when the run finishes), `approve` (held for a person), `deny` (recorded, never executed; the default for any type not named). Only types not denied are offered to the model.

| Type | Does | Needs |
|---|---|---|
| `notify` | Sends a message to the agent's channels. | |
| `continue` | Wakes the agent again in 1m to 24h, at most 12 in a row. | |
| `delegate` | Hands a task to an agent in `delegates`. | `delegates` |
| `task:message`, `task:cancel` | Messages or cancels a delegated task; follow the `delegate` rule unless named. | `delegates` |
| `webhook:<endpoint>` | Sends a body to an `orbit.yaml` endpoint; never a URL or header of the model's. | the endpoint |
| `github:merge` | Squash-merges a PR once its checks are green; `kensac/orbit` merges always wait for a person. | `github` |
| `agent:define` | Creates or changes database agents, itself included, as new revertible versions; a change that widens the agent waits for a person whatever the rule. | |

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

### `session`

claude-code runners only.

| Field | Type | Default | What it does |
|---|---|---|---|
| `resume` | bool | `false` | Continue the previous run's session when it succeeded under the same spec within `idle`. |
| `idle` | duration | `6h` | The resume window, at most `24h`. |
| `live` | duration | `0` (none) | Keep one process open this long after a message-started run and feed later runs into it; at most `1h` and at most `idle`. |

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
