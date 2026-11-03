# Architecture

orbit runs standing agents: each has a charter (what it is responsible for),
triggers (when it wakes), sources (what it reads), memory (what it keeps
between runs), conversations (what its person tells it), and rules (what it may do
alone, what waits for approval, what it may not do at all).

## Code first, model last

The model is the expensive, slow, non-deterministic part, so it gets only the part of a job that needs judgment. Everything else is code that runs before it: the harness fetches and paginates sources, and runs an agent's transforms (`internal/transforms`: coded reducers over a source, such as per-category totals with transfers removed, each output an input of its own), and then its gates (`unchanged`, `items_above`, `precheck`: a scheduled run whose inputs are what the last review judged, that has nothing to review, or whose precheck command says there is nothing to do, ends `skipped`, naming the gate, without a backend call, so a quiet day costs no tokens). A report's numbers should come from code, with the model explaining them.

## One run

```
trigger (schedule: cron or RRULE | webhook | manual | message | continue | task | delegation; a ticket's build runs its own path, below)
  -> runs row, status queued
  -> executor claims it (FOR UPDATE SKIP LOCKED; at most one running per agent, per conversation for a chat agent;
     a continuation not before its run_after)
  -> StartRunContext: memory + unread messages (marked consumed) + open questions
  -> memory entries the agent reads: an index in the prompt, the entries in full as an input
  -> session plan: continue the agent's Claude Code session, or start fresh
  -> a fresh session: the recent thread, packed into a byte budget, as conversation.md
  -> sources fetched by the harness, written to <workdir>/inputs/, stored as artifacts
  -> transforms run over sources, each output an input (a source only transforms read is withheld)
  -> repositories (repos): each fetched into orbit's mirror, its tree at the ref's commit under inputs/repos/<name>, _repos.json an input
  -> gates (scheduled and webhook runs, no message waiting): one that finds nothing to judge ends the run skipped
  -> runner called with: system contract, prompt, workdir, tools, output schema
  -> output validated against the contract
  -> RecordOutcome, one transaction: run succeeded, memory version, questions, actions
  -> notifications (unattended runs only): the report, then one per action waiting for approval
  -> action loop executes approved actions (auto ones arrive approved by rule)
```

Everything a run read and wrote is in Postgres: the prompt, system contract,
schema, every input, the raw output, the backend's full event stream, every
delivery attempt. The workdir is scratch and is removed after the run, except
for an agent with a session (below), whose workdir and HOME persist and whose
inputs go in a per-attempt directory removed after the run.

## Where agents come from

An agent is a file (`agents/<name>.yaml`), a database definition written
through the API (`agent_definitions`, every version kept), or built in
(`spec.Builtins`: `orbit`, the general agent). A database agent may be rendered from a template (`templates`, `template_versions`: a spec with `{{ }}` slots and typed parameters, `internal/templates`) by code, then defined through the same path (`engine/templates.go`; docs/api.md, Templates). `engine.LoadConfig` merges
the three and validates them as one configuration; a reload follows any
change to the files or the definitions.

## Conversations

An agent has many conversations (`conversations`; the person opens them as
"New chat") and one default conversation, where schedules, manual runs and
the agent-level API speak. Every message and run is in exactly one
(`conversation_id`, NOT NULL; a foreign key on `(conversation_id, agent)`
keeps it the agent's own), and a row written without one (the previous
release during a deploy) lands in the default by trigger. What a run reads
of its person is its conversation's only: unread messages, open questions,
the thread below, and the session it continues. Memory, schedules and rules
stay per agent, and so does the running run (`runs_one_running_lane`), so a
report agent's conversations queue behind each other rather than race on its
memory. A chat agent's conversations are separate sessions with separate
workspaces, so its lane is the conversation: one run per conversation, side
by side (`store.laneOf`); two that both replace its memory keep the later.

A run that starts a fresh session is shown its conversation's recent thread: the
person's messages earlier runs read, earlier reports and errors, and the
questions asked, newest kept, within `conversationBudget` (24 KB, each item
clipped to a quarter of it; `internal/engine/conversation.go`). It is stored
as the `conversation.md` artifact. The packing is deterministic.

## Sessions (claude-code, opt-in per agent)

`session: {resume: true, idle: 6h, live: 10m}` keeps an agent's Claude Code
session between runs. Claude Code stores a session's transcript at
`$CLAUDE_CONFIG_DIR/projects/<cwd, non-alphanumerics as ->/<id>.jsonl`
(`runner.SessionFile`); orbit pins `CLAUDE_CONFIG_DIR` to
`<data>/agents/<name>/home/.claude` and the cwd to `<data>/agents/<name>/work`,
so `ORBIT_DATA_DIR` must be a persistent volume for resume to survive a restart. The transcript is also stored centrally (`session_transcripts`, Worker fleet below) and written back on whichever host claims the next run, as the Agent SDK's SessionStore does; a run claimed where it is on neither starts fresh rather than failing.

- **Resume.** A run continues the session of its conversation's latest finished run
  when that run succeeded on Claude Code under the same spec within `idle`,
  no live turn on the session failed, the chain is under `maxSessionDepth`
  (20) runs, and the transcript is on disk. Otherwise it starts fresh and the
  `orbit.session` event says why. Every resume is `--resume <id>
  --fork-session`: a new id each run, so an interrupted attempt never edits
  the session the next attempt continues. A continued run's prompt keeps the
  charter and memory (small, and the authority if the CLI compacts) and drops
  the conversation, which the session holds.
