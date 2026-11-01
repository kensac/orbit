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
| `tickets`, `ticket_events`, `ticket_plans`, `ticket_id` on messages and runs; guarded transitions; receipts for filing, posting and approving | **Built**, unit-tested | `internal/store/tickets.go`, `migrations/00004_tickets.sql` |
| thread scoping of every reader in the table below | **Built**, unit-tested (store and engine) | `internal/store`, `internal/engine/session.go` |
| the ticket API below, person from the verified token | **Built**, unit-tested | `internal/api/tickets.go` |
| App JWT, per-repository down-scoped tokens, revocation, git auth by environment | **Built**, tested against a fake GitHub | `internal/github/app.go` |
| `github:merge`: the checks gate, the scrub, the pinned squash merge | **Built**, tested against a fake GitHub | `internal/github/merge.go`, `internal/actions` |
| the `github:` block in `orbit.yaml` | **Built** | `internal/spec` |
| the check-conclusion defaults (Open decisions) | **Built** as defaults, unit-tested | `github.Gate` |
| the `plan` output field, the ticket loop (build enqueue, PR polling, proposing the merge, reconciliation), the build runner and sandboxd, review comments, the failure report, the self-hosting rules | not built | specified below |

Nothing has run against the real GitHub API yet; the fake
(`internal/github/githubtest`) follows GitHub's documented shapes.

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

A ticket's discussion is orbit's existing thread machinery: the person's
messages are `messages` rows, the agent's replies are `runs` (their `report`
and `questions`), and anything the agent wants done is an `actions` row.
What tickets add is a scope and a lifecycle, so three tables and two columns:

| table / column | what it holds | why it is not an existing table |
|---|---|---|
| `tickets` | one row per ticket: kind, title, the agent discussing it, the target repository, status, the approved plan, the branch, PR and merge commit | the lifecycle has states no run or message has |
| `ticket_events` | every transition: from, to, who, when, detail | the history of a ticket is a record, never overwritten (a `tickets.status` alone would lose it) |
| `ticket_plans` | every plan version an agent proposed, and which one was approved | a plan is approved by version; a later plan must never inherit an earlier approval |
| `ticket_review_comments` | line comments on a PR's diff and the message that delivered them | a comment is anchored to a file and lines, and delivered exactly once (Lifecycle) |
| `messages.ticket_id` | the ticket a message belongs to, or null for the agent's general inbox | a ticket's messages are its thread |
| `runs.ticket_id` | the ticket a run works on, or null | a run on a ticket reads that ticket's thread only |

**Scoping (Built).** A thread is either an agent's general thread (`ticket_id`
null) or one ticket's. Everything that reads or continues a thread is
scoped by it, taken from the run row, so two tickets with the same agent
never see each other's words and ticket work never swallows the general
inbox:

| what | scoped how |
|---|---|
| unread messages a run consumes (`StartRunContext`) | the run's `ticket_id` |
| open questions, and what closes one (`openQuestion`) | a reply or later message in the same thread |
| a question a run asks (`RecordOutcome`) | lands in the asking run's thread |
| the conversation a fresh run is shown (`Conversation`) | the run's thread |
| the session a run resumes (`LatestSession`) and the live process it feeds (`runner.Session.Key`) | the latest finished run of the same thread; the live key is the agent, or `<agent>/ticket/<id>` |
| a `continue` the run asks for (`EnqueueContinuation`) | inherits the run's `ticket_id` |
| the thread API (`Store.Thread`, its cursor paging) | one implementation, filtered by thread: `GET /agents/{name}/thread` is the general thread, `GET /tickets/{id}/thread` a ticket's |

`runs_one_running` (one running run per agent) and `runs_one_continuation`
stay per agent: an agent's tickets queue behind each other, which keeps its
memory free of races.

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
   ^         |                     |          |          |
   |         v                     v          v          v
   +------ open (plan revised)   failed     failed     failed
   +------------------------------ failed (person reopens to discuss)
  approved <- failed (person retries the build with the same plan)
  any state before merged -> abandoned
