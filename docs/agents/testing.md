# Testing

- **Database tests run against real Postgres.** `make testdb-up` starts this
  checkout's own container; `internal/testdb` creates a fresh database per
  test. Without `ORBIT_TEST_DATABASE_URL` they skip locally and fail under CI.
  `make test` uses `ORBIT_TEST_DATABASE_URL` from the environment when set (CI sets it, so an unreachable server fails the run), else this checkout's container when it is running, else none, and says so. `make check-sandbox` always skips them and drops `-race` (it needs cgo), for the build sandbox; a green sandbox run is not a green `make check`.
- **A migration test writes its old-schema rows in SQL, never with today's store code.** It runs `testdb.Empty`, `MigrateTo` the version before the migration, inserts rows as that schema had them (`oldAgent`, `oldRun` in `internal/store`), then `Migrate` and asserts the result. Today's queries read columns later migrations add, so a test that calls them at an old version breaks on the next unrelated migration.
- **Runners are tested with fake CLIs** (`runner_test.go`): a script that
  records its argv, environment and stdin. That is where "the model never
  sees orbit's secrets" is enforced. A fake live CLI answers one result per
  stdin line, and can crash, hang, or send a control request mid-turn.
- **The engine is tested end to end** with a fake runner and an httptest
  server standing in for sources, channels and endpoints.
- **sandboxd's Docker integration tests** use a real daemon when available and
  skip locally when it is not. Under CI, missing Docker fails the test instead
  of skipping it. `TestIntegrationIsolation` exercises both the build profile
  and the native extension profile; extension checks include filesystem
  boundaries, absent network and credentials, process capabilities, inspected
  resource limits, host/session path visibility, read-only-root symlink writes,
  cross-mount hardlinks, process flooding, memory exhaustion, per-invocation scratch,
  timeout cleanup and container removal. This does not cover every isolation acceptance case in
  [plugins.md](../plugins.md), so it is not by itself evidence that extensions
  are ready to enable.
- **The CLI protocol has its own real-CLI check**: `ORBIT_REAL_CLAUDE=claude go test -run RealCLI ./internal/claudeproto/` (initialize with a host hook, set_permission_mode, can_use_tool allowed and denied, a hook_callback per call, an interrupt that withdraws a pending permission request; and `TestRealCLISteer`: a message written during a tool call folded into that turn, one written after the last tool call starting its own turn after the result, `cancel_async_message` withdrawing a queued one, an interrupt with `cancel_queued` listing it in its receipt). Run it after a CLI bump; it uses the person's own login, so it never runs in CI. The same command runs `TestRealCLIPermissionModes` for mode changes, launch flags, edits, and prompts.

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
`mcp__orbit__query`, `mcp__orbit__calc` and `mcp__orbit__search` (and `mcp__orbit__prometheus_query` for an agent with a `prometheus` source), a search for a word of an earlier run's report finds it, the totals come from query calls,
a Read of `../x` is denied with orbit's message, and a figure asked for "in
your head" is sent back and recomputed with a tool.

After changing the hook events or `internal/status`, watch `runs.hook_status` during that run: it reads `working` from UserPromptSubmit and `done` at Stop, the run's events hold an `orbit.hook` per hook event (SessionStart, UserPromptSubmit, a PreToolUse and a PostToolUse per allowed tool, Stop), and a second message to a live process records only its own turn's events (a new `prompt_id`, no SessionStart). The SessionEnd a live process sends when it closes lands in the spool, read by no run.

After changing delegation, run one agent with `delegates` naming two others and `actions: {delegate: approve}` against the real CLI: message it asking for two independent pieces of work. Check the delegate action was approved by the attended rule, both task runs ran at once (different agents), each task is `done` with the run's report, one `delegation` run woke the parent, and its report integrates both.

After changing skills or their wiring, run an agent with one skill whose `SKILL.md` tells it to read a file of the skill: the run's events show the `Skill` tool on `orbit:<name>`, then `mcp__orbit__skill` for `SKILL.md` and for the file, and the answer uses the file's content (seen on 2.1.316). After changing references or the built-in skills, also give it a `references/` file and ask for something only it holds: the `SKILL.md` read lists it by name and heading, and a `reference` call loads it; and run an agent with `verify: [figures]` that loads `orbit-query` before its first query.

After changing sessions or the live process, also run an agent with
`session: {resume: true, live: 10m}` and `continue: approve`: a message (run
fresh), a second message (`session_mode` live, one `claude` process), a
restart of orbit (the process closes), a manual run that must answer from the
session alone (`resumed`, a new `session_id`), an approved `continue` that
runs at its `run_after`, and a `kill -9` of the live process mid-turn (that
run fails with "claude exited during the turn"; the next starts fresh), with a
Read outside the workdir denied in the live turn.

The live-process unit tests also pin idle eviction under simulated memory
pressure, the maximum 24-hour window, and the hard expiry used by a sandbox
credential grant. The engine reload test changes deployment configuration
without changing a chat invocation and verifies that the same process and box
serve the next message.

After changing chat runs (`runner/chat.go`), run `ORBIT_REAL_CLAUDE=<wrapper> go test -run RealCLIChat ./internal/runner/`: orbit's SDK MCP tool is listed and called over the control channel, the reply is the final message, the second turn is `live` in the same process, and an interrupt ends the third turn with a result; `RealCLIChatPermissionLevels`: one live process at locked denies a Read outside the workspace unasked, then takes the next run at ask after a `set_permission_mode` and sends that Read to orbit's prompt (seen on 2.1.316); `RealCLIChatPlanApproval` (sonnet by default): the session's `ExitPlanMode` comes to orbit's prompt, and after the allow the CLI is back in the mode it planned from (plan, then acceptEdits on 2.1.316). The sandboxed path is covered by `TestASandboxedChatKeepsItsWorkspace` against a fake sandboxd that runs the argv locally; a real sandbox is seen only on hetzner.

After changing connectors' wiring (`runner/connectors.go`, the permission prompt, the transport), run `ORBIT_REAL_CLAUDE=<wrapper> go test -run RealCLIConnectors ./internal/runner/`: a connector with a dash in its name is listed and called as an SDK server, an auto tool runs unasked, and an approve tool reaches orbit's prompt with the tool use id its call then carries, in a report run and in a chat run.

After changing chat runs (`runner/chat.go`), run `ORBIT_REAL_CLAUDE=<wrapper> go test -run RealCLIChat ./internal/runner/`: orbit's SDK MCP tool is listed and called over the control channel, the reply is the final message, the second turn is `live` in the same process, and an interrupt ends the third turn with a result. `RealCLIChatPermissionLevels` checks one live process at locked and then ask, with `set_permission_mode`; `TestRealCLIChatSteer` checks a steer message written during the turn's tool call is started there (`Steered`) and the one reply answers both. Steering end to end (the database poll, `steered_into`, the `steer` item, withdrawal confirmed and not, a stop withdrawing a queued steer, the run woken for what the turn did not take) is `engine.TestSteeringARunningReply`, against a scripted fake CLI that speaks the protocol `claudeproto.TestRealCLISteer` pins. `RealCLIWebAllow`: a chat with `web.allow` fetches an allowed domain and a `*.` subdomain, and is denied another domain and `localhost`. The sandboxed path is covered by `TestASandboxedChatKeepsItsWorkspace` against a fake sandboxd that runs the argv locally; a real sandbox is seen only on hetzner.
