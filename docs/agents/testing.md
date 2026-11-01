# Testing

- **Database tests run against real Postgres.** `make testdb-up` starts this
  checkout's own container; `internal/testdb` creates a fresh database per
  test. Without `ORBIT_TEST_DATABASE_URL` they skip locally and fail under CI.
- **Runners are tested with fake CLIs** (`runner_test.go`): a script that
  records its argv, environment and stdin. That is where "the model never
  sees orbit's secrets" is enforced. A fake live CLI answers one result per
  stdin line, and can crash, hang, or send a control request mid-turn.
- **The engine is tested end to end** with a fake runner and an httptest
  server standing in for sources, channels and endpoints.

## The real-CLI smoke

Fakes prove orbit's side of the contract, not the CLI's. After changing the
Claude Code invocation, the output schema, or bumping `deploy/cli`, run one
agent against the real CLI: a scratch config with a `file` source and an
`approve` action, `orbit serve` locally, `POST /api/v1/agents/<name>/runs`,
then check the run succeeded, memory was written, the action is pending, and
approving it executes it. A local run needs a credential in the environment
(`CLAUDE_CODE_OAUTH_TOKEN`); a macOS keychain login is not visible under the
per-run `HOME`, nor with `CLAUDE_CONFIG_DIR` set (the keychain entry's name
includes a hash of it). For a keychain login, point the runner's `binary` at a
wrapper that links `$HOME/.claude/projects` (orbit's HOME) to the real
`~/.claude/projects`, then sets `HOME`, `USER`, `LOGNAME` back, unsets
`CLAUDE_CONFIG_DIR` and `CLAUDE_CODE_OAUTH_TOKEN`, and execs `claude`; give
orbit any placeholder token so the runner builds.

After changing sessions or the live process, also run an agent with
`session: {resume: true, live: 10m}` and `continue: approve`: a message (run
fresh), a second message (`session_mode` live, one `claude` process), a
restart of orbit (the process closes), a manual run that must answer from the
session alone (`resumed`, a new `session_id`), an approved `continue` that
runs at its `run_after`, and a `kill -9` of the live process mid-turn (that
run fails with "claude exited during the turn"; the next starts fresh), with a
Read outside the workdir denied in the live turn.
