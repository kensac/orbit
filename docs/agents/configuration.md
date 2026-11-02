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
