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
- **The CLI protocol has its own real-CLI check**: `ORBIT_REAL_CLAUDE=claude go test -run RealCLI ./internal/claudeproto/` (initialize with a host hook, set_permission_mode, can_use_tool allowed and denied, a hook_callback per call, an interrupt that withdraws a pending permission request). Run it after a CLI bump; it uses the person's own login, so it never runs in CI.

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

After changing `internal/harness` or its wiring, run an agent with
`verify: [figures]` and a JSON `file` source: the init event lists
`mcp__orbit__query`, `mcp__orbit__calc` and `mcp__orbit__search`, a search for a word of an earlier run's report finds it, the totals come from query calls,
a Read of `../x` is denied with orbit's message, and a figure asked for "in
your head" is sent back and recomputed with a tool.

After changing the hook events or `internal/status`, watch `runs.hook_status` during that run: it reads `working` from UserPromptSubmit and `done` at Stop, the run's events hold an `orbit.hook` per hook event (SessionStart, UserPromptSubmit, a PreToolUse and a PostToolUse per allowed tool, Stop), and a second message to a live process records only its own turn's events (a new `prompt_id`, no SessionStart). The SessionEnd a live process sends when it closes lands in the spool, read by no run.

After changing delegation, run one agent with `delegates` naming two others and `actions: {delegate: approve}` against the real CLI: message it asking for two independent pieces of work. Check the delegate action was approved by the attended rule, both task runs ran at once (different agents), each task is `done` with the run's report, one `delegation` run woke the parent, and its report integrates both.

After changing skills or their wiring, run an agent with one skill whose `SKILL.md` tells it to read a file of the skill: the run's events show the `Skill` tool on `orbit:<name>`, then `mcp__orbit__skill` for `SKILL.md` and for the file, and the answer uses the file's content (seen on 2.1.316).

After changing sessions or the live process, also run an agent with
`session: {resume: true, live: 10m}` and `continue: approve`: a message (run
fresh), a second message (`session_mode` live, one `claude` process), a
restart of orbit (the process closes), a manual run that must answer from the
session alone (`resumed`, a new `session_id`), an approved `continue` that
runs at its `run_after`, and a `kill -9` of the live process mid-turn (that
run fails with "claude exited during the turn"; the next starts fresh), with a
Read outside the workdir denied in the live turn.
