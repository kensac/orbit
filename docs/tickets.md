# Tickets and the build agent

A ticket is a request or a question a person files with orbit. It is
discussed with an agent in its own thread; a request ends in a plan the
person approves, a build run that implements it in a sandbox, a pull request
orbit opens, and a merge orbit performs once every required check is green.
In the fleet a merge is a deploy, so the merge is gated by code, not by the
model.

This document is the design. What exists today:

| piece | state | where |
|---|---|---|
| `tickets` (one per conversation), `ticket_events`, `ticket_plans`; guarded transitions; receipts for filing and approving; a message in a ticket's conversation reopening or refusing per the ticket's state | **Built**, unit-tested | `internal/store/tickets.go`, `migrations/00005_tickets.sql` |
| the ticket API below; the person from the verified token; `ticket_id` on a conversation | **Built**, unit-tested | `internal/api/tickets.go` |
| App JWT, per-repository down-scoped tokens, revocation, git auth by environment | **Built**, tested against a fake GitHub | `internal/github/app.go` |
| `github:merge`: the checks gate (with the Open decisions defaults), the scrub, the pinned squash merge | **Built**, tested against a fake GitHub | `internal/github/merge.go`, `internal/actions` |
| the `github:` block in `orbit.yaml` | **Built** | `internal/spec` |
| the `plan` output field and `ticket.json` input; a plan stored with its run's outcome | **Built**, unit-tested | `internal/engine/ticket.go`, `store.RecordOutcome` |
| the `build:` section of `orbit.yaml` | **Built**, unit-tested | `internal/spec/build.go` |
| the build runner: clone, sandbox, the CLI under orbit's permission handler, readback, diff policy, commit, push, PR; decisions and sandboxes recorded per attempt | **Built**; tested against a fake sandboxd and a fake GitHub, with a real git remote; the permission protocol **seen** against the real CLI 2.1.316 (`TestRealCLIPermissionProtocol`); the whole run against a real sandboxd and docker in CI (`TestIntegrationBuild`) | `internal/build`, `internal/engine/build.go`, `internal/store/builds.go`, `migrations/00007_builds.sql` |
| the ticket loop: build enqueue, PR and checks polling, proposing the merge, fix attempts with the failure report, done on deploy, reconciliation, closing an abandoned ticket's PR, the self-hosting rules | **Built**, tested against a fake GitHub and real Postgres | `internal/tickets` |
| review comments; `skipped` as a run status for gates | not built | specified below |

Nothing has run against the real GitHub API yet; the fake
(`internal/github/githubtest`) follows GitHub's documented shapes. No
build has run with a real model inside a real sandbox: CI runs the
sandboxed build with a stand-in CLI (no credential there), and the real
CLI is checked on a developer machine without the sandbox.

## Principles applied here

- **Code first, model last.** The model writes a plan and writes code. Code
  does everything else: state transitions, minting credentials, the checkout,
  the diff, the commit, the push, the PR, the checks gate, the merge, the
  scrub of the PR body, the reconciliation after a crash.
- **The model never holds a credential it does not need.** A build run's
  model process holds only its own backend credential. It never sees a
  GitHub token: the harness clones, commits, pushes and merges.
- **Every write is an action with a rule.** Opening a PR is the harness
  delivering a build's result; merging is the `github:merge` action, denied
  unless an agent's spec names it.
- **Never delete stored data.** A ticket's transitions, plans, runs and
  actions are kept. A rebuilt PR is a new build run and new rows.
- **Ticket text is data, not instructions.** Only an approved plan turns a
  ticket into work (below).
- **No agent writes to GitHub or dispatches work itself.** There is no CLI
  or tool in any run that files tickets, comments, pushes or starts another
  agent; each of those is an orbit action with a rule, or the harness.

## Data model

A ticket is a **conversation with a lifecycle**. orbit's conversations
(an agent's many threads, each with its own inbox, open questions, the
thread a run is shown, and Claude Code session; `docs/api.md`) already give
a ticket its discussion: the person's messages are `messages` rows in the
ticket's conversation, the agent's replies are its `runs` (their `report`
and `questions`), and anything the agent wants done is an `actions` row.
Nothing about a thread is built twice. What tickets add is the lifecycle:

| table / column | what it holds | why it is not an existing table |
|---|---|---|
| `tickets` | one row per ticket: kind, title, the agent discussing it, the target repository, status, the approved plan, the branch, PR and merge commit, and `conversation_id` (unique: one ticket per conversation) | the lifecycle has states no conversation has |
| `ticket_events` | every transition: from, to, who, when, detail | the history of a ticket is a record, never overwritten (a `tickets.status` alone would lose it) |
| `ticket_plans` | every plan version an agent proposed, and which one was approved | a plan is approved by version; a later plan must never inherit an earlier approval |
| `ticket_review_comments` | line comments on a PR's diff and the message that delivered them | a comment is anchored to a file and lines, and delivered exactly once (Lifecycle) |

**Scoping** is the conversation's: two tickets with the same agent never see
each other's words, and ticket work never touches the agent's other
conversations. Memory, schedules, rules and `runs_one_running` stay per
agent, so an agent's tickets queue behind each other rather than race on
its memory.