- **Live process.** A run a person's message starts opens one `claude -p
  --input-format stream-json` process and leaves it open for `live`; the next
  run of that conversation (any trigger) is written to it as one user message
  while it holds the session that run would resume. The process is keyed by
  the conversation, so a turn never lands in another conversation's process.
  Each message still is a run row
  with its own events and outcome. A turn ends at its `result` line, which
  carries that turn's own `structured_output` (verified on 2.1.316). Any
  failure, a crash, a timeout or a mismatch closes the process; at most
  `runner.MaxLive` (4) are open, the least recently used idle one closed first.
- **Recorded per run:** `session_mode` (fresh, resumed, live),
  `session_parent`, `session_depth`, `session_id`, and `session_cost_usd`. The
  CLI reports a cumulative cost per session (across turns, resumes and forks),
  so `cost_usd` is the difference from the parent run's total.
- **Tokens and the prompt cache.** Every turn sends the whole conversation again, so what a session costs is decided by whether the prompt cache hits. Each attempt records its tokens (`internal/tokens`: uncached input, cache reads, cache writes, output, the model, the backend's turn count, the split by model) from the backend's own report: Claude Code's result line (its `usage` and `model_usage_since_last_result` are the turn's; `modelUsage` is the process's running total), Codex's `turn.completed`, the API's `usage`. Before the call, the engine records the attempt's stable prefix (`runner.PrefixOf`: digests of the model, tools, orbit's tool names, the output schema, the skills digest, the system contract, the CLI's working directory and the charter), and a run's `prefix` names the parts that differ from its conversation's previous run, so a cache bust is visible as the part that caused it. `/usage` totals tokens by agent, model and day with the cache hit ratio (docs/api.md).

## Chat agents (claude-code, `chat: true`)

A chat agent is a plain Claude Code session, as a person's own terminal is: the built-in `orbit` agent is one (`spec.Builtins`). `engine/chat.go` and `runner/chat.go`.

- **The contract.** No `--json-schema`: the reply is the turn's final message (the `result` line's text), stored as the run's report, its first line the summary. Memory, questions and actions are tools orbit serves during the turn, `mcp__orbit__remember`, `mcp__orbit__ask` and `mcp__orbit__act` (the agent's enabled action types, each validated when called), beside `mcp__orbit__search` (the in-run search below, answered in orbit's process), and become the run's outcome exactly as a report agent's output does (`RecordOutcome`). They are an SDK MCP server (`{"type": "sdk", "name": "orbit"}` in `--mcp-config`): the CLI sends each JSON-RPC message to orbit's process as an `mcp_message` control request on its stdout and orbit answers on its stdin (`claudeproto.Handler.MCP`; verified on 2.1.316), so the tools run in orbit, with the store, wherever the CLI runs.
- **The workspace** is the conversation's directory (`<data>/conversations/<id>/work`, HOME beside it), kept between messages. After every run its files (without `inputs/` and `.orbit/`) are stored as a snapshot (`workspaces`, every one kept, a new row only when the tar changed or the newest is a person's); a host whose copy is not the newest snapshot restores it before the run (`workspace.sha256` marks which it has). So a conversation continues on any host, and `GET /conversations/{id}/files` and the workspace API read the snapshot, not a host's disk. A person's edit through the workspace API is a snapshot of its own (`source: person`), refused while a run of the conversation is running; the next turn is told by code which files the person changed, and starts a new live process (a new sandbox) from their version, resuming the session ([workspace.md](workspace.md)).
- **Sessions and the live process** work as for any session agent (below), keyed by the conversation; the built-in has `resume` for a day and `live` for 10 minutes.
- **Permission levels** (`permissions: {level, ceiling}`; configuration.md, Permissions): `locked`, `ask`, `edits`, `auto`, `full`, mapped to the CLI's `dontAsk`, `default`, `acceptEdits`, `auto` and `bypassPermissions` (`runner.PermissionMode`). Every chat process runs with the stdio permission prompt; a request is answered by the run's `Chat.Prompt` and recorded as an `orbit.permission` event before the answer (none answers deny). `auto` is asked for with `set_permission_mode` once the process runs, and is `ask` where the CLI refuses it (`orbit.permissions`).
- **Instructions.** A chat run answering its person's message treats their message as the instruction: `mcp__orbit__act` executes an action whose rule is `auto` or `approve` during the turn (`Store.StartActionNow`, then the executor, then `FinishAction`), with the turn's workspace (`runner.WorkspaceOf`: the sandbox's `/work` over attach, or the workdir), and records `instructed_by`. Unattended runs keep their rules. An action executed later by the action loop gets the conversation's stored workspace.
- **Stop.** `POST /runs/{id}/cancel` on a running run sets `cancel_requested_at`; the attempt polls it (`engine.cancelPoll`, 2 s) and, for a chat turn, sends the CLI an `interrupt` control request (with `cancel_queued` when the init frame advertises `interrupt_cancel_queued_v1`, so steer messages still queued go with it), which ends the turn with a result; the run ends `cancelled` with the reply so far.
- **Steering a running reply.** A message posted with `delivery: "steer"` while the conversation's chat run works is stored waiting with `delivery = 'steer'`, and wakes no run (`store.steerTarget` locks the running run FOR SHARE, so a run finishing at that moment either sees the message or the message is queued). The attempt running the turn reads waiting steer messages with the same poll as Stop (`PendingSteers`), from the database, so it works across instances and rollouts, and the runner writes each into the turn as a user message whose uuid is the message id (`runner/steer.go`). The CLI folds a message written during a tool call into that turn when the call's result goes back, and one written after the last tool call starts a turn of its own after the result (`claudeproto.TestRealCLISteer`, 2.1.316), so the runner writes only while a top-level tool call is in flight; a message still unwritten when the turn ends is the next run's. When the CLI's `command_lifecycle` frame says `started`, the message is `steered_into` the run (and read by it, `message_reads`) and the journal gets a `steer` item. When the result arrives, every message written and not started is withdrawn (`cancel_async_message`; an interrupt's receipt counts): a confirmed withdrawal leaves it waiting, and an unknown outcome closes the live process (it may still run there) and leaves it waiting too. The run's end (`finishRun`, every way a run ends) queues a run for any waiting steer message of its conversation unless one is queued, so a steer the turn did not take is still answered: a `message` run when a person wrote one of them, otherwise (a parent's `task:message`) a `task` run, so a parent's message never runs as a person's instruction (`wakeForSteers`). A steer with no run running, to an agent that is not a chat agent, or with attachments, is queued as `delivery: "queue"` would be, and the answer says so.
- **Sandboxed** (`sandbox: workstation | build`, needs sandboxd: `sandbox:` or `build:` in orbit.yaml). The CLI runs in a sandboxd container (The build sandbox, below) created from the workspace's tar, attached with `sh -c 'CLAUDE_CONFIG_DIR=/work/.orbit/claude exec claude ...'` (the environment a caller may set is the credential alone, which sandboxd turns into a credential proxy token) and `--permission-mode for its permission level, bypassPermissions by default`: the container is the boundary. The profile is sandboxd's choice of box, named at create (`?profile=`): `workstation` gives the session Claude Code's full tool set (no `--tools` list), a HOME kept with the workspace (`/work/.orbit/home`, stored beside it as `box-home/` without `.cache`, with `~/.local/bin`, `~/go/bin` and an npm prefix on `PATH` so user installs persist) and egress to any public host through sandboxd's workstation proxy (`ORBIT_SANDBOX_WORKSTATION_PROXY`, an `orbit egress` with the allowlist `*`, on the profile's own internal network, `ORBIT_SANDBOX_WORKSTATION_NETWORK`, which no build joins); `build` gives the tools the agent names and the build allowlist. A later workstation tier (root inside, a docker daemon of its own, stronger isolation) is another profile behind the same name. The conversation's session transcripts ride into the tar under `.orbit/claude/projects/-work/` and come back to HOME after every turn, with the workspace (`Archive`), so resume, the session store and recovery work unchanged. A live sandbox stays up with its process (`session.live`, inside the sandbox's one-hour lifetime); files a later message brings (uploads, inputs) are pushed into it with `tar -x` over an attach of their own. When sandboxd is full (429), idle live sandboxes are closed and the create retried once. The built-in `orbit` is a `workstation` whenever orbit.yaml names sandboxd, and then needs the `sandbox` capability; without sandboxd it runs on orbit's host with read-only file tools, the web and subagents.

## orbit inside the CLI: tools and hooks (claude-code)

A Claude Code run starts orbit's own binary twice over, so deterministic work
happens in tested Go at the moment the model needs it (`internal/harness`):

- **Tools** (`orbit mcp`, an MCP stdio server from `--mcp-config` under
  `--strict-mcp-config`): `mcp__orbit__query` filters, groups and
  counts or sums the records of a JSON input, exactly (numbers are read as the
  decimal literals they are); `mcp__orbit__calc` is exact rational arithmetic; `mcp__orbit__skill`, listed only when the run has skills, serves their text (Skills, below).
  The system prompt tells the model to compute every figure with them. Both
  read only under the workdir, and append each result to a ledger in the run's
  state directory.
  `mcp__orbit__search` is full-text search over the agent's own history
  (below): the tool server asks the engine over a per-agent unix socket
  (`engine/search.go`), the engine fills in the agent, and its results are
  not ledgered, since an earlier run's words back no figure.
  With `repos`, the repository tools (Repositories, below) are served the same way, over a socket of their own, and are not ledgered either.
  With a `prometheus` source, `mcp__orbit__prometheus_query` runs read-only PromQL (instant or range, bounded in `sources.PromQuery`) against that source's URL with its credentials, asked of the engine over a socket of its own (`engine/prometheus.go`); its answers are Prometheus's numbers, so they are ledgered. A chat agent has it among its SDK tools.
- **Hooks** (`orbit hook`, from `--settings`; with `--setting-sources ""` no
  other hook is loaded, so `disableAllHooks` is on whenever orbit has none):
  PreToolUse on Read, Glob and Grep denies a path that leaves the workdir after
  symlinks, a second wall behind the CLI's own; with `verify: [figures]`,
  PreToolUse on StructuredOutput sends the output back when a `$` amount in its
  summary, title or report is in neither the inputs (as written, or as cents)
  nor the ledger, rounding to the figure's precision allowed, at most
  `harness.MaxFigureDenials` (2) times per prompt. The engine runs the same
  check on the final output for every backend and notes any amount it cannot
  trace in the report. UserPromptSubmit, in every mode, adds the run's pinned
  entries (Memory entries, below) as context, from the file the engine wrote
  for the run (`harness.PinnedPath`).
- **Status** (`internal/status`): with orbit's binary, every Claude Code hook event (`status.HookEvents`; agent-team events left out) runs the same `orbit hook`, which writes its decision first (`{}` for any event but PreToolUse and UserPromptSubmit, so no decision of the CLI's own is changed) and then posts a small summary of the event to the attempt's listener over a unix socket only orbit's user can open (`status.SocketPath`, in the temporary directory and named for a hash of the state directory, so a live process's fixed hook command reaches every run it serves). When the socket does not answer, the hook appends the event to the spool in the state directory, which the listener reads every second and once more after the CLI returns. Reporting never changes the hook's output or exit status: an exit code 2 would block a Stop or deny a tool. The engine records each event of the attempt's own turn as an `orbit.hook` event and each change of the turn's state on the run (`hook_status`). An event stamped before the attempt began, or naming another turn (`prompt_id`, adopted from the first event that names one), is an earlier run's and is ignored.
- **Agent status** is derived (`status.Derive`): while a run is running, its hooks speak for it wherever they reported for its current attempt (UserPromptSubmit and tool events: working; PermissionRequest, Elicitation, an AskUserQuestion call and permission or elicitation notifications: waiting; Stop: done, shown as working until orbit has recorded the output; StopFailure, or SessionEnd before Stop: errored); the run's row fills in where they have not. A final run decides by its row: failed is errored, a finished run with an open question or a pending action is waiting on its person, otherwise done. A subagent's events change the turn's state only to wait on the person, and only the agent that waits clears its wait. PreCompact and PostCompact change nothing: an automatic compaction is mid-turn.
- Claude Code runs `statusLine` only in its interactive interface, never under `-p` (checked on 2.1.316 with one-shot and stream-json input), so subscription windows still come from `rate_limit_event` (docs/api.md, Usage).
- **An agent's own hooks** (spec `hooks`): `say`, `deny` or `run` at a Claude Code event, each run by `orbit agent-hook` with its definition as a flag (docs/agents/configuration.md, Hooks), added to the same `--settings` after orbit's own.

## Connectors

An agent's `connectors` give its Claude Code runs the tools of MCP servers orbit.yaml names (`internal/connectors`, `engine/connectors.go`, `runner/connectors.go`). orbit is the gateway, so no credential enters the CLI or a sandbox:

- **Before the CLI starts**, the engine lists each connector's tools (MCP streamable HTTP, with the connector's credential: a header bound like a source's credential, or an OAuth token a person signed in for, sealed in `connector_tokens`, a version per sign-in and per refresh) and decides each tool's rule: the agent's `mcp:<connector>:<tool>`, else `approve` for one in `write_tools`, else `auto`; `deny` is not offered. A connector that cannot be listed is left out, and the system prompt says so beside each offered connector's description.
- **In the CLI**, each connector is an SDK MCP server (`{"type": "sdk", "name": <connector>}` in `--mcp-config`), its tools `mcp__<connector>__<tool>`; `auto` tools are in `--allowedTools`, `approve` tools are ask rules in `--settings` with `--permission-prompt-tool stdio` (and the default permission mode: dontAsk denies an ask rule unasked). A report run with connectors talks stream-json too, since their traffic rides the control channel. Verified on 2.1.316: several SDK servers in one config, a dash in a name, the tool use id in `tools/call`'s `_meta` matching the prompt's, and an ask rule reaching the prompt in the default and bypassPermissions modes (`TestRealCLIConnectors` holds all but the sandbox's bypassPermissions).
- **A call** arrives as an `mcp_message`; orbit forwards an `auto` one to the server and returns its result, or, over 32 KiB, writes it into the attempt's inputs as JSON, copies it into a live sandbox, and returns its path. An `approve` call first reaches orbit as a permission prompt: in a run answering its person's message, orbit records a pending `connector:<connector>/<tool>` action (in the thread and the inbox at once) and holds the prompt until the person decides; on approval it claims the action, makes the call, and allows the tool, whose `tools/call` then gets that result by its tool use id. In any other run orbit records the pending action, denies the prompt saying so, and the action loop executes it at most once after approval; its approval is notified like any other. Every call is an `orbit.connector` event with its bounded result.

## Repositories

An agent reads git repositories through orbit, not through the web (`internal/repos`, `engine/repos.go`; configuration.md, Repositories). orbit keeps a bare mirror of each repository in `github.read_repositories` that an agent names, under `<data>/mirrors/`: every branch, tag and pull request head (`refs/pull/<n>/head`), fetched before each run with a `contents: read` token that exists only for that fetch. The run gets the tree at the ref's commit, without `.git`, at `inputs/repos/<name>/`, and `_repos.json` (repo, ref, sha) as an input: the prompt names each path and commit, the `unchanged` gate sees a new commit as a change, and a recovered attempt extracts the same commits again. A tree is not stored as artifacts; its commit is the record, and the mirror can give it again.

History and GitHub's state are tools orbit runs, never a credential or a `.git` in the model's reach: `git_log`, `git_show`, `git_diff` and `git_blame` over the mirror, and `pr_list` (every page up to its limit, `merged_at` set only for a merge) and `pr_view` (with the head commit's check runs and statuses) from GitHub with a read token minted per call. A Claude Code report agent reaches them through `orbit mcp`, which asks the engine over a per-agent unix socket (as search does); a chat agent has them among its SDK tools. A build's new clone is seeded from the mirror's default branch head (`build.Pool.Seed`), so its fetch from GitHub transfers only what the mirror lacked.

## Mail

With `mail` configured (configuration.md), a person talks to orbit by mail as well as by the API (`internal/mail`):

- **Out.** Every mail orbit sends for a run carries `Reply-To: local+<token>@domain` and `Message-ID: <orbit.<token>.<random>@domain>`, where the token is the run's conversation id and an HMAC of it under the operator token (`mail.Token`), plus `References: <orbit.<token>@domain>` so a mail client threads a conversation. A run that read a mail is answered by mail, whatever `notify.when` is (`never` included) and although a message triggered it: its report and questions, or a short note with the first line of the error when it fails, go to the mail's sender (`To`), as a reply to that mail (`In-Reply-To`, `References` after the conversation's, `Re:` its subject), through one channel: the agent's first `smtp` channel, else the deployment's first by name. Nothing goes to the agent's other channels. A later chat message in the same conversation is answered in the chat only, since its run read no mail; a run answering the portfolio chat notifies nothing.
- **In.** A poller (every `poll`, on each instance that takes claims) reads the unseen mails of the IMAP mailbox. Each is parsed in code: the sender, the tokens it names (a recipient's plus address, then `In-Reply-To`, then `References`), and its text (plain, or the HTML as text) with the quoted mail and signature cut deterministically (`StripQuoted`). A mail from an allowed sender that names a signed token becomes a message in that conversation; one that names none opens a new conversation with `mail.agent`, titled by its subject. Automatic mail (`Auto-Submitted`, `Precedence: bulk`) and a failed DMARC result in `Authentication-Results` are ignored, so an out-of-office reply cannot start a loop. The author is the sender's address.
- **Once.** `IngestMail` writes the message, its run and the `mail_in` row in one transaction keyed by Message-ID, and the mail is marked seen only after; a database error leaves it unseen for the next poll.
- **Attachments** are not uploads yet: the message names each (file, type, size) and the raw mail is kept in `mail_in`.

## Continuations

An agent with the `continue` action may ask to be woken again
(`after_minutes`, 1 to 1440) with a note to itself. When executed, it queues a
`continue` run with `run_after`, in the asking run's conversation;
`runs_one_continuation` keeps one waiting
per agent, and a chain is bounded by `actions.MaxContinueChain` (12): the
run at the limit has its proposal denied, and `EnqueueContinuation` holds the
same bound. A long task becomes a chain of short checkpointed runs, each with
the session continued when the agent has one.

## Delegation

One agent coordinates others. An agent whose spec names `delegates` (agents, or `"*"`) and a rule for `delegate` proposes `delegate` with a batch of tasks: each a `key`, an `agent`, a `brief`, optionally `after` (keys of the tasks it waits for: in the same batch, or, for a key the batch does not have, the newest earlier task of that key this conversation delegated), `fresh` and `decision`. Executing it (`store.Delegate`, one transaction) gives each task its own conversation of its agent, with the brief as the first message (author `agent:<parent>`), and returns the tasks' ids as the handle. Code does the rest (`store.ReconcileTasks`, on control after each run, after delegation actions and every 5 s):

- **Start.** A task starts, as a `task` run in its conversation, once its dependencies are done and any decision approved; the run is told it reports to an agent, not a person, and given its dependencies' reports. Tasks of different agents run in parallel; one agent's queue behind each other (`runs_one_running_lane`), except a chat agent's, one per conversation.
- **Outcome.** A task's run says whether it did the work: its output carries `outcome` (`succeeded`, `failed`: it could not, or `blocked`: it cannot go on until something outside its reach changes) and `outcome_reason`, fields only a run in a task's conversation is offered. A run without them (a chat turn) succeeded.
- **Settle.** A task is `working` while a run of its conversation is queued or running, or one of its actions is approved and not yet executed (a `continue` about to queue); then its newest run decides: failed or cancelled end it so; succeeded with outcome `failed` fails it (`error` gives the reason, `report` keeps what the run reported); an open question or outcome `blocked` makes it `needs_input`; otherwise it is `done` and that run's summary and report are its `report`. A cancelled dependency cancels its dependents; a failed one keeps them `waiting`, for the parent to retry it or cancel them.
- **Retry.** `task:retry` (`task_id`, `reason`, optionally a new `brief` or another `agent` of its `delegates`) reopens a failed task on the same row (`store.RetryTask`): the attempt it ends is kept in `task_attempts`, the task waits again and starts as before, and its conversation is told why it runs again; another agent gets a new conversation, the brief its first message. The circuit breaker: a task counts its `failures` (a run the reaper failed after losing its worker does not count, and reclaims never settle a task), and past `MaxTaskFailures` (3) a retry is held for a person's approval whatever the rule, even in a run answering the person, and the store refuses one no person approved.
- **Wake, never poll.** A task needing input or failed is due to its parent at once, so the parent can answer, retry or reroute while the task's siblings still run; other finished tasks when their whole batch is over (`report_due_at`). A parent conversation with a due task and no run started since gets one `delegation` run, and never more than one waiting (`runs_one_delegation`; any queued run there will read the same state), whose prompt lists every task with its status and, in full, what is new: reports, questions with their ids, errors. Any run in that conversation sees the same section, and one that started after the change clears it, so a parent is told once.
- **Stalls.** A batch none of whose open tasks can move (each waits, directly or through another open task, only on a dependency that failed, or on a decision pending longer than `defaults.decision_stall`, 24h) wakes its parent once: its open tasks get `stalled_at` and are due. A task working or needing input can move. `stalled_at` clears when one can move again, so a later stall wakes it again.
- **Mailboxes.** A task's questions go to its parent, never to the person (a task's run notifies nothing but approvals, and its open questions do not make anything need the person's input). The parent answers with `task:message` (`reply_to` the question), or corrects a task, or escalates by asking its own person. `task:message` writes into the task's conversation (`store.MessageTask`): while a run of a chat agent works there and nobody asked it to stop (`steerTarget`), the message is a steer, written into that run's turn as a person's steer is (Steering a running reply, prefixed `from <parent>:`), and no run is queued; otherwise it queues a run to read it, or joins one already queued (a done task reopens). The result's `delivered` (`steered` with that `run_id`, or `queued` with the run that reads it) is what a chat parent's `act` call returns. A steer the turn did not take is answered by the task run its end queues. `task:cancel` ends a task. A person can read and write any task's conversation like any other.
- **Decision gates.** A task with `decision` waits `blocked` on a `task:decision` action of the parent run, which only a person resolves (approve or reject, as any approval; no rule decides it). It is re-checked every reconcile: approved starts the task, told so; rejected cancels it and its dependents.
- **Interactive and unattended.** In a run answering the person's own message, delegation's types under an `approve` rule apply directly (`engine.ruleFor`); every other run follows the rules as written. `task:message`, `task:cancel` and `task:retry` follow the `delegate` rule unless named.
- **Fresh workers.** `fresh: true` runs the agent's spec as a template: the task's runs read none of its memory and write none.
- **The general agent** (`orbit`, built in) delegates to every agent (`delegates: ["*"]`, `delegate: approve`): the super agent a person talks to, which hands work to the others.
- **Bounds** (`store`): `MaxTasksPerDelegation` (8) per action, `MaxTaskDepth` (3) levels of tasks delegating, `MaxConversationTasks` (64) per conversation, `MaxTaskFailures` (3) failed attempts per task before only a person reopens it.

## The output contract

Every backend returns the same JSON (`internal/engine/contract.go`):
`summary`, `report` (Markdown), `notify` (send, priority, title), `memory`
(update, the complete new document), `questions` (each a string, or a question with a form), `actions`, and, only for a
run in a request ticket's conversation that is still being discussed,
`plan` ([tickets.md](tickets.md)), only for a run in a delegated task's conversation, `outcome` and `outcome_reason` (Delegation, above), and `entries`, the memory entry writes the run makes (below); skills are written with the `skill:write` action (Skills, below). The backend is
given a strict schema in which each action type's parameters are spelled out;
the engine validates against a looser envelope and then judges each action on
its own, so one malformed proposal is denied without losing the report.

The person reads the report, so it must hold the whole answer: the contract says so, and the engine enforces it (`fullReport`). When the run's final assistant prose (`journal.FinalProse`: its last run of top-level assistant messages, projected from the transcript as the journal is) holds more than the submitted report (that is empty, one line where the prose has several, or shorter), the prose becomes the report. The submitted summary stays, and `output.json` keeps the submitted report.

## Backends are interchangeable

A runner takes a fully built request and returns the output JSON. Nothing an
agent means is resolved inside a runner, so moving an agent from Claude Code
to Codex or to a plain API call is one line in its spec.

| kind | how | auth | tools |
|---|---|---|---|
| `claude-code` | `claude -p --output-format stream-json --json-schema` | `CLAUDE_CODE_OAUTH_TOKEN` (subscription, `claude setup-token`) or `ANTHROPIC_API_KEY` | Read/Glob/Grep in the workdir, plus the agent's `tools` |
| `codex` | `codex exec --json --output-schema -s read-only` | `CODEX_API_KEY` / `OPENAI_API_KEY` / `CODEX_HOME` | its read-only sandbox; an agent with `tools` fails validation |
| `anthropic` | Messages API, schema as a forced tool | `api_key_env` | none (an agent with `tools` fails validation); inputs inlined (bounded) |

## Notifications

Channels are for runs nobody is watching. A run a person's message woke
(trigger `message`) notifies nothing: its reply, questions, pending
approvals and failure are in the conversation they are reading
(`engine.Attended`). Schedule, manual, continue and build runs deliver per
the agent's `notify.when`, and always their failures and the approvals
they wait on.

Separately from channels, everything waiting on a person is an `attention`
row (store/attention.go), written in the transaction that created it, for
attended runs too. It feeds the app's inbox (`GET /attention`) and, for
unattended runs, Web Push to the person's devices (`internal/push`), which
carries a title and a link only; bursts fold into one summary push.

## Memory, questions, messages

- **Memory** is one Markdown document per agent, replaced whole by a run that
  sets `memory.update`. Every version is kept (`memory_versions`).
- **Memory entries** are the durable layer above it (below).
- **Questions** a run asks become `from_agent` messages and are delivered with
  the report, in the asking run's conversation. A question may carry a form (`fields`, `internal/forms`: typed fields with options, defaults and required), checked by code when it is recorded; a reply's `answers` are checked against it when they are posted and listed in the reply's body, which is what the agent reads (docs/api.md, A conversation's thread). A question stays open until
  a reply names it or any later message reaches that conversation; open
  questions are shown to the conversation's next run so it does not ask
  again.
- **Messages** to an agent are consumed by the first run of their
  conversation that starts after them. A run that fails or is interrupted hands them back. Each attempt's read is a `message_reads` row, kept when it hands the message back (`released_at`); a message waits while no attempt holds it (`store.waitingMessage`).

## Memory entries

What a person teaches orbit, and what orbit learns about them, outlives any one agent's memory document. Memory entries hold it as one fact each (`memory_entries`, every version in `memory_entry_versions`; nothing is deleted, a retired or superseded entry is a version), with Claude Code's memory types (`user`, `feedback`, `project`, `reference`), a title, a one-line description, a body, why it holds and how to apply it, and where it came from (a person through the API, a learning run, an import). Three scopes decide who reads one:

- **profile**: the person; every agent.
- **topic**: a domain (`finance`, `homelab`) shared by the agents that read it: the spec's `topics`, plus topics the API attaches (`agent_attachments`). `topics: ["*"]` (`spec.AllTopics`) is every topic there is (holding entries, attached, or named by any agent's spec); the built-in `orbit` agent reads every topic, since it is the person's general session.
- **agent**: one agent's own.

A run that starts fresh (not a recovery) reads every active entry in its scopes (`Store.EntriesFor`) and gets them twice (`internal/engine/known.go`): an index in the prompt (`# What you know`: one line per entry, the profile, then each topic, then the agent's own; within each, feedback first, then user, project, reference, newest first), packed into `knownIndexBudget` (8 KB, each line at most `knownLineMax`), and every entry in full as an input file, `inputs/_memory.md`, within `knownFileMax` (256 KB), stored as an artifact like any input. When the index cannot list every entry (many topics, as the `orbit` agent reads), it keeps them by `knownPriority`: every feedback entry first, then the most recently changed, whatever the scope, so the first topics by name never crowd out the rest; it still shows them grouped as above, and ends with how many it left out (how many of them feedback), that they are in `_memory.md`, and, when the run has the tool, to find one with `mcp__orbit__search` (kind `memory_entry`). The system prompt says what the entries are and that they never authorize an action the rules do not. The packing is deterministic, so the same entries give the same prompt.

