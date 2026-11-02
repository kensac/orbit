# The workspace

**Status: partly built.** Built: finding a workspace, the snapshot backend's file reads (W1), its writes, trash, uploads and the next turn told what the person changed (W3), and the audit. The rest is planned. The contract is in [api.md](api.md), Workspace, with every unbuilt part marked; the implementation items and their owners are in [WORKBOARD.md](WORKBOARD.md). The code is `internal/api/workspace.go` (reads, limits, the snapshot cache), `internal/api/workspace_write.go` (writes), `internal/store/workspaces.go` (snapshots, the write transaction, the trash) and `internal/workspace` (`tree.go` indexes a stored snapshot, `gitignore.go` decides `ignored`, `edit.go` makes a new snapshot from one and diffs two).

The workspace is an IDE inside the orbit chat at `kanishksachdev.com/orbit` (kensac/personal-portfolio, `app/orbit`): a file explorer, an editor, several terminals (one of them can be Claude Code's own interactive TUI, "Claude Code (direct)"), and a diff view whose line comments go to the agent. orbit serves only the API. The portfolio is the only surface: no subdomain, no orbit-hosted page, no desktop or editor integration.

## One shell, not panels

The chat, the workspace, the editor, terminals, diffs, agents and memory are one application. Every portfolio PR that adds or changes a panel meets these, and its description says how:

- **One layout and navigation.** One frame: a rail (conversations, agents, memory, explorer, search), a main area of tabs (thread, file, diff, terminal, agent, memory entry), and the chat beside it. Panels are tabs and docks inside it, never separate pages with their own chrome. A tab survives switching conversations and is restored on reload.
- **One command palette.** A single registry: each panel registers its commands (open file, new terminal, Claude Code (direct), diff vs base, send review, approve action, switch agent, search). One shortcut opens it everywhere; no panel has a palette of its own.
- **One set of design tokens.** Colours, type, spacing, radii and motion come from the portfolio's tokens, light and dark. The editor, xterm and diff themes are derived from those tokens, not shipped with their own palettes.
- **One state store and one opener.** Everything is addressed by orbit's ids: `conversation_id`, `workspace_id` with a path and line, `run_id`, `terminal_id`, `action_id`, a memory entry's id. One `open(ref)` places a ref in the main area, and deep links (`/orbit/c/<conversation>?open=<ref>`) are the same refs. So a path in a reply opens in the same editor tab the explorer opens, a reply's "files changed" opens its diff in the diff tab, an approval in a terminal's flow is the same action card as in the thread, and an edit in the editor is the same buffer the diff view shows.
- **No duplicate fetches.** Panels share the store's caches and streams: one thread stream per conversation, one change stream per workspace, one socket per attached terminal per tab.

The API supports this by naming everything the same way: a conversation carries its `workspace_id`, a run item carries its `changes` summary, and a path in a run's journal (`tool_call` summaries, `diff` items) is relative to the run's directory, which for a chat agent is the workspace's `cwd`.

## Where files and shells live

A **workspace** is one tree of files a person and an agent share, behind one API. It has a backend:

| backend | where | what works | when |
|---|---|---|---|
| `snapshot` | the conversation's newest workspace snapshot (`workspaces` rows, written after every chat run) | read, write (a new snapshot), trash, upload, download, the change stream, per-reply diffs, history. No terminals, no git commands. | first: every chat conversation has one |
| `box` | the agent's workstation: a persistent gVisor container on hetzner or oci, made by sandboxd | everything, including terminals and git | once the workstation exists |

A client reads `capabilities` and never the backend name to decide what to show, so moving an agent from snapshots to a box changes no route and no shape. A conversation's workspace is found with `GET /conversations/{id}/workspace`: on the snapshot backend it is the conversation's own; on a box it is the agent's box, with `cwd` the conversation's directory in it (`conversations/<id>`), so an agent's conversations share one machine, as a person's terminal sessions share one laptop.

**Snapshot backend.** Reads come from the stored tar, so they work whichever host ran the conversation and whether or not its sandbox is up. A write stores a new snapshot (author: the person) and the next run restores it; a live session process is not reused after one (its sandbox holds the old copy), so the session resumes in a new process started from the person's version. A write while a run of the conversation is running is refused (`busy`), since the run's own snapshot would follow it; an advisory lock per conversation, exclusive for the write and shared for the run's restore, makes the check and the restore exclusive of each other. orbit never runs git over a snapshot: a model-written `.git/config` can name commands (`core.fsmonitor`, hooks) that git would run on orbit's host. Git therefore exists only in the box.

**The box.** One container per agent, created by sandboxd's `box` profile (beside `build` and `workstation`): runsc required (sandboxd refuses the profile without it), persistent volumes for `/work` and `/home`, no lifetime timer, the open egress proxy chats use, the same uid, capability and seccomp rules as every sandbox. Its image is multi-arch (amd64 for hetzner, arm64 for oci) and carries a laptop's tools: git, gh, jq, ripgrep, fd, curl, make, node, python, go, tmux, the pinned `claude` CLI, and docker (a daemon inside the box is a later tier of the profile). PID 1 is `orbit box`, a helper in the image that serves file operations (through `os.Root`, so nothing escapes the tree), git, tmux and an inotify watcher over the attach channel, so sandboxd itself gains only two things: the persistent profile and a TTY attach with resize. The box's `/work` and `/home` are stored as snapshots after every agent turn and at most every 10 minutes while a person edits, so a box can be rebuilt on either host and history survives it. An agent's box lives on one host at a time; moving it is a snapshot and a restore.

## Files

Paths are relative to the workspace root, POSIX, clean, inside the tree (symlinks are shown, never followed out of it). A file's `version` is `sha256:<hex>` of its bytes, sent as its `ETag`.

- **Tree** is lazy: one directory per request, with `has_children` on directories, `ignored` from `.gitignore` and, in a box, each entry's git status. The explorer loads a directory when it opens and keeps it current from the change stream.
- **Read** returns text for the editor up to 2 MiB; a larger file or a binary one (a NUL in the first 8000 bytes, as git decides, or invalid UTF-8) returns its stat and a `reason` instead, and the editor offers download or, for raster images, a preview.
- **Write** needs `If-Match` with the version the editor read, or `If-None-Match: *` to create. A mismatch is `412` with the current version, so two devices never silently overwrite each other; the editor shows a merge.
- **Delete is a move to the trash** (`.orbit/trash/` in a box; on the snapshot backend the earlier snapshot keeps the content). Nothing is deleted: the trash has no empty operation, and every item can be restored. Rename never overwrites.
- **Upload and download**: multipart up to 64 MiB a file, a signed URL above that (as conversation uploads), a directory downloads as a tar.gz.
- **The change stream** (SSE) tells the explorer and open editors what changed and who changed it (the person, a run, a terminal); a lost cursor or a watcher overflow sends `resync`, and the client re-reads what it shows.

## Terminals

A terminal is a tmux session in the box: it survives the browser closing, a deploy of orbit, and several devices attaching at once (tmux sizes the window to the most recent active client). tmux over dtach because it gives scrollback to replay and per-client sizing; dtach gives neither.

- **Presets.** `shell` (a login shell in `cwd`) and `claude`: the pinned `claude` CLI in its interactive TUI, in `cwd`, with the box's HOME (so its sessions and settings persist) and the subscription credential in that terminal's environment. Options are a fixed list (resume a session, continue, model), never an argv. This is "Claude Code (direct)": the person driving Claude Code themselves, not an orbit run, so orbit records its lifecycle, not its turns.
- **Attach** replays the scrollback (`tmux capture-pane` with escapes, 10,000 lines), then streams live. The socket protocol is binary frames for terminal bytes and JSON text frames for control (resize, exit, ping); api.md has it exactly.
- **Toolset and GitHub.** Every terminal has gh authenticated and the full toolset above. A person at a terminal may `git commit` and `git push` and use `gh` with write access: direct mode holds GitHub write credentials, and that exposure is accepted for now. The token is put in each terminal's environment by orbit when it starts, never written into the box's files, and agent turns in the box are started without it.

### Transport: through the proxy, or straight to orbit

The portfolio reaches orbit through a server-side proxy on Vercel that forwards the authentik token, so the browser never holds it. A terminal needs a bidirectional, long-lived stream, and Vercel functions do not accept a WebSocket upgrade and cap how long a response may stream. The spike on Vercel, Cloudflare and traefik has not reported at the time of writing, so the contract supports both paths and the client picks:

| | direct: WebSocket to `orbit.kanishksachdev.com` | through the proxy: SSE out, POST in |
|---|---|---|
| how | the proxy asks orbit for an attach ticket (authentik token, server side); the browser opens `wss://orbit.kanishksachdev.com/api/v1/terminals/{id}/socket?ticket=...`. The ticket is single-use, names one terminal and one person, and expires in 30 s; the browser never sees the authentik token. | output as SSE (`/terminals/{id}/output`, resumable with `Last-Event-ID`), each keystroke batch a `POST /terminals/{id}/input`, both through the proxy |
| latency | one round trip to hetzner per keystroke | a Vercel invocation per input batch, plus its cold starts |
| limits | Cloudflare closes a WebSocket idle for 100 s, so both ends ping every 30 s; traefik passes upgrades as is | the function's streaming cap ends the SSE every few minutes and the client resumes; each input is a billed invocation |
| exposure | orbit's public listener takes the socket, so it checks `Origin` against the portfolio's origin (no cross-site socket) and rate-limits tickets | nothing new is exposed |

Recommendation: direct, with the proxied path as a fallback the client switches to when the socket cannot open. The same `/socket` route also accepts `Authorization: Bearer` for a proxy that can carry a WebSocket, so a later proxy needs no change in orbit.

## Git

In a box, for every repository under the workspace (a directory with `.git`, up to three levels deep): status, diff of the worktree against `HEAD` or any base (a ref, or `merge-base:<ref>` for "what this branch changed"), log and history of a file, a file at a revision, branches. Diffs come back structured (files, hunks, lines with numbers) so the diff view and its comments anchor to exact lines, with caps and a truncated flag.

- **Per-reply diff.** What one reply changed: on the snapshot backend, the snapshot before the run against the one after; in a box, orbit's checkpoints (a tree written into a separate git directory under `.orbit/` at the start and end of every turn, not touching the repository's index or history). Every run item in a thread carries a `changes` summary; the diff view opens `GET /runs/{id}/changes`.
- **Line comments to the agent** work as a ticket's review comments do (tickets.md, Review comments): drafts on a path and line range, anchored to a run's change, a git diff or the worktree; edits are new versions; sending the review writes one message to the conversation, formatted by code with each comment's file, lines, the quoted code and the body, and wakes the agent.
- **Commits and pushes, two paths.**
  - **Direct, by the person**: from a terminal with git and gh, or the workspace's commit and push routes, which run git in the box as the person with the terminal's GitHub credential. Nothing is approved; it is the person's own action, recorded in the workspace's events.
  - **Unattended, by an agent**: the box's agent turns hold no GitHub token. An agent proposes `workspace:push` (the repository, the branch, the commit range); orbit's executor reads the commits out of the box as a bundle, applies the diff policy (tickets.md) and pushes with an App token for one repository from `github.repositories`, to a branch that is not protected. The rule decides whether a person approves first. Merges stay `github:merge`.