**Messages to a ticket** go through the one conversation message path
(`POST /conversations/{id}/messages`), which knows about tickets: a message
to a `planned` ticket reopens it (the person is discussing the plan, not
approving it), and a ticket that is `merged`, `done` or `abandoned` takes no
more messages (409). There is no second message route that could disagree.

### Ticket text is untrusted

A ticket's title, body and messages are written by a person, but may quote
anything (a pasted log, an issue from elsewhere, a web page), and once
tickets come from webhooks they may be written by someone other than the
owner. orbit therefore treats ticket text as source data, the same as a
fetched source:

- it reaches the model as an input artifact (`ticket.json`: title, body,
  the thread's messages with their authors), never spliced into the system
  contract; the prompt names it as data the agent must not take
  instructions from, wrapped in a delimiter whose occurrences inside the
  text are escaped, with control characters escaped and the length capped;
- the author of every message is recorded, and only messages from the
  owner (the authenticated person, `infra-admins`) can approve a plan; a
  ticket or message from anyone else can be discussed but never approved
  by its own text;
- the build run's instructions are the **approved plan**, which a person
  read and approved by version, plus the ticket text as data. Nothing a
  ticket says can widen the build's tools, repository, commands or paths:
  those are configuration.

### Ticket kinds and states

`kind` is `question` or `request`. A question is answered in its thread and
closed; a request goes through the whole lifecycle.

```
                 question: open -> done | abandoned

request:
  open -> planned -> approved -> building -> review -> merging -> merged -> done
   ^         |                     |    ^     |  |       |          ^
   |         v                     v    +-----+  |       v          |
   +------ open (plan revised)   failed  (fix) failed  failed       |
   +------------------------------ failed (person reopens to discuss)
  approved <- failed (person retries the build with the same plan)
  review, failed -> merged (reconciliation: GitHub says the PR merged)
  any state before merged -> abandoned
```

| state | meaning | left by |
|---|---|---|
| `open` | being discussed; the agent may propose a plan | the agent proposing a plan (`planned`), a person abandoning it |
| `planned` | a plan version waits for the person | the person approving that version (`approved`) or replying, which reopens it (`open`) |
| `approved` | the approved plan waits for a build run | the harness enqueuing the build run (`building`) |
| `building` | the build run is working in its sandbox | the harness opening the PR (`review`), or the build failing (`failed`) |
| `review` | the PR is open; checks are running | the gate seeing every required check green (`merging`), a check failing (a fix, `building`, or `failed` once `tickets.MaxFixAttempts` fixes are spent), the PR merged by a person on GitHub (`merged`) |
| `merging` | a `github:merge` action exists | the action executing (`merged`), failing (`failed`), or being rejected (`failed`) |
| `merged` | the PR is merged; for orbit itself, the new revision is not yet running | deployment confirmed (`done`) |
| `done`, `abandoned` | terminal | nothing |
| `failed` | a build, its checks or its merge failed; `error` says which | a person retrying (`approved`) or reopening (`open`), or abandoning |

Every transition is a guarded UPDATE in `internal/store` (`WHERE status =
ANY(<allowed from>)`) and inserts its `ticket_events` row in the same
transaction; zero rows is `ErrConflict`. The allowed edges are one table in
`internal/store/tickets.go` (`ticketEdges`), so the diagram above and the
code cannot disagree silently: a test walks the table.

Approving a plan names its version and succeeds only while that version is
the newest one and the ticket is `planned`. A person approving version 2
while the agent writes version 3 gets a 409, not an approval of a plan they
did not read.

## Lifecycle

1. **File.** `POST /tickets` opens the ticket's conversation (titled as the
   ticket), stores the ticket (`open`) and its body as the conversation's
   first message, and wakes a run there, in one transaction.
2. **Discuss.** Each person message on the ticket wakes a run scoped to it.
   The agent replies in its report, asks questions, and when it has enough,
   proposes a plan. A run in an `open` or `planned` request's conversation
   gets a `plan` field in its output contract (Markdown: what changes,
   which files, how it is tested, what could go wrong; required and empty
   when the run proposes none, so every backend's strict schema accepts
   it); a non-empty plan is stored as the next version in the same
   transaction as the run's outcome, moving the ticket to `planned`, and a
   ticket that moved on meanwhile keeps the run's outcome without the plan.
   A question ticket, and a run outside a ticket, never get the field. The
   run also gets `ticket.json` as an input: the ticket, its thread's
   messages with their authors, and its plans.
3. **Approve.** The person approves a plan version (API, or a signed link
   like action approvals). Replying instead reopens the ticket.
4. **Build.** The harness enqueues a build run (below). Its result is a diff
   orbit computes, never one the model reports.
5. **PR.** orbit commits the diff to `orbit/<ticket-short-id>` (the first
   eight hex characters of the ticket's id: deterministic, never chosen by a
   model, so retries and reconciliation find the same branch) from the base
   commit the build started at, pushes that one ref, and opens the PR with a
   title and body built from the run's output and the approved plan, scrubbed
   of attribution.
6. **Checks.** The ticket loop polls the PR's head commit (later: webhook
   events) and moves the ticket to `merging` when the merge gate passes, or
   to `failed` when a required check concluded anything but success.
