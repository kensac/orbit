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
  Every other Claude Code run also keeps its transcript while it runs, in
  the run's own HOME (denied like any `~`), so an interrupted attempt can be
  continued; it is removed with the run's workdir, and only that run's next
  attempt reads it.
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

## Memory entries a model writes

A run writes memory entries directly, and a learning run writes them from a conversation, so a poisoned input or message can plant an entry that later runs of every agent reading its scope are shown. What bounds it: entries are shown as data (the system prompt says they never authorize an action the rules do not, and an instruction in one is not to be followed); a run may write only to the profile, itself and the topics it reads, at most 10 writes, and only change entries it was shown at the version it was shown; every version is kept with the run and conversation it came from (`GET /memory/entries?run_id=`), so a bad one is found and retired or restored. A learning run reads the person's messages and the agent's reports (which can quote inputs), not the inputs themselves.

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
- `POST /api/v1/drain` takes an instance out of service; it exists only on
  the internal listener, so nobody on the public one can, whatever their
  token.
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
- **`/mcp`** is the API as tools, with the same check: an authentik token (the person decides and authors), the operator token only on the internal listener. Its tools can do what `/api/v1` can (post, approve, reject); nothing more.
- **The MCP OAuth proxy** (`oauth.go`) lets anyone register a client, so the redirect host list (`ORBIT_MCP_REDIRECT_HOSTS`, default the Claude apps) is what stops a link that would send a signed-in person's code to a stranger's host; the code is useless without the PKCE verifier its client holds. A code is single-use per instance only (a replay to the other instance of a rollout within its 2 minutes is the gap), and every sealed value is keyed from `ORBIT_API_TOKEN`. The client receives the person's authentik access token for the `orbit-mcp` provider, which orbit accepts like the portfolio's; the refresh token it receives is sealed and works only through orbit, which holds the client secret.
- `GET /runs/{id}/stream` and `/journal` show the agent's prose, thinking,
  tool inputs and outputs: the same data `/events` already serves to the
  same callers.

## Mail

A mail is a message from a person, so it is held to who may send one (`internal/mail`):

- **Who.** Only a sender in `mail.allow` (default: the smtp channels' recipients) is acted on. `From` can be forged, so a mail whose `Authentication-Results` says `dmarc=fail` is refused; a mail with no such header has the allow list as its only check. Requiring a DKIM pass would be stronger.
- **Where.** A reply reaches a conversation only through a token signed with the operator token, so knowing a conversation id is not enough to write into it; any other mail opens a new conversation with `mail.agent`.
- **What.** The text becomes a message like one typed into the API: the agent sees it as its person's words. Nothing in a mail is executed, and attachments reach the agent only as names.
- **Loops.** Automatic mail is never acted on, so an auto-reply to orbit's report does not wake a run that mails again.
- The IMAP password is in orbit's environment only, like every channel secret; a port other than 993 must offer STARTTLS before login.

## Workers

A worker (`orbit worker`) holds the database URL, its backend credential and the source credentials of the capabilities it advertises; no API token, approval secret, channel credential, GitHub key or sandbox token (docs/agents/deployment.md, Environment per role). So a compromised worker host reaches what its own agents could read, plus the database: every run's inputs and transcripts, which is why workers run on fleet hosts only and reach orbit-db over the tailnet with TLS, never publicly. It cannot send a notification (control does, from `outbox`, with the links it signs) or execute an action (control's action loop executes only actions recorded under each agent's rules). Stored session transcripts (`session_transcripts`) are the same model-written state as the files above, now readable from the database too; a restore writes only regular files named for the session, under the claiming host's own projects directory.

## GitHub

The App (`orbit-kensac`, docs/tickets.md) has its private key in orbit's
environment only. Every token minted
from it names one repository from `github.repositories` and one permission
set from `internal/github` (never an empty set, which would mean all of the
installation's), is revoked after its operation, and is refused if GitHub
grants more than was asked. `github:merge` lets a compromised model ask to
merge an orbit PR whose checks are green; the executor re-checks the head
commit, the branch and every check itself, and the rule decides whether a
person approves first. In the fleet a merge is a deploy: keep it `approve`.

`actions: write` (rerunning failed jobs) also covers dispatching and cancelling workflows; orbit mints it alone, only in the ticket loop, for `RerunJob` on the job ids of checks that failed, and no model sees it.

## The build sandbox

A build run's model can write files and run any command its tool policy
lets through, and `go test` runs code it wrote, so the tool policy is not
the boundary: the container is (`internal/sandboxd`, ARCHITECTURE.md).

**Guaranteed by sandboxd's code**, checked from inside a real container by
`TestIntegrationIsolation` (CI runs it on every change; locally it skips
without docker):

- Every process runs as uid and gid 10001 with no capabilities (effective,
  permitted and bounding sets empty), `no_new_privs` set and docker's
  seccomp filter on; sandboxd refuses a daemon without seccomp.
- The root filesystem is read-only: writes to `/`, `/etc`, `/usr`, `/var`
  and `/root` fail. `/work` (the checkout, read back) and `/tmp` (tmpfs,
  discarded) are the only writable paths. No host path is mounted and the
  docker socket is not there.
- No route out except the proxy: a direct TCP connection to a public
  address fails, an external name does not resolve, and through the proxy
  GitHub, a host off the allowlist and any port but 443 are refused (403).
  The proxy never dials a private, loopback, link-local or CGNAT address
  (every tailnet address), so an allowed name that resolves to one cannot
  reach the host's networks.
- The environment is sandboxd's fixed list plus the backend credential
  (`sandbox.AllowedEnv`); no orbit secret, no GitHub token.
- Callers cannot pass docker options: the create request is the tar and a
  run id, the attach request is argv and env (unknown fields refused); the
  unit tests pin the full create document and reject any container the
  daemon made differently (`verifyCreated`). The API answers "no such
  sandbox" for any container without sandboxd's label, including every
  other container on oci.
- Lifetime: PID 1 is `sleep 1h`, so the kernel ends every process at
  expiry; the reaper removes the container 30 minutes later. At most two
  exist at once, each limited to 6 GiB of memory (tmpfs included), 2 CPUs
  and 1024 processes.
- The workdir crosses the boundary only through `tarfilter.go`: no devices
  or FIFOs, no names outside it, ownership and special mode bits rewritten.

**Not guaranteed:**

- **The backend credential is readable by code the model runs** (same uid;
  `/proc/<pid>/environ`), and an allowed host can carry it out: the Go
  module proxy fetches any module path a request names, from a host the
  path names, so a request for a crafted path is a message to whoever owns
  that host. Hence tickets.md's precondition (only `kensac/orbit`, whose
  diff is reviewed, until the credential proxy keeps the token out) and the
  diff policy's exact-match scan before anything is committed. Treat every
  allowlisted host as a channel for anything the sandbox holds.
- **The kernel is shared** with oci. A container escape is root on oci,
  which runs CI runners and holds less than hetzner; `ORBIT_SANDBOX_RUNTIME=
  runsc` (gVisor) narrows this where installed.
- **`/work` has no disk quota**: a sandbox can fill oci's docker volume
  until its lifetime ends. The readback refuses more than 256 MiB.
- **sandboxd holds the docker socket**, so a bug in it is root on oci. Its
  surface is the four handlers and `docker.go`; it listens only on the
  tailnet address or a unix socket (`validListen` refuses every interface)
  and requires the token on every request.
- **A dropped attach does not stop the process**; the lifetime or a destroy
  does.
- Tunnelled traffic is opaque: the proxy sees hostnames, not requests, so
  it cannot tell a download from an upload to an allowed host.

## Build runs

What a build's model can do, and what stops it (docs/tickets.md, The build
run; `internal/build`):

