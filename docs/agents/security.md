# Security model

orbit runs a model on data that may contain instructions (a transaction
memo, a web page, a message someone else wrote). Assume any input can be an
attack on the model, and design so that a fully compromised model can do no
more than its agent's rules allow.

## What the model's process can reach

- **Environment:** an allowlist (`runner.childEnv`): `HOME` (the run's own
  `.home`, or a session agent's persistent home), `PATH`, `LANG`, and one
  backend credential: with `CLAUDE_CODE_OAUTH_TOKEN` set, `ANTHROPIC_API_KEY`
  is withheld, so a stray key can never turn subscription runs into API
  billing. Never orbit's
  environment, which holds every source, channel and endpoint secret, and the
  API token. `TestClaudeCodeRun` and `TestCodexRun` fail if that changes.
- **Files:** Claude Code is given Read/Glob/Grep without `--allowedTools`, so
  headless it may read only inside its working directory, and `--settings`
  adds deny rules for `/proc`, `/sys`, `/run`, `/etc`, `/config` and `~`.
  Verified against the real CLI on 2026-11-01: a read inside the workdir
  worked; a path outside it and `/proc/self/environ` were both denied.
  Codex runs with `-s read-only`.
- **Network:** whatever the agent's `tools` grant (for example `WebFetch`).
  A tool that can reach the network can exfiltrate what the run read; grant it
  only to agents whose inputs you would be fine to see leave.

- **Session files are model-written state.** A session agent's transcripts
  (under its persistent HOME) hold everything earlier runs read and said,
  including any instruction an input smuggled in, and a resumed run reads all
  of it. So a poisoned input can steer later runs of the same agent within
  `idle`, not only its own run; that is the cost of resume, and why it is
  opt-in, bounded (`idle`, `maxSessionDepth`) and reset by any failure or
  spec change. The model cannot read or edit these files directly (they are
  outside its cwd and under the denied `~`), and they never cross agents.
  Claude Code's own auto memory, a second model-written store under HOME, is
  off (`autoMemoryEnabled: false`), so memory_versions stays the only
  memory. Everything in a transcript is also in run_events and artifacts.
- **Control requests:** a live process may ask orbit for a permission or a
  hook decision; orbit configures none of their sources and answers any with
  an error, recorded in the run's events. The tool grant is the spec's
  `tools`, decided before the process starts.

- **orbit's tools and hooks** run inside the CLI's process tree with its
  environment (no orbit secret) and are given `--root <workdir>` and
  `--state <dir>` as flags, never from the environment. The tools read
  only under the root and return values computed from it; a compromised model
  gains no reach it lacked (it could already Read those files). The hook fails
  closed: an event it cannot parse is denied. Hooks come only from orbit's
  `--settings`; a workdir's `.claude/settings.json` is never read.

## What the model can cause

Only actions its spec enables, through executors here, to destinations in
configuration. `continue` can make an agent run again on its own, so it is
bounded in delay and chain length and, like any type, denied unless named. A webhook action chooses a body, never a URL or a header.
`deny` is the default for every type an agent does not name, and denied
types are not even offered.

## The API

- Deployed behind traefik at a public hostname. `/ui/` is behind authentik
  forward-auth; `/api/` is routed past it, and orbit verifies the authentik
  access token itself (`internal/auth`, docs/api.md): signature against the
  issuer's JWKS, `iss`, `exp`/`nbf`, `aud`, the group. The person in the
  token is the author and decider of record; a request body cannot name
  someone else.
- **The operator token** (`ORBIT_API_TOKEN`, at least 32 characters,
  constant-time compared) is accepted only on the internal listener
  (`ORBIT_INTERNAL_LISTEN`), which traefik never routes to. Which listener a
  request arrived on is a property of the socket; a header saying a request
  is internal could be sent by anyone, so none is trusted. Never give the
  internal port a traefik router or a public port mapping.
- Only asymmetric signatures are accepted. An `HS*` token would be verified
  with the client secret, which every client holds, so any client could mint
  any person.
- `/ui/actions/{id}` accepts `sig`, an HMAC of the action id under the
  operator token, so a phone can approve from a notification without a
  login. The GET only shows; deciding is a POST from the page, so a link
  preview cannot approve anything. Rotating the token invalidates every
  outstanding link.
- `/ui/runs/{id}` is read-only and relies on forward-auth. Reports can hold
  personal data; never route `/ui/` around authentik.
- `GET /runs/{id}/stream` shows the agent's narration and tool summaries
  (file paths, commands, URLs): the same data `/events` already serves to
  the same callers.

## GitHub

The App's private key is in orbit's environment only. Every token minted
from it names one repository from `github.repositories` and one permission
set from `internal/github` (never an empty set, which would mean all of the
installation's), is revoked after its operation, and is refused if GitHub
grants more than was asked. `github:merge` lets a compromised model ask to
merge an orbit PR whose checks are green; the executor re-checks the head
commit, the branch and every check itself, and the rule decides whether a
person approves first. In the fleet a merge is a deploy: keep it `approve`.

## Adding a capability

A new tool grant, action type, channel or source is a security change. Say
in the PR what a compromised model could do with it.