7. **Merge.** Moving to `merging` proposes a `github:merge` action on the
   build run, with the rule the build agent's spec gives it. With `approve`
   the person gets an approval notification as for any action; with `auto`
   it executes at once. The executor re-checks everything itself (below).
8. **Done.** After the merge, orbit confirms the PR is merged on GitHub
   (`merged`), and for orbit's own repository waits until it is running the
   merge commit (`done`; see Self-hosting). Other repositories go straight
   to `done`: their fleet CD deploys on merge.

**Failure and abandon paths.** A build that times out, edits nothing, or
fails its policy checks fails the ticket with the reason. A failing check
fails the ticket with the check's name and URL. A person can retry
(`failed -> approved`, a new build run and a new commit on the same branch),
reopen to discuss (`failed -> open`), or abandon from any state before
`merged`. Abandoning a ticket with an open PR closes the PR (orbit, with the
build token) and keeps the branch.

**Fixing failed checks.** When a required check fails, code collects what
failed: each failing check's name, conclusion and URL, and the last 16 KB
of its log (GitHub Actions job logs, read with the `checks: read`,
`actions: read` token; the log's storage redirect is followed by hand,
with no credential), at most five checks. A fix run (`ticket_builds.purpose
= fix`, `review -> building`) gets exactly that, as data, with the approved
plan, starts from the PR's head and commits on top of it; its prompt says
"make these checks pass without changing the plan". After
`tickets.MaxFixAttempts` (2) fixes on one PR the ticket fails with the
checks' names and URLs, for a person.

**Review comments.** A person can review the PR's diff in orbit: line
comments `{path, start_line, end_line, body}` are stored as
`ticket_review_comments` rows, and submitting the review sends them to the
ticket's thread as one structured message (each comment with its file and
line range), which wakes the next run. A comment records the message that
carried it; it counts as delivered once the run that consumed that message
succeeded, and a failed run hands the message back like any other, so a
comment is never lost and never sent twice. An edited comment is a new row.

**Prechecks are gates.** Before any build or retry run calls the backend,
tested Go predicates decide whether there is work: the PR already merged,
the branch already at the approved plan's result, the checks already green.
A run a gate ends is recorded with its own status, `skipped`, naming the
gate, not as `succeeded`: a run that judged nothing must be distinguishable
from one that did.

**Reconciliation.** A crash can separate a side effect from its row: the
merge succeeded on GitHub but `FinishAction` never ran, so the action is
failed as orphaned. The ticket loop is idempotent and reads GitHub's state:
a ticket in `merging` or `failed` whose PR is merged moves to `merged` with
the merge commit. The same holds for a pushed branch whose PR was never
opened (the build's next attempt opens it from the commit, without the
model) and a PR opened twice (GitHub refuses a second open PR for a head,
and orbit then takes the existing one). The loop (`internal/tickets`,
every `tickets.TickEvery`, 30 s, under an advisory lock so one instance
reconciles at a time, stopped by a drain) holds no state between ticks:

| ticket | GitHub or the store says | the loop |
|---|---|---|
| `approved` | | enqueues the build (waits while the repository has one building) |
| `building` | the build run failed or was cancelled without moving it (the reaper ran out of attempts) | `failed` with the run's error |
| `review` | PR merged / closed / head moved off what orbit built | `merged` / `failed` / `failed` |
| `review` | checks green at the head orbit built | proposes `github:merge` (`merging`); waits while another ticket of the repository merges |
| `review` | a check failed | a fix (`building`), or `failed` after `MaxFixAttempts` |
| `merging` | the action executed | `merged` with its merge commit |
| `merging` | the action failed, was rejected or orphaned | `merged` if the PR merged anyway, else `failed` with why |
| `failed` (reached merging) | the PR merged | `merged` |
| `merged` | another repository / orbit runs the merge commit or a descendant | `done` |
| `abandoned` with an open PR | | closes the PR, keeps the branch |

## API