- **Every Edit, Write and Bash call is decided by orbit's process**, outside
  the container, by `build.Policy`: ask rules in the CLI's settings send
  each one to the stdio permission prompt (without them the CLI runs
  commands it deems read-only unasked; seen on 2.1.316), and deny rules
  refuse pushes, network clients, `gh`, `docker`, `sudo` and writes under
  `.git` or `.github` before orbit is asked. Each decision is a
  `build_decisions` row written before the answer; a failed write, or an
  attempt that lost its claim, answers deny. `TestToolPolicy` holds the
  table; `TestRealCLIPermissionProtocol` checked the protocol against the
  real CLI on 2026-11-01 (allowed and denied writes and commands, deny
  rules refusing unasked). The policy keeps a run on task; it is not the
  boundary, because commands it allows run code the model wrote.
- **The diff policy is where unseen writes are caught**: a file a test
  wrote, a symlink, a submodule, a protected path, a large file, the
  backend credential or a credential pattern in an added line all refuse
  the build before anything is committed. The model's `.git` never reaches
  orbit's clone, and orbit's git runs with no hooks and no user config.
- **No GitHub token reaches the sandbox.** The read token clones and is
  revoked before the sandbox starts; the delivery token exists only in the
  environment of `git push` and orbit's API calls, and pushes one refspec,
  `+<sha>:refs/heads/orbit/<8 hex of the ticket id>`, which the code
  cannot build for any other ref. Neither has `workflows`, so GitHub
  refuses a workflow change too. An Actions log is fetched by following
  its storage redirect by hand, with no credential.
- **The PR is orbit's**: title and body from the model's result, scrubbed
  of attribution, with the ticket and plan version and, for orbit itself,
  a trust boundary line. A compromised model can therefore at most open a
  PR on its own branch with a diff the policy passed; merging it needs
  green required checks and, for orbit, a person (`tickets.MergeRule`).
- **Ticket text is data**: the build's instructions are the approved plan;
  the ticket and any failure report reach the prompt inside a delimited
  data block whose delimiter and control characters are escaped.
- **One build and one merge at a time per repository**
  (`tickets_one_building`, `tickets_one_merging`), and an attempt destroys
  any sandbox an interrupted attempt of its run left before it starts its
  own, so a build never holds two sandboxes.
- **Not guaranteed**: the backend credential is in the sandbox (above);
  everything the sandbox section says it does not guarantee holds for a
  build.

## Adding a capability

A new tool grant, action type, channel or source is a security change. Say
in the PR what a compromised model could do with it.