## Auth, scope and limits

- **Auth** is the authentik JWT as for every route, checked by orbit. The socket takes a ticket minted with one. Writes, trash, uploads, terminals and git writes also require `ORBIT_WORKSPACE_GROUP` (default `ORBIT_OIDC_GROUP`), so the shell can be narrowed to fewer people than the chat.
- **Scope.** A workspace belongs to one conversation (snapshot) or one agent (box), and every route checks the workspace exists and the ref it names belongs to it. An agent's runs reach only their own agent's box; no agent is given the workspace API or another agent's box.
- **Limits** (per person per workspace unless said): reads 100/s, writes 20/s, 8 running terminals per workspace, 8 clients per terminal, 10 attach tickets a minute; editor read 2 MiB, write 8 MiB, upload 64 MiB a file (signed URL to 1 GiB; on the snapshot backend the whole tree is at most 256 MiB, sandboxd's tar limit), tree 2000 entries a page, diffs 300 files, 5000 lines a file, 10 MiB in all. Over a rate is `429` with `Retry-After`. A slow socket client is dropped once 4 MiB is queued for it, and reattaches with a replay.
- **Audit.** Every workspace operation is a `workspace_events` row (append-only, as the journal is): the operation, path, versions before and after, bytes, person, terminal, request id. Terminal lifecycle is recorded; terminal input never is (it carries passwords). The next agent turn in the conversation is told, by code, which files the person changed since its last turn.

## What a compromised model can do here

A chat agent in a workstation sandbox can already run any command with open egress. The box adds: a person's terminals in the same container, so a model's process can read a terminal's environment, the GitHub credential included, and act as the person on GitHub. That is accepted for now and closed by the planned credential proxy. The model still cannot reach orbit's secrets, the App key, another agent's box, or the host. Snapshots and the trash keep every file it overwrites or removes.