Under `/api/v1`, with the conventions of [api.md](api.md). The person is
the verified authentik token's: a ticket's `author`, a message's author and
a plan's approver are the token's `preferred_username`. Body `author` and
`by` are read only with the operator token on the internal listener, as for
messages and decisions. Creating a ticket and approving a plan take an
optional `client_request_id` (posting takes `client_message_id`, as any
conversation message): a retry with the same id returns
the first result (the `receipts` table, keyed by the ticket's agent); the
same id for a different request is a 409.

| method | path | body | result |
|---|---|---|---|
| `POST` | `/tickets` | `{"kind": "request", "title": "...", "body": "...", "agent": "orbit", "repo": "kensac/orbit", "wake": true, "client_request_id": "..."}` | `201 {"ticket_id", "conversation_id", "message_id", "run_id"}` |
| `GET` | `/tickets?status=&agent=&before=&limit=` | | `{"tickets": [...]}`, newest first |
| `GET` | `/tickets/{id}` | | `{"ticket", "plans", "events"}` |
| `POST` | `/tickets/{id}/plans/{version}/approve` | `{"client_request_id"}` | `{"ok": true}`; 409 unless it is the newest plan of a `planned` ticket |
| `POST` | `/tickets/{id}/abandon` | `{"reason"}` | `{"ok": true}`; 409 once merged or terminal |
| `POST` | `/tickets/{id}/retry` | | `{"ok": true}`; `failed -> approved` |
| `POST` | `/tickets/{id}/reopen` | | `{"ok": true}`; `failed -> open` |

A ticket's thread and its messages are its conversation's:
`GET /conversations/{conversation_id}/thread` and
`POST /conversations/{conversation_id}/messages`. In the agent's
conversation list a ticket's conversation carries its `ticket_id`.

`repo` is required for a request and must be one of `github.repositories`
in `orbit.yaml`. A ticket in the API:

```json
{"id": "uuid", "kind": "request", "title": "...", "agent": "orbit",
 "conversation_id": "uuid",
 "repo": "kensac/orbit", "status": "review", "author": "kanishk",
 "approved_plan": 2, "branch": "orbit/3f2a9c1e", "pr_number": 41,
 "head_sha": "...", "merge_sha": null, "error": null,
 "created_at": "...", "updated_at": "..."}
```

## GitHub access: one App, per-run tokens

orbit reaches GitHub as one GitHub App installed on the owner's repositories.
The App's private key is in orbit's environment and is used by the harness
only (`internal/github`):

1. **App JWT.** RS256 over `{"iat": now-60s, "exp": now+9m, "iss": <app
   id>}`, signed with the private key. Ten minutes is GitHub's maximum; the
   backdated `iat` absorbs clock skew. Standard library only
   (`crypto/rsa`, `crypto/sha256`, `encoding/base64`): a JWT with one fixed
   algorithm and three claims is thirty lines, and a JWT library would bring
   algorithm negotiation orbit must then make sure it never uses.
2. **Installation.** `GET /repos/{owner}/{repo}/installation` with the JWT
   finds the installation for the target repository.
3. **Token.** `POST /app/installations/{id}/access_tokens` with
   `{"repositories": ["<repo>"], "permissions": {...}}` returns a token
   valid for at most an hour, limited to that one repository and those
   permissions, whatever the installation itself was granted. GitHub
   refuses a permission the App does not have.
4. **Revoke.** `DELETE /installation/token` when the operation is done, so
   a token outlives its use only if orbit dies mid-operation, and then by at
   most an hour.

The permission sets are named in code, one per use:

| use | permissions |
|---|---|
| reading run (clone a repository for an agent to read) | `contents: read` |
| build delivery (push `orbit/*`, open the PR, close it on abandon) | `contents: write`, `pull_requests: write` |
| merge (read the PR and its checks, update the body, merge) | `contents: write`, `pull_requests: write`, `checks: read`, `statuses: read` |
| failure report (read failing checks and their job logs) | `checks: read`, `actions: read` |

`metadata: read` is implicit in every token. No set includes `workflows`, so
a pushed change to `.github/workflows/` is refused by GitHub itself: a build
cannot change what CI runs with repository secrets.

**Git without a credential on disk.** orbit runs git with the token in the
environment only (`GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_0`/`GIT_CONFIG_VALUE_0`
setting `http.https://github.com/.extraheader` to `Authorization: Basic
base64(x-access-token:<token>)`), so the token is never written to
`.git/config`, never in a URL, and never in the archive handed to a sandbox.

## The build run

A build run is a run of a build agent: a separate trust tier, declared in
its spec, executed by a different runner. It is the only kind of run whose
model may write files and run commands, so everything it can touch is
disposable.

```
ticket approved
  -> the ticket loop: runs row (trigger 'build') and ticket_builds row, ticket
     approved -> building, in one transaction (one building ticket per
     repository: tickets_one_building)
  -> an executor claims it like any run; the engine hands it to internal/build
  -> orbit: mint contents:read token, clone the repo at the default branch's
     head (base_sha), revoke the token
  -> orbit: tar the checkout (without credentials), ask the sandbox launcher
     for a container with that tar at /work
  -> orbit: start the CLI inside it, speak its stream-json protocol over the
     container's stdio, answer every permission request with code (below)
  -> model finishes with its build output (summary, PR title, PR body, notes)
  -> orbit: fetch /work back as a tar, destroy the container
  -> orbit: in its own clone, replace the tree with the fetched files, compute
     the diff against base_sha, enforce the diff policy, store the diff as an
     artifact
  -> orbit: mint the build-delivery token, commit, push orbit/<ticket>,
     open (or update) the PR, revoke the token
  -> ticket: building -> review, with the run's outcome (store.FinishBuild)
```

The build run is a run of the agent `build.agent` names. It is in the
ticket's conversation when that agent is the ticket's, and in the build
agent's default conversation otherwise (a run is always in a conversation
of its own agent). It reads no messages and writes no memory.

**Attempts.** A build run is claimed, heartbeated and fenced like any run:
its permission decisions (`build_decisions`) and its sandboxes
(`build_sandboxes`) are written only by the attempt holding the claim, and
an attempt that loses it writes nothing more. Before creating a sandbox an
attempt destroys every sandbox an earlier attempt of the run recorded and
did not see destroyed, so an interrupted build never leaves two; one is
destroyed, not reattached, because the CLI's conversation lived in the
attach stream that ended with its attempt. A draining instance hands a
build back instead of starting its sandbox. An attempt that finds the
branch already at a commit naming its run (`Orbit-Run: <run id>`, written
by an earlier attempt that died after the push) does not call the model:
it reopens the PR from that commit's message (a gate, recorded as the
`orbit.gate` event; until runs have a `skipped` status the run is
`succeeded` with the gate in its report).

