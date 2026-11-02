# Configuration

A deployment is a directory: `orbit.yaml` plus `agents/<name>.yaml`
(`examples/` is complete and loaded by tests). Unknown fields fail the load;
so does any invalid cross-reference. `orbit validate <dir>` checks a
directory and lists the environment variables it references.

## Agent names

An agent's name is its file's (`^[a-z][a-z0-9-]{0,62}$`), and may not be
one of `spec.ReservedAgentNames`: the UI's own top-level routes under
`/orbit/` (`admin`, `api`, `c`, `new`, `components`, `lib`) and
`orbit-admin`. Such an agent fails the load, naming the reason.

## Secrets

Never in YAML. Channels name variables with `*_env`; source headers, query
values and endpoint headers use `${VAR}`. An unset variable is an error, never
an empty string.

## GitHub

`github:` in `orbit.yaml` names the App (`app_id`), the variable holding its
private key (`private_key_env`: PEM, or base64 of the PEM for a one-line env
file), and the `repositories` tickets and `github:merge` may target. Without
it, a request ticket or a `github:merge` rule fails validation.

## Build

`build:` in `orbit.yaml` turns approved tickets into PRs: `sandboxd` (the
service's tailnet `host:port` or `unix:/path`), `token_env` (default
`ORBIT_SANDBOX_TOKEN`), `agent` (whose claude-code runner, timeout and
`github:merge` rule a build uses; timeout at most `spec.MaxBuildTimeout`),
`author` (`"Name <email>"`, the App's bot user), and per repository the
`commands` a build may run (argv prefixes of plain words), `protected`
globs and `max_diff_lines`. Each repository must be in
`github.repositories`, and today must be `kensac/orbit` (docs/tickets.md,
Open decisions). Without the section no ticket is built; approved tickets
wait. orbit also needs `CLAUDE_CODE_OAUTH_TOKEN`, the one credential the
sandbox receives. `orbit validate` lists both variables.

## Capabilities

`needs: [<capability>, ...]` on an agent says what a host must have to run it; a host advertises its own in `ORBIT_CAPABILITIES` (comma-separated; `*` alone is every agent, control's default when unset; a worker's default is none). A capability is `name` or `name:qualifier`, lower case (`spec.CapabilityRE`). An agent with no needs runs on any host. Give an agent a need for anything only some hosts have: a network its sources are on (`docker-network:hetzner`), a source credential that lives only on some hosts (`sure`), a tool (`browser`). The table of capabilities and the hosts that have them is docs/agents/deployment.md, Worker fleet. A run whose agent needs what no host has waits queued; nothing fails it.

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
as a paginated `http` source is); `where` is the query tool's conditions
(`field`, `op`, `value`). Sums are exact decimals. A transform that fails
fails the run, naming it. A new kind is a function in
`internal/transforms`, its parameters in `parse`, and a test.

## Gates

`gates:` in an agent are checks, in code, that end a run as `skipped` (naming the gate in `runs.gate`) before any backend call, so a quiet day costs no tokens and sends nothing. They apply only to scheduled runs with no person's message waiting: a message, a manual run, a continuation, a build and a recovered attempt always reach the model. They are checked in order, after sources and transforms:

| kind | fields | skips when |
|---|---|---|
| `unchanged` | none | every input the model would see (by file name and sha256) is what the agent's newest succeeded run saw, under the same spec |
| `items_above` | `input` (a source or transform), `items`, `n` | `input` has at most `n` records at `items` |

A skipped run counts as a success for staleness (docs/agents/store.md).

## Templates in sources

URLs and query values may use `{{today}}`, `{{now}}`, `{{month_start}}`,
`{{prev_month_start}}`, `{{prev_month_end}}`, `{{days_ago_N}}`, in the agent's
timezone. An unknown `{{name}}` fails the fetch.

## Adding a kind

Each kind is listed once in `internal/spec` (`channelKinds`, `sourceKinds`,
`runnerKinds`) and implemented once in its package. Add both, plus the
validation of its required fields, plus a test, plus the table in
docs/ARCHITECTURE.md. A kind validation accepts that no package implements
fails at runtime, not at load.

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
the chat. Failures and pending approvals of unattended runs always notify.

## Changing a running deployment

orbit polls its config directory and reloads on any content change. A change
that fails to load is refused and the previous configuration stays in force:
check `orbit_config_valid` and the log after a deploy.
