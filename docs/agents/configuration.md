# Configuration

A deployment is a directory: `orbit.yaml` plus `agents/<name>.yaml`
(`examples/` is complete and loaded by tests). Unknown fields fail the load;
so does any invalid cross-reference. `orbit validate <dir>` checks a
directory and lists the environment variables it references.

## Secrets

Never in YAML. Channels name variables with `*_env`; source headers, query
values and endpoint headers use `${VAR}`. An unset variable is an error, never
an empty string.

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

## Changing a running deployment

orbit polls its config directory and reloads on any content change. A change
that fails to load is refused and the previous configuration stays in force:
check `orbit_config_valid` and the log after a deploy.