### Isolation: one container per build run, started by a launcher

orbit's own container never gets the docker socket, not even through a
filtering proxy. A socket proxy that permits `POST /containers/create` lets
the caller ask for `Privileged`, a bind mount of `/`, the host network or
the host PID namespace: whoever can create containers is root on the host.

**Built** (`internal/sandboxd`, `internal/egress`, `internal/sandbox`; not
yet called by a build run). A small launcher, `orbit sandboxd` (the same
image, a separate compose service), holds the socket and exposes the four
operations orbit needs, with every security-relevant option fixed in its
code (`internal/sandboxd/options.go`):

| request (orbit -> sandboxd) | what sandboxd does |
|---|---|
| `POST /v1/sandboxes?run_id=` with the workdir tar as the body | creates a container from the build image pinned in sandboxd's own configuration, unpacks the tar (sanitized) into a fresh volume at `/work`, starts it; returns an id |
| `POST /v1/sandboxes/{id}/attach` `{argv, env}` | runs argv in it (env: the backend credential only), streams stdio both ways over an upgraded connection, ends with the exit status |
| `GET /v1/sandboxes/{id}/archive` | `/work` as a tar (sanitized) |
| `DELETE /v1/sandboxes/{id}` | kills and removes the container and its volume |

Fixed, never taken from a request: the image digest; uid 10001; `CapDrop:
ALL`; `no-new-privileges`; a read-only root filesystem with a tmpfs `/tmp`
and the `/work` volume as the only writable paths; memory, CPU and PID
limits; no bind mounts, devices or extra privileges; and the network. A run
id label and a maximum lifetime let sandboxd reap containers orbit
abandoned. The only environment it passes is the backend credential orbit
sends (see below) and the proxy settings. sandboxd authenticates orbit with
its own token (`ORBIT_SANDBOX_TOKEN`) on its tailnet address. What each
option guarantees, and what it does not: `docs/agents/security.md`.

**Network.** The build container sits on an `internal` docker network with
no route out (sandboxd refuses to start on one that is not internal). Its
only egress is `orbit egress`, an allowlisting CONNECT proxy in its own
container on that network and one other (`HTTPS_PROXY`), which allows the
backend's API host and the package registries the repository's build needs
(for Go: `proxy.golang.org`, `sum.golang.org`), on port 443 only, to public
addresses only, and nothing else. It refuses to load an allowlist naming
GitHub: the checkout is already inside, and a sandbox that cannot reach
GitHub cannot push even with a token it found.

**Where it runs.** sandboxd can run on any docker host orbit reaches over
the tailnet; the default is oci (Open decisions). oci (the ARM CI host, already running CI runners and nothing
public) is the better home than hetzner, which holds every other secret in
the fleet: a container escape there reaches far less.

**Alternatives considered.** Rootless podman inside orbit's container needs
user namespaces and `/dev/fuse` that docker's default seccomp profile
refuses, and nesting weakens both layers. gVisor (`runsc`) as the build
container's runtime is a hardening of this design, not a replacement:
sandboxd sets the runtime when the host has it. A microVM per run
(Firecracker) is stronger and far more machinery than a personal fleet
needs today.

### No orbit secrets inside

The container receives: the checkout (no `.git/config` credentials), the
CLI command line, the prompt on stdin, and the backend credential
(`CLAUDE_CODE_OAUTH_TOKEN`), which the CLI needs to reach its own backend
(AGENTS.md rule 2). It never receives a GitHub token, the App key, orbit's
database URL, the API token, or any source or channel secret.

The backend credential is still reachable by code the model runs: `go test`
executes whatever the model wrote, as the same user as the CLI, and can read
`/proc/<cli pid>/environ`. Two measures, in order of preference:

1. **Keep the token out of the container**: point the CLI at a credential
   proxy in sandboxd (`ANTHROPIC_BASE_URL`) that adds the real credential on
   the way out, and give the container a placeholder. To be verified against
   the real CLI with a subscription token before it is relied on.
2. **Catch it on the way out**: the diff policy refuses any diff containing
   the exact credential value (orbit knows it) or a secret pattern, before
   anything is committed, since orbit's repository is public.

### Tool policy inside the sandbox

The build CLI runs with:

```
claude -p --input-format stream-json --output-format stream-json --verbose
  --permission-prompt-tool stdio --permission-mode default
  --tools Read,Glob,Grep,Edit,Write,Bash
  --strict-mcp-config --setting-sources "" --settings <orbit's settings>
  --json-schema <the build output schema>
```

How the CLI decides a tool call, from the Agent SDK that wraps it
(`@anthropic-ai/claude-agent-sdk@0.3.316`, the release matching the CLI
pinned in `deploy/cli`):

- `--permission-prompt-tool stdio` is what the SDK passes when a host
  supplies `canUseTool` (`core.mjs`, `ProcessTransport` argument builder:
  `canUseTool` sets `--permission-prompt-tool=stdio` and refuses to combine
  with `permissionPromptToolName`), together with `--input-format
  stream-json`. The CLI then writes a `control_request` with `request:
  {subtype: "can_use_tool", tool_name, input, ...}` on stdout
  (`SDKControlPermissionRequest`, `sdk.d.ts`) and waits for a
  `control_response` carrying `{behavior: "allow", updatedInput}` or
  `{behavior: "deny", message}` (`PermissionResult`). orbit is Go and does
  not use the SDK; it speaks this protocol directly (`internal/claudeproto`), which the SDK is the readable reference for.
