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

An agent changes agents with the `agent:define` action (`internal/engine/define.go`, the API's own path): enable it per agent (`actions: {agent:define: auto}`, as the built-in `orbit` has it). Each change records the run and the person it answered; `POST /agents/{name}/definitions/{version}/revert` undoes one. A change that widens the agent (`spec.Widening`: a tool, source, credential, delegate target, need or webhook trigger it adds, a source it changes, a move to a sandbox profile, an action type allowed more readily) waits for a person's approval whatever the rule.

Database and built-in agents are held to what orbit.yaml offers (`spec.validateTrust`): no `file` source, no `${VAR}` that is not in `credentials:`, without a `sandbox`, only the tools in `spec.UnsandboxedTools` (Read, Glob, Grep, WebSearch, WebFetch, Task, TodoWrite), no precheck and no `run` hook. A spec breaking one fails validation, naming it.

## Secrets

Never in YAML. Channels name variables with `*_env`; source urls, headers, query
values, Prometheus expressions and endpoint headers use `${VAR}`. An unset variable is an error, never
an empty string.

`credentials:` in `orbit.yaml` names the variables sources may send, each bound to the URLs it may go to:

```yaml
credentials:
  SURE_API_KEY:
    url_prefix: https://sure.example.com/api/
```

`url_prefix` is an http(s) URL with a host and a path ending in `/`, without user, query or fragment. A source sends a credential only in a request whose URL starts with it (redirects included); where the URL has no template, a mismatch fails the load. A database or built-in agent may reference only credentials; a file agent may also name any other variable (docs/agents/security.md, What a model can define).

## GitHub

`github:` in `orbit.yaml` names the App (`app_id`), the variable holding its
private key (`private_key_env`: PEM, or base64 of the PEM for a one-line env
file), and the `repositories` tickets and `github:merge` may target. Without
it, a request ticket or a `github:merge` rule fails validation.

`read_repositories` (optional) lists the repositories agents may read (`repos`, below). It is separate from `repositories`: reading a repository never permits a ticket or a merge there, and a repository in `repositories` is not readable unless it is listed here too.

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

## Capabilities

`needs: [<capability>, ...]` on an agent says what a host must have to run it; a host advertises its own in `ORBIT_CAPABILITIES` (comma-separated; `*` alone is every agent, control's default when unset; a worker's default is none). A capability is `name` or `name:qualifier`, lower case (`spec.CapabilityRE`). An agent with no needs runs on any host. Give an agent a need for anything only some hosts have: a network its sources are on (`docker-network:hetzner`), a source credential that lives only on some hosts (`sure`), the GitHub App's key (`github`, required with `repos`), a tool (`browser`). The table of capabilities and the hosts that have them is docs/agents/deployment.md, Worker fleet. A run whose agent needs what no host has waits queued; nothing fails it.

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

## Templates in sources

URLs and query values may use `{{today}}`, `{{now}}`, `{{month_start}}`,
`{{prev_month_start}}`, `{{prev_month_end}}`, `{{days_ago_N}}`, in the agent's
timezone. An unknown `{{name}}` fails the fetch.

## Adding a kind

Each kind is listed once in `internal/spec` (`channelKinds`, `sourceKinds`,
`runnerKinds`) and implemented once in its package. Add both, plus the
validation of its required fields, plus a test, plus the table in
docs/ARCHITECTURE.md and the kind's row in spec-reference.md. A kind validation accepts that no package implements
fails at runtime, not at load.

## Chat agents

`chat: true` (claude-code runners only) makes an agent a Claude Code session whose reply is its final message, with memory, questions and actions as orbit's tools (docs/ARCHITECTURE.md, Chat agents); each conversation gets its own workspace. `sandbox: workstation` (needs `chat`) runs it in a sandboxd container with Claude Code's full tool set, a kept home and egress to any public host; `sandbox: build` with the tools in its `tools` and the build allowlist; sandboxd is `sandbox: {sandboxd: <host:port | unix:/path>, token_env: ORBIT_SANDBOX_TOKEN}` in orbit.yaml, or the `build:` section's when there is no `sandbox:`. The CLI there needs `CLAUDE_CODE_OAUTH_TOKEN`. Give a sandboxed agent `needs: [sandbox]` so only hosts that reach sandboxd claim it (the built-in has it).

## Sessions and continuations

`session` (claude-code runners only) is `{resume: true, idle: <=24h, live:
<=1h}`; `idle` defaults to 6h, `live` to 0 (no live process) and may not exceed
`idle`. `live` is refused with a runner's `max_budget_usd`, because the CLI
counts that budget over a process's whole life, every run fed into it. An
agent with `session` needs `ORBIT_DATA_DIR` on a persistent volume; without
one every run after a restart starts fresh (it does not fail).

`continue` is an action type like any other: absent means denied. Its bounds
are constants in `internal/actions` (`ContinueMinDelay`, `ContinueMaxDelay`,
`MaxContinueChain`).

## Delegation

`delegates: [<agent>, ...]` (or `["*"]`, every configured agent, itself included) names the agents an agent may hand tasks to; an unknown name fails the load. The `delegate` action needs it, and is denied unless named, like any action; `task:message` and `task:cancel` follow the `delegate` rule unless named themselves. In a run answering the person's message, `approve` on these applies directly (docs/ARCHITECTURE.md, Delegation). Bounds are constants in `internal/store` (`MaxTasksPerDelegation`, `MaxTaskDepth`, `MaxConversationTasks`).

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
the chat. Failures and pending approvals of unattended runs always notify, except a failure that holds its runner (the backend refused orbit's login, or the subscription window is spent): that sends one notification for the runner, and the runner's unattended runs wait until it answers again (docs/ARCHITECTURE.md, Reliability).

## Memory topics

`topics: [finance]` names the shared memory topics whose entries every run of the agent reads, beside the profile's and its own (docs/ARCHITECTURE.md, Memory entries). A topic exists as soon as an entry or an agent names it; the API can attach more (`PUT /agents/{name}/topics/{topic}`).

## Hooks

`hooks` adds Claude Code hooks to an agent's runs (claude-code runners only; at most 20), beside orbit's own path guard and figure check. Each names an `event` (`PreToolUse`, `PostToolUse`, `UserPromptSubmit`, `SessionStart`, `Stop`), for a tool event optionally a `matcher` (tool names joined by `|`, as Claude Code reads them), and exactly one of:

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

`skills: [monthly-close]` names skills from orbit's library the agent runs with (docs/ARCHITECTURE.md, Skills). A name no skill has is skipped at run time and shown as `missing` in `GET /agents/{name}/context`; the API attaches more.

## Learning

Every agent is learned from (docs/ARCHITECTURE.md, Memory entries): a conversation quiet for `learn.idle` (default 30m, between 5m and 24h) with something new from the person gets one learning run. `learn: {enabled: false}` turns it off; `learn.runner` names a runner for learning runs instead of the agent's own (a cheaper model).

## Changing a running deployment

orbit polls its config directory and reloads on any content change. A change
that fails to load is refused and the previous configuration stays in force:
check `orbit_config_valid` and the log after a deploy.