**Pinned entries.** A feedback entry with `pinned` (a field versioned like the rest; only feedback is pinned, and an edit that makes an entry another type unpins it) is a rule that must hold however long a session runs. The index lists it as `[feedback, pinned]`, and every turn of a run that reads it is given it again, fresh, beside the person's words, so it does not fade as a long or live session compacts the charter and the index (`renderPinned`: newest first within `pinnedBudget`, 2 KB, each as its title and how to apply it). Where orbit's hook runs (a Claude Code run with the harness, outside a sandbox) the engine writes them to the run's hook state and the UserPromptSubmit hook adds them as context to each prompt, including every turn a live process takes; elsewhere (a sandboxed chat, another backend) they lead the run's prompt. A person pins through the API; an agent's entry writes leave it as it is, as an import does.

**Agents write their own entries.** Every run's output has `entries` (at most `maxEntryWrites`, 10): `add` (to the profile, the agent's own, or a topic it reads), `update` (only the fields given) and `retire` (with the entry that supersedes it, possibly one the same run adds), each naming an entry by the ref the index showed (`internal/engine/entries.go`). They are applied in `RecordOutcome`'s transaction, each in a savepoint: a write the store refuses (a version written since the run read it, such as a person's edit; an add whose key exists, which is how a near-duplicate is turned into a refusal rather than a second entry) is skipped and reported, and the rest apply. Nothing waits for approval: every version is kept, and a person reverts or retires through the API. What became of each write is the run's `entries.json` artifact.