- `canUseTool` is asked only when the decision is "ask": `allowedTools`
  entries "execute automatically without asking" (`Options.allowedTools`),
  and in `dontAsk` mode an ask is denied without asking anyone
  (`PermissionMode`). So the build run passes **no allow rules for Edit,
  Write or Bash** and uses `default` mode. That alone is not enough: in
  default mode the CLI runs commands it classifies as read-only without
  asking (seen on 2.1.316: `ls -la` ran with no request). orbit's settings
  therefore carry **ask rules** for `Bash`, `Edit` and `Write`, which send
  every call of those tools to orbit's handler (seen: `ls` and `git status`
  then each arrived as a `can_use_tool` request). Read, Glob and Grep
  inside the working directory stay allowed by the mode; outside it they
  are asked, and orbit denies them.
- Deny rules and `PreToolUse` hook denials win before `canUseTool` is
  consulted: "Denials that resolve before canUseTool runs: PreToolUse hook
  denies, deny-rule overrides of hook allow/ask decisions, and file-tool
  calls (Read, Edit, Write) refused by a path-scoped deny rule"
  (`SDKPermissionDeniedMessage`). orbit's `--settings` therefore also
  carries deny rules for what is never acceptable (`Bash(git push:*)`,
  `Bash(curl:*)`, `Bash(wget:*)`, `Bash(gh:*)`, `Bash(docker:*)`,
  `Bash(sudo:*)`, `Edit(.git/**)`, `Write(.git/**)`, `Edit(.github/**)`,
  `Write(.github/**)`, and reads of `/proc`, `/sys`, `/etc`, `/run`), as a
  second layer that holds even if orbit's handler had a bug. Seen on
  2.1.316: `curl`, a Write under `.git/` and one under `.github/` were
  refused without orbit being asked, and listed in `permission_denials`.
- The authoritative record of what was refused is the result's
  `permission_denials`; orbit stores it as the `orbit.permission_denials`
  event. Every decision its own handler made is a `build_decisions` row,
  written before the answer is sent: when the write fails the answer is a
  deny, so no allow is ever unrecorded (and an attempt that lost its claim
  can record, and so allow, nothing). Each is also an `orbit.permission`
  event for the run's stream. If orbit's process dies, the attach stream
  ends, the CLI's stdin closes and no pending request is ever allowed.

orbit's handler (Go, outside the container, so the model cannot alter it) is
deterministic:

| tool | allowed when |
|---|---|
| `Edit`, `Write` | the path resolves inside `/work`, outside `.git/`, and outside the repository's protected paths (configuration; for orbit: `.github/**`, `deploy/**`) |
| `Bash` | the command parses (a POSIX shell parser, not a regex) into simple commands joined by `&&`, `\|\|`, `;` or `\|`, with no command or process substitution, no backgrounding, redirections only to files inside `/work` or `/dev/null`, and every command's argv prefix on the repository's list (for orbit: `go build`, `go test`, `go vet`, `gofmt`, `make check`, `make fmt`, `make test`, `git status`, `git diff`, `git log`, `git show`, `ls`, `rg`) |
| anything else | never |

A denial's message tells the model what is allowed, so it adapts instead of
retrying.

orbit already runs a `PreToolUse` hook inside Claude Code (`orbit hook`,
`internal/harness/hook.go`: a path guard on the file tools). In a build run
that hook would run inside the sandbox, in the CLI's process tree, beside
code the model wrote and can run; it is a useful second layer there (it
sees each call before the permission prompt) but not the authority. The
decision that counts is the stdio permission prompt, answered by orbit's
process outside the container, which nothing inside can reach or alter. This list keeps the run on task and makes its intent auditable; it
is **not** the security boundary. `go test` and `make check` run code the
model wrote, which can do anything the container allows. The boundary is
the container, its network, and the absence of secrets in it.

### What the CLI's own sandbox does and does not guarantee

The CLI has a sandbox setting (`SandboxSettings`, `sdk.d.ts`; bubblewrap and
seccomp on Linux). From its documented fields:

- It applies to **Bash commands** ("Run Bash commands inside the sandbox"),
  not to the CLI's in-process tools: `network.strictAllowlist` "is enforced
  for sandboxed commands only; in-process tools such as WebFetch are not
  gated by this setting". The build run grants no network tools.
- `credentials.envVars` with `mode: deny` "unsets the variable for sandboxed
  commands", which would keep the backend credential away from `go test`,
  but only with a fresh `/proc`: in an unprivileged docker container the
  sandbox needs `enableWeakerNestedSandbox`, documented as "the host /proc
  stays readable by sandboxed commands", which reopens the CLI's environment.
- With `failIfUnavailable: false` (the CLI default) a sandbox that cannot
  start is a warning and commands run unsandboxed; `excludedCommands` is "a
  convenience, not a security boundary".

