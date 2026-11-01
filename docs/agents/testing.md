# Testing

- **Database tests run against real Postgres.** `make testdb-up` starts this
  checkout's own container; `internal/testdb` creates a fresh database per
  test. Without `ORBIT_TEST_DATABASE_URL` they skip locally and fail under CI.
- **Runners are tested with fake CLIs** (`runner_test.go`): a script that
  records its argv, environment and stdin. That is where "the model never
  sees orbit's secrets" is enforced.
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
per-run `HOME`.