```

| state | meaning | left by |
|---|---|---|
| `open` | being discussed; the agent may propose a plan | the agent proposing a plan (`planned`), a person abandoning it |
| `planned` | a plan version waits for the person | the person approving that version (`approved`) or replying, which reopens it (`open`) |
| `approved` | the approved plan waits for a build run | the harness enqueuing the build run (`building`) |
| `building` | the build run is working in its sandbox | the harness opening the PR (`review`), or the build failing (`failed`) |
| `review` | the PR is open; checks are running | the gate seeing every required check green (`merging`), a check failing (`failed`) |
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

1. **File.** `POST /tickets` stores the ticket (`open`) and its body as the
   first message of its thread, and wakes the discussing agent's run with
   `ticket_id` set.
2. **Discuss.** Each person message on the ticket wakes a run scoped to it.
   The agent replies in its report, asks questions, and when it has enough,
   proposes a plan. The output contract gains an optional `plan` field
   (Markdown: what changes, which files, how it is tested, what could go
   wrong); the engine stores it with `ProposePlan`, which moves the ticket to
   `planned`. A question ticket never gets a plan field in its schema.
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

**Fixing failed checks.** When a ticket fails on its checks, code collects
what failed: each failing check's name, conclusion and URL, and the last
lines of its log (GitHub Actions job logs, read with a token that adds
`actions: read`), bounded in size. A retry's build run gets exactly that as
an input artifact, with the approved plan and the current branch, never a
model's summary of it, and its prompt says only "make these checks pass
without changing the plan".

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
opened (the loop opens it) and a PR opened twice (the loop finds the
existing one by head branch).

## API

Under `/api/v1`, with the conventions of [api.md](api.md). The person is
the verified authentik token's: a ticket's `author`, a message's author and
a plan's approver are the token's `preferred_username`. Body `author` and
`by` are read only with the operator token on the internal listener, as for
messages and decisions. Creating a ticket, posting to it and approving a
plan take an optional `client_request_id`: a retry with the same id returns
the first result (the `receipts` table, keyed by the ticket's agent); the
same id for a different request is a 409.

| method | path | body | result |
|---|---|---|---|
| `POST` | `/tickets` | `{"kind": "request", "title": "...", "body": "...", "agent": "orbit", "repo": "kensac/orbit", "wake": true, "client_request_id": "..."}` | `201 {"ticket_id", "message_id", "run_id"}` |
| `GET` | `/tickets?status=&agent=&before=&limit=` | | `{"tickets": [...]}`, newest first |
| `GET` | `/tickets/{id}` | | `{"ticket", "plans", "events"}` |
| `GET` | `/tickets/{id}/thread?before=&limit=` | | the ticket's thread, as `GET /agents/{name}/thread`: `{"items", "has_more", "next_before"}` |
| `POST` | `/tickets/{id}/messages` | `{"body", "reply_to", "wake", "client_message_id"}` | `201 {"message_id", "run_id"}`; a `planned` ticket reopens |
| `POST` | `/tickets/{id}/plans/{version}/approve` | `{"client_request_id"}` | `{"ok": true}`; 409 unless it is the newest plan of a `planned` ticket |
| `POST` | `/tickets/{id}/abandon` | `{"reason"}` | `{"ok": true}`; 409 once merged or terminal |
| `POST` | `/tickets/{id}/retry` | | `{"ok": true}`; `failed -> approved` |
| `POST` | `/tickets/{id}/reopen` | | `{"ok": true}`; `failed -> open` |

`repo` is required for a request and must be one of `github.repositories`
in `orbit.yaml`. A ticket in the API:

```json
{"id": "uuid", "kind": "request", "title": "...", "agent": "orbit",
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
  -> runs row (ticket_id, trigger 'ticket')
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
  -> ticket: building -> review
```

### Isolation: one container per build run, started by a launcher

orbit's own container never gets the docker socket, not even through a
filtering proxy. A socket proxy that permits `POST /containers/create` lets
the caller ask for `Privileged`, a bind mount of `/`, the host network or
the host PID namespace: whoever can create containers is root on the host.

Instead a small launcher, `orbit sandboxd` (the same image, a separate
compose service), holds the socket and exposes the only operation orbit
needs, with every security-relevant option fixed in its code:

| request (orbit -> sandboxd) | what sandboxd does |
|---|---|
| `POST /sandboxes` `{run_id, archive}` | creates a container from the build image pinned in sandboxd's own configuration, with a fresh volume at `/work` holding the archive; returns an id |
| `POST /sandboxes/{id}/attach` | starts it with the CLI command line orbit sends (argv only, no options), streams stdio both ways |
| `GET /sandboxes/{id}/archive` | `/work` as a tar |
| `DELETE /sandboxes/{id}` | kills and removes the container and its volume |

Fixed, never taken from a request: the image digest; uid 10001; `CapDrop:
ALL`; `no-new-privileges`; a read-only root filesystem with a tmpfs `/tmp`
and the `/work` volume as the only writable paths; memory, CPU and PID
limits; no bind mounts, devices or extra privileges; and the network. A run
id label and a maximum lifetime let sandboxd reap containers orbit
abandoned. The only environment it passes is the backend credential orbit
sends (see below) and the proxy settings. sandboxd authenticates orbit with
its own token (`ORBIT_SANDBOX_TOKEN`) on a docker network only the two
share.

**Network.** The build container sits on an `internal` docker network with
no route out. Its only egress is an allowlisting CONNECT proxy in sandboxd
(`HTTPS_PROXY`), which allows the backend's API host and the package
registries the repository's build needs (for Go: `proxy.golang.org`,
`sum.golang.org`; per repository in configuration), and nothing else. It
does not allow `github.com`: the checkout is already inside, and a sandbox
that cannot reach GitHub cannot push even with a token it found.

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
  not use the SDK; it speaks this protocol directly, which the SDK is the
  readable reference for.
- `canUseTool` is asked only when the decision is "ask": `allowedTools`
  entries "execute automatically without asking" (`Options.allowedTools`),
  and in `dontAsk` mode an ask is denied without asking anyone
  (`PermissionMode`). So the build run passes **no allow rules for Edit,
  Write or Bash** and uses `default` mode: every write and every command
  reaches orbit's handler. Read, Glob and Grep inside the working directory
  stay allowed by the mode, as they are for read-only agents today.
- Deny rules and `PreToolUse` hook denials win before `canUseTool` is
  consulted: "Denials that resolve before canUseTool runs: PreToolUse hook
  denies, deny-rule overrides of hook allow/ask decisions, and file-tool
  calls (Read, Edit, Write) refused by a path-scoped deny rule"
  (`SDKPermissionDeniedMessage`). orbit's `--settings` therefore also
  carries deny rules for what is never acceptable (`Bash(git push:*)`,
  `Bash(curl:*)`, `Bash(wget:*)`, `Bash(gh:*)`, `Bash(docker:*)`,
  `Bash(sudo:*)`, `Edit(.git/**)`, `Write(.git/**)`), as a second layer that
  holds even if orbit's handler had a bug.
- The authoritative record of what was refused is the result's
  `permission_denials`; orbit stores it, and every decision its own handler
  made, as run events (`orbit.permission`).

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

- something changed, and nothing outside the tree (symlinks pointing out of
  the repository are refused, as are new submodules and files over a size
  limit);
- no protected path changed;
- no credential value or secret pattern appears in the diff;
- the diff is within the size the plan implies (a configured line limit).

The commit is authored by the App's bot user, with a message from the PR
title. orbit pushes exactly one refspec, built from the ticket id:
`<sha>:refs/heads/orbit/<ticket-short-id>`. The code that builds refspecs
cannot produce anything but `refs/heads/orbit/`; a retry force-pushes that
same branch. The default branch should also be protected on GitHub
(require a pull request), with the App not on any bypass list, so even a
bug in orbit cannot push to it.

## Merge: the `github:merge` action (Built)

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
   `dirty`, `blocked`, `behind`, `draft` or `unknown`.
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
  `internal/actions`, `internal/github`, `internal/runner`, the sandbox, the
  tool policy, `deploy/`, `AGENTS.md` or `docs/agents/security.md` is
  labelled as such in the PR and its approval notification, so the person
  approving knows they are approving a change to what orbit may do.
- **Done means running.** A merged orbit ticket waits in `merged` until
  `orbit version` (the process's own revision) is the merge commit or a
  descendant of it; only then is it `done`. A merge that never deploys
  stays visible.
- **The deploy restarts orbit mid-flow.** Runs requeue on shutdown already;
  the merge action and the ticket transition are separate steps, and the
  reconciliation above finishes whichever did not happen. One self-hosting
  ticket merges at a time: a second waits in `review` until the first is
  `done`, so two unrelated changes never ship in one restart nobody
  reviewed together.
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

`orbit.yaml` gains (Built):

```yaml
github:
  app_id: 123456
  # The App's private key: PEM, or base64 of the PEM so it fits one line of
  # an env file. Read by orbit only; never passed to a model.
  private_key_env: ORBIT_GITHUB_APP_PRIVATE_KEY
  # Repositories tickets and github:merge may target.
  repositories: [kensac/orbit]
```

An agent enables merges with `actions: {github:merge: approve}`. A build
agent's spec will add a `build:` block (per repository: the command list,
protected paths, registry hosts, diff limit) and sandboxd its own section
(the build image digest, limits, the egress allowlist).

## Deploying it (owner steps)

1. **Create the GitHub App** under the personal account (Settings, Developer
   settings, GitHub Apps, New). Name: `orbit` (App names are global; if it
   is taken, `orbit-kensac`). Homepage: the repository URL. Webhook:
   inactive for now (orbit polls). Repository permissions: Contents read
   and write; Pull requests read and write; Checks read-only; Commit
   statuses read-only; Actions read-only (job logs of failing checks);
   Metadata read-only (mandatory). No organization or
   account permissions, and not Workflows. Where can it be installed: only
   this account. Events: none until event triggers exist; then Check run,
   Check suite, Pull request and Status.
2. **Install it** on all repositories of the account. Every token orbit
   mints is still limited to one repository and one permission set.
3. **Generate a private key**, and store `base64 -w0 <key>.pem` in Infisical
   as `HETZNER_ORBIT_GITHUB_APP_PRIVATE_KEY` (one line, so it renders into
   `/root/.env` safely), then delete the downloaded file. Add
   `ORBIT_GITHUB_APP_PRIVATE_KEY=${ORBIT_GITHUB_APP_PRIVATE_KEY}` to the
   `orbit` service and the `github:` block, with the App ID from the App's
   page, to `orbit/orbit.yaml` in hetzner-vps-config.
4. **Protect `main`** on repositories orbit may merge: require a pull
   request and the CI check (`check` for orbit), and leave the App off the
   bypass list.
5. When the build runner lands: run sandboxd (on oci), with
   `ORBIT_SANDBOX_TOKEN` (Infisical, under both hosts' prefixes, rotated
   together).