So the CLI sandbox is defense in depth inside orbit's container, enabled
with `failIfUnavailable: true` and `allowUnsandboxedCommands: false` if it
starts in the build image, and never what a guarantee rests on.

### The diff policy and the commit

The model's `.git` inside the sandbox is discarded: orbit applies the
fetched tree to its own clone, so hooks, config or refs the model planted
never run anywhere. Before committing, code checks:

- something changed, and nothing outside the tree: everything named `.git`
  at any depth is dropped from the readback; a symlink pointing out of the
  repository or into `.git`, and a file written through a symlink, are
  refused, as are hard links, devices, new submodules and changed files
  over `build.MaxFileBytes` (1 MiB);
- no protected path changed (`.github/**` and `.gitmodules` always, plus the
  repository's `protected`);
- no added line contains the backend credential (matched exactly; the error
  never echoes it) or a credential pattern (Anthropic, GitHub, AWS and Slack
  token shapes, private keys);
- the diff is within `max_diff_lines` (added plus deleted).

This is where a write the handler never saw is caught: code the model ran
(`go test`) can write anything in `/work` without asking. The diff is
stored as the run's `diff.patch` artifact only once it passed, so a
refused one that held a credential is never stored.

The commit is authored by the App's bot user (`orbit-kensac[bot]`); its
message is the PR's title and body and the `Orbit-Run: <run id>` trailer. orbit pushes exactly one refspec, built from the ticket id:
`<sha>:refs/heads/orbit/<ticket-short-id>`. The code that builds refspecs
cannot produce anything but `refs/heads/orbit/`; a retry force-pushes that
same branch. The default branch should also be protected on GitHub
(require a pull request), with the App not on any bypass list, so even a
bug in orbit cannot push to it.

## Merge: the `github:merge` action

Params, shown to an agent like any action type:

```json
{"repo": "kensac/orbit", "pull_number": 41, "head_sha": "<the commit that was checked>"}
```

The validator requires `repo` to be in `github.repositories`. The executor
mints a merge token for that repository and refuses, with the reason as the
action's error, unless all of these hold at execution time:

1. The PR is open, not a draft, its base is the repository's default
   branch, its head is a branch `orbit/*` in the same repository (orbit
   merges only its own PRs), and its head is still `head_sha`. A commit
   pushed after the checks or the approval means the approval was for
   something else.
2. GitHub reports it `mergeable: true` (orbit re-reads while GitHub is still
   computing it, a few times, then refuses), and `mergeable_state` is not
   `dirty`, `blocked`, `behind` or `draft`.
3. **The checks gate.** The required checks are the union of the default
   branch's protection (`GET /repos/{o}/{r}/branches/{branch}`,
   `protection.required_status_checks`) and its rulesets (`GET
   /repos/{o}/{r}/rules/branches/{branch}`, rules of type
   `required_status_checks`). Each must be present on the head commit, as a
   check run (latest by name, and from the named app when the rule names
   one) or a commit status (by context), and must have concluded
   `success`; `skipped` or `neutral` does not satisfy a required check.
   With no required checks, every check run on the head must have
   concluded `success`, `skipped` or `neutral` and every commit status must
   be `success`, and there must be at least one that succeeded: a merge
   with no check at all is a deploy with no gate (see Open decisions).
4. The PR body, scrubbed of attribution (lines naming an AI tool as an
   author, such as `Co-Authored-By:` trailers for one or "generated with"
   footers), is written back to the PR first, and the squash merge is
   `PUT /repos/{o}/{r}/pulls/{n}/merge` with `merge_method: squash`, `sha:
   head_sha` (GitHub refuses if the head moved in between), the PR title as
   the commit title and the scrubbed body as the commit message, because a
   squash merge makes the body the commit message.

GitHub enforces branch protection on the merge call as well, so a required
check orbit could not see still blocks the merge (405). The executor
returns the merge commit's sha as its result. As for every action, it runs
at most once; an interrupted merge is failed for a person and reconciled by
the ticket loop from GitHub's state.

## Self-hosting: when the repository is orbit

Merging a PR to `kensac/orbit` builds `ghcr.io/kensac/orbit` and, once
hetzner's digest pin moves, replaces the process that did the merge. What
changes:

- **The merge rule is always `approve`** for orbit's own repository, whatever
  the agent's spec says (code, not configuration). Automatic merges to orbit
  stay off until a person decides otherwise in a reviewed change to this
  document and that code.
- **Changes to orbit's trust boundary need review.** A diff touching
  `internal/actions`, `internal/github`, `internal/runner`, `internal/harness`,
  the sandbox (`internal/sandbox`, `internal/sandboxd`, `internal/egress`),
  the tool policy (`internal/build`), `deploy/`, `AGENTS.md` or
  `docs/agents/security.md` is labelled as such in the PR body and in the
  merge action's reason, which its approval notification shows
  (`build.TrustBoundary`), so the person approving knows they are approving
  a change to what orbit may do.