**`/remember`** (`internal/engine/remember.go`). A person's message that starts with `/remember` is written as an entry by code when the run reads it, not left for the learning pass: `/remember [--agent | --topic <name>] [--feedback | --user | --project | --reference] <text>`. The place is the profile unless `--agent` (this agent's own) or `--topic` (one the agent reads) says otherwise; the type is a flag's, else inferred from simple cues (a link or "can be found": reference; "I am", "I live", "my job": user; "we", "our", a deadline: project; anything else: feedback); the title is the text's first sentence, within 80 bytes, and the body the whole text. The version is `source: person`, authored by the message's author, with the run and conversation. The same text already in that place is reported, not written again, so a retried run never doubles an entry. The reply says what was saved, or why not (an unknown flag, a topic the agent does not read): a run the message woke with nothing else to answer ends with that reply and no backend call; beside other messages, the model is told what was saved and the confirmation follows its reply. Messages an agent wrote (a task's brief, `task:message`) are never commands, and the learning pass leaves `/remember` messages out of its window.

**Learning runs** (`internal/engine/learn.go`) turn a quiet conversation into entries. Every serving instance, every `LearnEvery` (a minute), asks the store which conversations of agents that learn (`learn`, on unless disabled) have a message from the person after the last succeeded learning run's point and have been quiet for `learn.idle` (default 30 minutes): no message or run start, no run finished, nothing queued or running (`Store.LearningDue`). Each gets one learning run (trigger `learn`), unique per conversation and point (`runs_one_learning`), so instances never double it and a failed one is not retried until the person says something new: the cost is bounded by what the person writes, and a run whose window holds no message from them is `skipped` (`learn:nothing-new`) without a backend call. The run reads the window (the person's messages up to the point, the agent's questions and reports after the previous point; `learnBudget`, 32 KB, newest kept), the agent's charter and its entries (index and file, as a run gets them), and returns only `summary` and `entries`, applied as above with the conversation it read as their source. It is a one-shot backend call (no session, no tools, no sources) on the agent's runner or `learn.runner`, within `learnTimeoutMax` (5 minutes). It lives in the agent's learning conversation (`conversations.purpose`, archived), so the conversation it read keeps its thread, prompt and session exactly as they were. A learning run never notifies; its failure is on the run.