- **Done means running.** A merged orbit ticket waits in `merged` until
  `orbit version` (the process's own revision) is the merge commit or a
  descendant of it (GitHub's compare says `identical` or `ahead`); only then
  is it `done`. A merge that never deploys stays visible. A process whose
  revision is not a commit (`unknown`, a local build) never marks one done.
- **The deploy restarts orbit mid-flow.** Runs requeue on shutdown already;
  the merge action and the ticket transition are separate steps, and the
  reconciliation above finishes whichever did not happen. One self-hosting
  ticket merges at a time: a second waits in `review` until the first is
  `done`, so two unrelated changes never ship in one restart nobody
  reviewed together (`tickets_one_merging`, one `merging` or `merged`
  ticket per repository).
- **orbit cannot fix a broken orbit.** If the new revision does not start,
  no orbit run can revert it; recovery is the person rolling hetzner's pin
  back. The build run's `make check` and the image build in CI are the only
  gates before that, which is why the merge stays `approve`.

## Open decisions

Recommended defaults, implemented as the defaults; the owner has not
confirmed them.

- **Which check conclusions pass.** Default: a required check passes only
  on `success`; with no required checks, `success`, `skipped` and
  `neutral` pass and anything else blocks. Owner to confirm.
- **Where sandboxd runs.** Default: oci (above). Owner to confirm.
- **Which PRs orbit merges.** Default: only its own `orbit/*` branches in
  the same repository, not Renovate's or anyone else's. Owner to confirm.
- **Keeping the backend credential out of the build container** (the
  credential proxy above). Open. **Precondition:** until it is verified
  against the real CLI, build runs target only `kensac/orbit`, whose
  diff is reviewed before every merge, and no other repository.

## Configuration

`orbit.yaml` gains:

```yaml
github:
  app_id: 5571920
  # The App's private key: PEM, or base64 of the PEM so it fits one line of
  # an env file. Read by orbit only; never passed to a model.
  private_key_env: ORBIT_GITHUB_APP_PRIVATE_KEY
  # Repositories tickets and github:merge may target.
  repositories: [kensac/orbit]
```

and, to build tickets (without it approved tickets wait):

```yaml
build:
  sandboxd: 100.123.116.85:8091        # host:port on the tailnet, or unix:/path
  token_env: ORBIT_SANDBOX_TOKEN       # the default
  agent: builder                       # whose runner, timeout and merge rule a build uses
  author: "orbit-kensac[bot] <351241810+orbit-kensac[bot]@users.noreply.github.com>"
  repositories:
    kensac/orbit:
      commands: [go build, go test, go vet, gofmt, make check, make fmt, make test,
                 git status, git diff, git log, git show, ls, rg]
      protected: ["deploy/**"]         # .github/** and .gitmodules always are
      max_diff_lines: 2000             # the default
```

Validated at load (`internal/spec/build.go`): the github block exists,
every repository is in `github.repositories` and, until the credential
proxy is verified, is `kensac/orbit`; the agent exists, uses a
claude-code runner and has a timeout of at most `spec.MaxBuildTimeout`
(50m, inside the sandbox's one-hour lifetime); commands are plain words;
protected paths are relative globs. The build's model and turn limit are
the agent's runner's `model` and `max_turns`; its credential is orbit's
`CLAUDE_CODE_OAUTH_TOKEN`, the only secret the sandbox receives. The egress
allowlist and the image are sandboxd's and egress's own configuration
(docs/agents/deployment.md).

An agent enables merges with `actions: {github:merge: approve}`.

## Deploying it

**The App** is `orbit-kensac` (App ID `5571920`; `orbit` was taken), owned
by `kensac` and installed on all of its repositories. Webhook inactive
(orbit polls). Repository permissions, exactly: Contents read and write,
Pull requests read and write, Checks read, Commit statuses read, Actions
read (job logs of failing checks), Metadata read. No Workflows, no
organization or account permissions. Events: none until event triggers
exist; then Check run, Check suite, Pull request and Status. Every token
orbit mints is still limited to one repository and one permission set.
Commits orbit makes are authored by `orbit-kensac[bot]`.

**The key** is in Infisical as `HETZNER_ORBIT_GITHUB_APP_PRIVATE_KEY`:
base64 of the PEM GitHub issued, on one line, so it renders into
`/root/.env` safely as `ORBIT_GITHUB_APP_PRIVATE_KEY`. orbit's loader
(`github.ParsePrivateKey`) takes the value as a PEM or as base64 of one,
decodes it, and parses PKCS#1 (`RSA PRIVATE KEY`, what GitHub issues) or
PKCS#8; `TestParsePrivateKeyAcceptsGitHubsFormats` holds all three forms.

**Wiring it** (hetzner-vps-config): in `compose.orbit.yml`, the `orbit`
service's `environment` gains

```yaml
      # The GitHub App's key (orbit-kensac). Read by orbit only; never given
      # to a model.
      - ORBIT_GITHUB_APP_PRIVATE_KEY=${ORBIT_GITHUB_APP_PRIVATE_KEY}
```

and `orbit/orbit.yaml` gains

```yaml
github:
  app_id: 5571920
  private_key_env: ORBIT_GITHUB_APP_PRIVATE_KEY
  repositories: [kensac/orbit]
```

An agent may merge only once its spec names the rule:
`actions: {github:merge: approve}`.

**Protect `main`** on repositories orbit may merge: require a pull
request and the CI check (`check` for orbit), and leave the App off the
bypass list.

**The build runner** needs sandboxd and egress on oci and the `build:`
section on hetzner: the steps are in docs/agents/deployment.md.