`orbit import claude-memory` maps a Claude Code memory directory (one file per fact, frontmatter `name`/`description`/`type`, `**Why:**` and `**How to apply:**` paragraphs, the `MEMORY.md` index for titles) onto entries by key, through `POST /memory/import`, so a repeated import changes only what changed (docs/agents/deployment.md). `orbit sync claude` is the reverse (`internal/claudesync`): it writes entries back as memory files and `MEMORY.md` lines, and skills as skill directories, replacing a local copy only when it is exactly a version orbit holds, and applying only the plan whose digest a dry run printed.

## Skills

orbit keeps Claude Code skills in a library (`skills`, `skill_versions`, `skill_files`; every version kept, `internal/store/skills.go`) and gives each agent the ones its spec names (`skills`) or the API attached, at their current versions (`internal/engine/skills.go`):

- **Claude Code** loads them as real skills from a plugin, `<HOME>/.claude/orbit-skills-<digest>/`, passed with `--plugin-dir` (with `--setting-sources ""` Claude Code reads no skills directory of its own; verified on 2.1.316), with `Skill` added to the run's tools. The plugin holds a stub per skill: its name and description, which is what the CLI decides by, and an instruction to read the rest with `mcp__orbit__skill`. orbit's tool server serves each skill's `SKILL.md` and files from the run's own copy under its state directory, confined to the skill's directory after symlinks, so what the model follows is exactly the version the run was given, and a skill's files are readable without a path outside the workdir. The directory's name carries a digest of the skills' contents: an unchanged set is reused, a changed one replaces it, and because the digest is in the CLI's arguments, a live process holding the old set is closed rather than fed a turn. Without orbit's tools (no harness binary) the plugin holds each full `SKILL.md` instead, and its files are not reachable.
- **Other backends** get every `SKILL.md` as one input file, `inputs/_skills.md`.

A run records the versions it had (`skills.json`). It writes or retires skills with the `skill:write` action, under its rule like any action (unnamed, it is denied and not offered): at most `maxSkillWrites` (3) per run, resolved by code when proposed against the version the run had (a retire must name one it has), with what the write changes worked out then (`actions.SkillChanges`: SKILL.md's diff and line counts, the files a write keeps, and which of them are scripts or executables), which the approval shows. Executed, it applies against that version (`store.ApplySkillWrite`; a newer one fails the action), and a written skill is attached to the agent; the action's result is the record of what it changed. `orbit import skills` seeds the library from a Claude Code skills directory.

## Actions and rules

An action type is code (`internal/actions`): a name, a parameter schema shown
to the agent, a validator, and an executor. An agent's spec maps types to
`auto`, `approve` or `deny`; unnamed types are denied and not offered.
Today: `notify` (an extra message to one of the agent's channels),
`continue` (wake this agent again later, above), `schedule:add`, `schedule:pause`, `schedule:resume`, `schedule:remove` and `run:now` (an agent's triggers changed from the person's words, parsed by code, each a new version; configuration.md, Changing schedules by talking), `agent:save` (the conversation as a new agent: the model writes its words, code its reach from what the conversation did, held for a person as an `agent:define`), `agent:define` (create or change an agent defined in the database, itself included, as a new revertible version; a change that widens it waits for a person; docs/api.md, Defining agents), `template:create` (an agent from a template, rendered by code and recorded as an `agent:define` that waits for a person or a question with a form for values the person must supply), `skill:write` (write or retire a skill, Skills above), `github:clone` and `github:pr` (a chat agent's repository work: orbit clones into its workspace and commits and pushes its working tree to `orbit/chat/<branch>`, `build.Git`), `webhook:<endpoint>` (a
JSON body to an endpoint whose URL, method and credentials are configuration, never
chosen by the model; an endpoint's `path` may have `{name}` placeholders the action fills from params checked against the endpoint's declared types, and a `body_schema` the body must match, so a write like Sure's `PUT /transactions/{id}` is expressible without the model choosing a route; configuration.md, Endpoints), `connector:<connector>/<tool>` (a connector call that waits for approval, Connectors, above; recorded by orbit during the run, never proposed in an output), `github:merge` (squash one of orbit's own
`orbit/*` PRs, only if its head is still the approved commit and every
required check passed; [tickets.md](tickets.md)), and `github:comment` (a comment on an issue or pull request of `github.comment_repositories`, as the App with an `issues: write` token per comment).

Approval happens from a signed link (`/ui/actions/{id}?sig=`), the API, or
`orbit approve`. An action is executed at most once: one interrupted during
execution is failed for a person to check, never retried.

## The journal

A run's journal (`internal/journal`, `journal_items`) is its transcript in one typed shape for every backend: messages, thinking (with a token estimate), tool calls with their full input and bounded results, diffs, plans, approvals, questions, status, and turns whose state (running, completed, interrupted, unverifiable) is kept apart from the backend's verdict. Code derives it from `run_events` as each event is stored, and backfills older runs with the same code, so the two agree; `run_events` stays the record it is derived from. Items are append-only revisions addressed by (run, epoch, seq): an epoch is a timeline of attempts that continue each other's sessions, so a cursor never mixes a resumed attempt with an abandoned one. Large values are a bounded head with their size and digest, the whole value fetched by digest. The stream, a thread run's `steps` and `turn`, MCP progress and the run page (`/ui/runs/{id}`, each item's latest revision, a subagent's nested under its call) all read it. Shapes: docs/api.md.

## The API

`internal/api` serves two listeners: the public one traefik routes to, where
`/api/v1` takes authentik tokens verified by orbit (`internal/auth`) and
`/ui` sits behind forward-auth, and an internal one that also takes the
operator token. A conversation is read as one thread (messages and runs
interleaved, `Store.Thread`), and a run is watched as Server-Sent Events
(`/runs/{id}/stream`), which poll the run's journal and send its rows,
with a prose-only view of them beside. The stream reads only rows,
so it follows a run across a requeue, a deploy or another replica with no
signalling between writers and readers. `GET /usage` and `GET /health` are
the admin read side, computed from stored rows and the configuration in
force: subscription windows (the freshest per window of the newest
`rate_limit_event` Claude Code wrote into a run's stream and, with
`usage_poll`, the newest poll of the endpoint Claude Code's `/usage` reads;
each says which and when), exact cost sums, the queue, held runs, running
runs and next fires. `/mcp` serves the same reads and writes as MCP tools (`internal/api/mcp.go`, stateless Streamable HTTP), with an OAuth proxy in front of authentik for clients that need dynamic registration (`oauth.go`). The chat UI is the portfolio's (kanishksachdev.com/orbit, its own repository), a client of `/api/v1` and the streams. Contract: docs/api.md.

## Search

Everything a person or an agent wrote is searchable (`Store.Search`, `GET /search`, the `search` MCP tool, and in a run `mcp__orbit__search`): people's messages, agents' questions, run reports and errors, the prose a backend narrated, each tool call's input, memory versions, memory entries, skills, and the prose of imported sessions (below; a run's in-run search covers its own agent's records and the memory entries it reads: its own, the profile's and its topics', and no imported sessions, which belong to no agent). A chat run has the same search among the tools orbit serves it (`chatTools`); any other Claude Code run reaches it through the harness's tool server and a socket per agent. `search_docs` holds one document per piece with a stored English `tsvector` and a second `simple` `code_tsv` over `orbit_search_code_text`, both GIN indexed and derived by triggers from source tables. The code index splits identifiers at punctuation and camelCase so different spellings match. Tool call documents also expose `repo` and `path` to the `repo:` and `path:` operators. The `agent:`, `kind:`, `repo:` and `path:` operators are parsed in code and apply as filters; a first page with no match is retried once with misspelt words corrected from `search_terms` using `pg_trgm`. Queries are ranked by `ts_rank_cd` normalised by length, with `ts_headline` snippets whose matches are split out in code, never as markup in the text. Tool results are not indexed: they are inputs, already in artifacts, and would drown every query.

## Imported sessions

Most of what a person has worked out with Claude Code lives in the CLI's transcripts on their laptop (`~/.claude/projects/<cwd key>/<session id>.jsonl`), not in anything orbit wrote. `orbit import claude-sessions` brings them in (`external_sessions`, `external_session_lines`, `external_line_contents`; `internal/store/external_sessions.go`), so search reaches them and orbit can learn from them.

- **Lines, stored once.** A transcript is one JSON object per line, appended to. Each line is stored in a canonical form (`claudeimport.NormalizeLine`: keys sorted, its `sessionId` taken out, credentials scrubbed by `claudeproto.ScrubCredentials`, a line over `MaxSessionLine` cut to the skeleton that keeps the conversation's structure) under the SHA-256 of that form, and a session is its digests in order. A resumed or forked session starts with its parent's lines under a new id; with the id taken out they are the parent's bytes, so they are stored once and the fork adds only its own.
- **Incremental.** A session records how many bytes of its file it holds (`offset_bytes`) and their SHA-256 (`prefix_sha256`). The client reads that state (`GET /sessions`), hashes the same prefix of the file, and sends only the complete lines after it; a line being written (no newline yet) waits for the next import. A file whose prefix no longer matches is sent whole as the session's next generation, the earlier one kept. An import item that does not continue the stored state (another import moved it) is a conflict and writes nothing, so two imports never interleave a session.
- **Scrubbed twice.** The client normalizes every line before sending it, so a credential it recognises never leaves the laptop, and the server normalizes again (the form is idempotent), so what is stored never depends on the client.
- **Searched** as kind `session`: a trigger on `external_session_lines` derives one `search_docs` row per line that carries prose (`orbit_search_session_text`: a user line's typed text, an assistant line's text, a summary), keyed by the line's digest, so a line two sessions share is one result.

## Reliability

- **Scheduling:** one leader by advisory lock. A fire is enqueued at most once
  (`runs_one_per_fire`). After downtime, each trigger runs once for its newest
  missed fire; a trigger that has never fired is not backfilled. A fire
  claimed past its trigger's grace (`spec.MissedGraceFor`: the time to its
  next fire, at most an hour, or `missed_grace`) ends `skipped`, gate
  `missed`, instead of running late.
- **Webhooks:** a signed delivery to `/hooks/{agent}/{trigger}` (`internal/api/hooks.go`, verified by `internal/webhook`) is enqueued with its body in one transaction, at most once per delivery id (`webhook_deliveries_once`), so a sender's retry or a second instance starts nothing new. The body is the run's `webhook` source, read from the database, so any host can run it.
- **Backend holds** (`internal/engine/hold.go`): a run whose backend refused orbit (an auth failure such as `Not logged in` or a 401, or a `rate_limit_event` with a rejected window before the failure) fails and holds its runner (`backend_holds`, one open per runner): every unattended run of an agent on that runner waits queued (the claim skips it), and one notification says why, instead of each run failing and notifying. A run a person started (a message, a mail, a manual run) that trips it reports its own failure as usual (the chat, a reply to the mail, a notification) and opens the hold without a second notification. Only an error the runner returned counts, never a source's 401. Control probes a held runner with one minimal call every 15 minutes, or once a rejected window resets; a probe that succeeds, or a person's message or manual run on the runner that succeeds, closes the hold and wakes the executors. Fires that waited past their grace then end `missed`.
- **Low-priority work waits for the subscription.** With
  `defer_low_priority`, the scheduler's leader holds the scheduled runs of
  `priority: low` agents in the queue (`run_after`, recorded in
  `run_deferrals`) while a window is above `usage.DeferLowAbove`, enqueueing a
  fire already held so no executor claims it in between, and lets them go
  once every window is below (`scheduler/defer.go`).
- **Shutdown** is a drain (Upgrades, below): runs in progress finish under
  their own timeouts, and only past the drain's cap are they handed back.
  Nothing a run does outside its own rows happens before it finishes, so
  another attempt is safe.
- **Lost workers:** a run without a heartbeat for `engine.StaleAfter` is
  requeued, and failed after `store.MaxAttempts` claims.
- **One claim, one writer.** A claim is an attempt (`run_attempts`), and
  every write a running attempt makes (heartbeat, runner, session, outcome,
  hand-back) is guarded by `status = 'running' AND attempt = <its own>`. An
  attempt that was reaped and reclaimed elsewhere is refused everything
  (`ErrConflict`); its heartbeat sees that, or sees no beat land for
  `StaleAfter` less one beat, and cancels it before another attempt can
  start. So two instances never both work on one run.

## Worker fleet

orbit runs on several hosts, as jobtracker's workers do: one binary, two roles, one database.

```
orbit serve (hetzner, control)            orbit worker (oci, transmission, ...)
  API, scheduler (leader lock), ticket      claims runs its capabilities cover,
  loop, config sync (files -> database),    executes them from the specs in the
  outbox delivery, actions, reaper,         database, writes every result to the
  builds, and executors of its own          database; nothing else
                 \                         /
                  orbit-db (Postgres, over the tailnet with TLS)
```

- **Capabilities.** An agent's spec names what a host must have (`needs: [docker-network:hetzner, sure]`); a host advertises what it has (`ORBIT_CAPABILITIES`, a list; `*` is everything, control's default). `Store.ClaimRunFor` claims a run only when its agent's current spec needs a subset of the host's, reading `needs` from the spec JSON in `agent_specs`, so an agent defined by any writer of that table is covered. An agent with no needs runs anywhere. Build runs (they need the GitHub App and sandboxd) are claimed only by control.
- **Specs from the database.** control records every loaded `orbit.yaml` (`deployment_specs`, the one in force in `deployment_current`) beside the agent specs it already recorded. A worker reads no file: before each run it rebuilds the configuration from those rows (`spec.FromStored`), so it runs the spec the claim's needs were checked against, with the SHA control computed. A worker waits until the schema is current (control migrates) and a deployment is recorded.
- **Delivery and actions stay on control.** A worker holds no channel credential and no approval secret, so a worker's engine (`Outbox`) records each notification in `outbox` instead of sending it; control sends it every `outboxEvery` (5 s), adding the run link or the signed approval link, and records each attempt in `deliveries` as for its own runs. Actions a worker's run proposed are rows like any other; control's action loop executes them. Each row is sent once; one a dead control claimed is reclaimed after `store.OutboxReclaimAfter`.
- **Sessions across hosts.** After a session agent's run, before the run is recorded as succeeded, and after any hand-back, the transcript (`<id>.jsonl` and `<id>/` from the CLI's projects directory, as a gzipped tar) goes to `session_transcripts`; before a resume or a recovery, a host without it writes it back under its own workdir's key (`engine/sessionstore.go`). Postgres rather than Garage: every host that runs agents already holds the database credential and needs nothing more, the write is ordered before the run's outcome so the next claim anywhere finds it, and it is backed up with every other row. A session id is stored once (every resume forks); an archive over `MaxSessionArchive` (64 MiB) stays on its host. A lost attempt (a crash) saved nothing, so its run starts over elsewhere, saying why.
- **One subscription is shared**, so the fleet adds reach (a source only one host can see, a browser, a residential IP), not capacity.

## Upgrades

orbit is replaced by starting a new instance beside the old one and
draining the old one, so nobody sees the switch and no model work is paid
for twice. The rollout contract is in docs/agents/deployment.md.

- **Two instances at once** share the database and the `/data` volume, and
  nothing else. Claims are split by `FOR UPDATE SKIP LOCKED` and
  `runs_one_running_lane`; one scheduler leads by advisory lock and the other
  takes over within its 30 s retry when the leader exits; each polls its own
  config mount and reloads; actions are claimed one at a time, and an
  action is failed as orphaned only after `engine.OrphanAfter` executing, so
  neither instance fails the other's. Metrics are per instance except
  `orbit_queued` and `orbit_pending`, read from the shared database.
- **Schema in the overlap.** The new instance migrates at start while the old one keeps running, so every migration is expand-only (store.md). `orbit preflight` checks that against the live database before a rollout starts the new image, and CI runs the previous release on each change's schema (`deploy/compat.sh`).
- **Readiness.** `/readyz` is 200 while the instance takes new work, 503
  while it drains or cannot reach the database; `/healthz` stays liveness
  (the image's HEALTHCHECK). traefik routes by `/readyz`.
- **Drain** (SIGTERM, `orbit drain`, or `POST /api/v1/drain` on the
  internal listener): stop claiming runs and approved actions, report not
  ready, keep serving the API and open streams, let every run held here
  finish under its own timeout, then close live processes (between turns,
  since none is running) and exit. The cap, `ORBIT_DRAIN_MAX`, defaults to
  `engine.DrainMax`: the longest agent timeout plus `engine.DrainMargin`
  (5 minutes, for source fetches before the backend call and deliveries
  after it), so a run that can finish within its timeout does. The
  scheduler keeps leading through a drain (it only enqueues, and the ready
  instance claims). A second signal hands back at once.
- **Hand-back without paying twice.** A run stopped mid-flight (the cap, a
  second signal, a lost claim, a crash the reaper finds) is continued, not
  restarted: the next attempt resumes the interrupted attempt's Claude Code
  session with `--resume <id> --fork-session` (never writing into it), on
  the inputs that attempt read (restored from its artifacts, not refetched),
  with a prompt that says it was interrupted and to continue
  (`engine/recover.go`). The session id is recorded per attempt as soon as
  the stream names it (`Session.Attached`), so a killed attempt still says
  where its work is. Every Claude Code run keeps its session for this,
  agents without `session` in a per-run HOME under `runs/<run>` (one
  workdir per run, not per attempt: the CLI files a session under its cwd),
  removed when the run ends. It starts over, with the reason in
  `recover_skipped`, when the interrupted attempt ran on another backend or
  spec, named no session, or its transcript is not on the volume.
- **What an interruption cost.** On SIGTERM Claude Code saves its running
  session total in the transcript (a `cost-state` line) and restores it on
  resume; on SIGKILL it saves nothing, and that spend is unknown
  (`runner.TranscriptCost`, verified on 2.1.316). Each attempt records its
  share (`run_attempts.cost_usd`) and the session's total; a run's
  `cost_usd` is every attempt's.
- **The stream follows it.** An attempt that resumed the previous one's
  session continues its journal timeline (the same `epoch`); a client keeps
  what it showed (docs/api.md).
- **Configuration** reloads when the directory's content changes. An invalid
  change is refused and the previous configuration kept
  (`orbit_config_valid` = 0).

## The build sandbox (sandboxd)

A build run's model writes files and runs commands, so it runs in a
container that holds nothing worth taking. Three pieces, all in this
repository and all built; build runs use them (below, and tickets.md):

```
orbit (hetzner) --internal network, bearer token--> orbit sandboxd (hetzner, holds the docker socket)
                                              -> build container (deploy/build.Dockerfile)
                                                 on the internal network orbit-sandbox
                                                    -> orbit egress (CONNECT proxy) -> allowlisted hosts
                                                    -> orbit credproxy (ANTHROPIC_BASE_URL) -> api.anthropic.com, the real credential added
```

- **`orbit sandboxd`** (`internal/sandboxd`) is the only orbit process with
  a docker socket. It serves four operations (`internal/sandbox`: create
  from a workdir tar, attach a process, read the workdir back as a tar,
  destroy) and builds every docker request itself from fixed options
  (`options.go`, whose test pins the whole create document): the image by
  digest, uid 10001, no capabilities, `no-new-privileges`, read-only root,
  `/work` (a fresh volume) and `/tmp` (tmpfs) the only writable paths,
  memory, CPU, PID and lifetime limits, the internal network, no mounts of
  the host. A caller chooses the tar's content, a run id label, an argv and
  the backend credential, nothing else: an unknown request field is
  refused. Every operation refuses a container without sandboxd's label, so
  the API cannot reach another container on the host. After creating, it
  inspects the container and refuses one the daemon did not make as asked.
  A container's PID 1 is `sleep <lifetime>`, so its processes end at expiry
  whatever sandboxd does; a reaper removes expired ones after a grace for
  the readback.
- **Tars both ways are rewritten** (`tarfilter.go`): files, directories and
  links only (no devices or FIFOs), names inside the workdir, ownership set
  to the sandbox uid on the way in and dropped on the way out, permission
  bits only, no extended attributes, a size and entry cap. The readback is
  spooled first, so a refusal is a clean error, never a truncated tar.
- **`orbit egress`** (`internal/egress`) is the container's only route out:
  a proxy on the internal network and one other, logging every decision. The build profile tunnels to exact
  hostnames on port 443 at public addresses, only for TLS naming that host, and
  refuses to load an allowlist that names GitHub; the workstation profile is the open web (any public host and port but SMTP's, plain HTTP too) minus deny lists of hosts and networks. Neither dials a private, loopback, link-local or tailnet address.
- **`orbit credproxy`** (`internal/credproxy`) is the CLI's only route to its backend: every sandbox has `ANTHROPIC_BASE_URL` pointing at it on the internal network. At attach, sandboxd replaces the backend credential a caller sends with a token: the credential sealed (AES-256-GCM) under `ORBIT_CREDPROXY_KEY`, which only sandboxd and the proxy hold, with the container's address on the sandbox network and its expiry. The proxy opens it, requires the request to come from that address before then, and forwards only `POST /v1/messages` and `/v1/messages/count_tokens` to `api.anthropic.com` with the real credential in the header the CLI used (`Authorization` for a subscription token, `x-api-key` for an API key), streaming the answer back as it arrives. A sandbox therefore never holds the credential; seen against the real CLI in tickets.md (Credential proxy: what the CLI does).
- **Attach** is an HTTP upgrade: orbit writes the process's stdin and
  half-closes it; sandboxd sends framed stdout, stderr and a final exit
  status (`protocol.go`). Claude Code's stdio permission protocol (tickets.md,
  Tool policy) rides on it.

What this guarantees and what it does not: docs/agents/security.md. How it
is deployed: docs/agents/deployment.md.

## Tickets: the build runner and the ticket loop

[tickets.md](tickets.md) is the design; this is where it lives.

```
ticket approved --ticket loop (internal/tickets)--> build run (trigger build) in the queue
  -> executor claims it -> engine (build.go) -> internal/build:
       clone (contents:read token) -> sandbox create (tar) -> attach: claude -p ... --permission-prompt-tool stdio
       <- can_use_tool requests answered by build.Policy in orbit's process, each a build_decisions row first
       -> archive (tar) -> diff policy -> commit as orbit-kensac[bot] -> push orbit/<id> (deliver token) -> PR
  -> FinishBuild: run succeeded and ticket review, one transaction
ticket loop, every 30 s: checks green -> github:merge proposed (approve for orbit itself)
  (a race: every lane built on orbit/<id>-<lane>, scored in code, a person's ticket:pick, then as above)
  -> action loop executes it -> merged -> done (on deploy, for orbit)
```

- **`internal/build`** is deterministic code around one model call: the
  tool policy (`policy.go`, `shell.go`: a Bash command is parsed with a
  shell parser, never matched with a regex), the stdio permission prompt
  (`protocol.go`, spoken through `internal/claudeproto`), the diff policy (`diff.go`), and the run itself
  (`runner.go`). It holds the GitHub tokens; the sandbox holds only a credential proxy token.
- **`internal/engine/build.go`** turns a claimed build run into a job from
  the ticket, its approved plan and the configuration in force, records
  decisions, sandboxes, events and artifacts under the attempt's claim,
  hands the run back on a drain, and moves the ticket with the run.
- **`internal/tickets`** is the loop: no state of its own, every decision
  from the store and GitHub at that moment, each transition guarded.

## Not built yet

The direction, so changes now do not close it off:

- **Tickets, what is left**: webhook events in place of the loop's polling ([tickets.md](tickets.md)); agents already take webhook triggers (Reliability, Webhooks).
- **Gmail drafts and Calendar events** as actions (`gmail:draft`, `calendar:create`): orbit holds no Google credential, so they wait for the connectors work, where orbit holds a connector's credential and an action rule gates its writes.
- **The workspace**: an IDE in the portfolio chat over files, terminals and git in a conversation's snapshot or an agent's box ([workspace.md](workspace.md)).
