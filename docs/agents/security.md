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
  only to agents whose inputs you would be fine to see leave. `web: {allow: [...]}` narrows WebFetch to named domains (one `WebFetch(domain:...)` allow rule each, never the bare tool) and denies the private names (`spec.PrivateWebNames`) with `--disallowedTools`, which no allow or answer overrides. Seen on 2.1.316: an allowed domain and a `*.` subdomain were fetched, another domain and `localhost` were denied, and a WebFetch to `127.0.0.1` reached the host's loopback, so an address is never a valid entry. The rules match the URL's host, not where it resolves: an allowed public name that resolves to a private or tailnet address is still reached, which only a sandbox's egress (it never dials one) prevents. An allowed domain is still a channel out for what the run read.

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
- **Control requests:** a live process may ask orbit for a permission, a
  hook decision or an SDK MCP message; orbit configures them only for its
  chat tools and a run's connectors (Connectors, below), and answers any
  other with an error, recorded in the run's events. The tool grant is the
  spec's `tools` and the connector tools its rules offer, decided before
  the process starts. Chat processes also ask for tool permission decisions under their configured level. Orbit records a decision before answering and denies a request if it cannot record the decision.

- **orbit's tools and hooks** run inside the CLI's process tree with its
  environment (no orbit secret) and are given `--root <workdir>` and
  `--state <dir>` as flags, never from the environment. The tools read
  only under the root and return values computed from it; a compromised model
  gains no reach it lacked (it could already Read those files). The hook fails
  closed: an event it cannot parse is denied. Hooks come only from orbit's
  `--settings`; a workdir's `.claude/settings.json` is never read.
  orbit never writes hooks or a status line into any settings file, the
  user's `~/.claude/settings.json` least of all: they exist for one run's
  CLI process, on its command line.
- **The status channel** is a unix socket, mode 0600, and a spool file in the run's state directory under the CLI's HOME (which the CLI denies reading): local to the host, never a network listener. What arrives on it is the model's process tree reporting on itself, so it is data, never trusted for a decision: it sets the status a person sees and the run's `orbit.hook` events, and nothing else. A forged event can misreport a running run's status at most; the run's row decides once it is final.
- **The search tool** (`mcp__orbit__search`) holds no database credential:
  the tool server asks the engine over a unix socket (`--search`, a 0600
  file under the temp directory) that the engine serves for one agent, and
  the engine sets the agent on every query, so a run finds only its own
  agent's records, which it could already have been shown. Results are
  past writing, so they are data like any input, and they never back a
  figure (they are kept out of the tool ledger).

- **The usage poller** (`usage_poll`) is orbit's own code on control, outside any run: it reads the subscription's OAuth token from orbit's environment and sends it only to the configured URL. The token never reaches a model, a sample or a log line (an error records the status and a clipped body).

## Memory entries a model writes

A run writes memory entries directly, and a learning run writes them from a conversation, so a poisoned input or message can plant an entry that later runs of every agent reading its scope are shown. What bounds it: entries are shown as data (the system prompt says they never authorize an action the rules do not, and an instruction in one is not to be followed); a run may write only to the profile, itself and the topics it reads, at most 10 writes, and only change entries it was shown at the version it was shown; every version is kept with the run and conversation it came from (`GET /memory/entries?run_id=`), so a bad one is found and retired or restored. A learning run reads the person's messages and the agent's reports (which can quote inputs), not the inputs themselves. A learning run over an imported session reads the person's own Claude Code session, tool calls included (one line each) but not their results, so whatever passed through that session (a web page it fetched and quoted, a file's text) can reach it the same way; its entries are bounded as above, and the skills it proposes are only ever `skill:write` actions a person approves, because a skill is instructions every agent given it follows.

## Agent hooks

An agent's `run` hooks are commands from its configuration files, never chosen by a model (a database or built-in agent may not have one, What a model can define), but they read the event's JSON (tool inputs the model wrote) on stdin, so write them as parsers of untrusted input. They run as orbit's user with only `PATH`, `HOME` (the workdir) and `LANG` in their environment, so no orbit secret and no backend credential; the filesystem and network are not isolated beyond that, so a hook command is as trusted as anything else in the configuration. `say` and `deny` run no command.

## Skills

A skill is instructions a run follows, and agents write their own, so a run steered by a poisoned input can write a skill that steers its later runs, and those of every agent the skill is attached to. What bounds it: a write is the `skill:write` action, under the agent's rule (denied unless named; keep it `approve` where a person should see each one, as the built-in `orbit` has it), and the approval shows what it changes and every script or executable the skill holds, worked out by code, never the model's account; a run writes at most 3 skills, only new ones or ones it was given (at the version given), and a written skill is attached to that agent only; every version is kept with its run (`source_run_id`), so a bad one is found, restored or retired. The skill tool reads only the run's own copy, inside each skill's directory after symlinks; it records nothing in the ledger, so a skill's text never backs a figure. Supporting files are stored as given and never executed by orbit; a script in a skill runs only if the agent's own `tools` let it.

## What the model can cause

Only actions its spec enables, through executors here, to destinations in
configuration. `continue` can make an agent run again on its own, so it is
bounded in delay and chain length and, like any type, denied unless named. A webhook action chooses a body and, where its endpoint's path has placeholders, their values, each checked against the endpoint's declared pattern, enum or integer type and escaped as one path segment: never a host, a route outside that template, the method or a header. A credential in an endpoint's header goes only to a URL under its `url_prefix`, and no redirect is followed, so a write is never re-sent somewhere else. What a compromised model can do with one is any write that endpoint's template and `body_schema` allow, on any value its pattern admits (every transaction, for Sure's categorize), under the action's rule. `agent:define` lets a model rewrite an agent defined in the database or built in, itself included: its charter, tools, sources and rules. What such an agent may hold is limited below (What a model can define), and a change that widens one waits for a person. Never a file agent's. Every change is a version with its run, so it is seen and reverted through the API. The schedule actions are narrower forms of it: they change only triggers (within reach, so no person is needed beyond the rule), only of the agent itself or one it delegates to, with times parsed by code. `run:now` starts a run, but a run no message woke cannot start its own agent, so it is no unbounded loop; `continue` is the bounded way.
`deny` is the default for every type an agent does not name, and denied
types are not even offered.

## What a model can define

A database agent is written through the API or by `agent:define`, and the built-in `orbit` is changed the same way, so a model steered by a poisoned input (a fetched page, an upload) can write either. Without a sandbox (`sandbox:`, Sandboxed chats below) their runs are in orbit's container, which holds every `ORBIT_*` secret, and their sources are fetched by orbit either way. So `spec.validateTrust` holds them (`spec.Agent.Restricted`) to what orbit.yaml offers, at load and so on every write:

- **No `file` source.** It would read orbit's filesystem (`/proc/self/environ`, `/config`, `/data`); the fetcher refuses one for a restricted agent too.
- **A webhook secret only where orbit.yaml binds it.** A webhook trigger names a variable from `webhook_secrets` whose `agents` list the agent (file agents too), so a model cannot make `ORBIT_API_TOKEN` a signing key or take another agent's deliveries.
- **No `${VAR}` but a credential.** orbit.yaml's `credentials:` binds each variable it lists to a URL prefix; a restricted agent may reference only those, so it can never render `${ORBIT_GITHUB_APP_PRIVATE_KEY}` into a header. The fetcher sends a credential only in a request whose URL starts with its prefix and has no dot segment, and refuses a redirect outside it (Go resends custom headers to any host). The binding holds for file agents too; a file agent may still name any other variable.
- **Without a sandbox, only `spec.UnsandboxedTools`** (Read, Glob, Grep, WebSearch, WebFetch, Task, TodoWrite): no Bash, Edit, Write or NotebookEdit, which would run commands or write files beside the secrets. The list is the one place to change. In a sandbox the container is the boundary and its profile decides the tools.
- **No precheck and no `run` hook** (Prechecks and Agent hooks, below): either would run a command on orbit's host as orbit's user. `say` and `deny` hooks are allowed.
- **Connectors only from orbit.yaml.** Every agent names connectors by name; their URLs and credentials are configuration. Adding one, or raising a tool's rule (`mcp:<connector>:<tool>`), widens the agent.
- **A change that widens an agent waits for a person.** `spec.Widening` compares the change, loaded as it would run, with the agent's current version (nothing, for a new agent): a tool, a source added or changed, a credential, a delegate target, a need, a webhook trigger, a move to a sandbox profile, or an action type allowed more readily (deny, approve, auto). An `agent:define` that widens is recorded as `approve` whatever its rule says. One executing by rule alone is compared again against the agent as it is then, and fails if it widens, so a person's narrowing between proposal and execution holds.

- **"Approve and always allow" is a person's change**, made only through the API or MCP with their token (`POST /actions/{id}/approve` with `always`): a new version with that one type under `auto`, never offered to a model. It does not lift the widening check, which holds an `agent:define` for a person whatever its rule.
- **"Save this conversation as an agent" (`agent:save`) never lets a model choose reach.** The model writes the name, description, charter and starters; the tools, sources, rules and schedules come from the conversation's record, no broader than what happened (an approved type becomes `approve`, never `auto`), and the whole agent waits for a person as an `agent:define`, whatever the rule.

- **A template is a spec a person wrote, filled by code.** `template:create` gives a model no reach `agent:define` does not: what it renders is an `agent:define` of a new agent, always waiting for a person, held to everything above. Values are data in JSON strings, never scanned for slots, so a value cannot add a field or a slot. A `user_prompt` value is taken only from the person's own answers to orbit's question (store.FormAnswers), never from the proposal, and a `file` value only from the conversation's uploads.

What is left: an agent with `WebFetch` can send what its run read to any host, and a change within the agent's current reach (its charter, its triggers, narrowing) applies without a person.

A worker rebuilds agents from the database without their origin and does not repeat these checks: control validated every spec it recorded, and the credential binding is checked on every fetch.

## Connectors

A connector's tools act on a person's account at another service (internal/connectors, engine/connectors.go). What holds:

- **No credential reaches the model's process.** Each connector is an SDK MCP server in the CLI's `--mcp-config`: its calls arrive in orbit's process over the control channel, and orbit sends them to the server with the credential. The CLI's environment stays `runner.childEnv`, and a sandbox gets nothing new; `TestClaudeCodeConnectors` fails if the credential reaches the CLI's environment or arguments.
- **Requests go only to the connector's URL.** The URL is configuration; orbit refuses a request to another host and follows no redirect, and a header credential must be bound to a prefix the URL starts with (`credentials:`, checked at load and per call).
- **Writes wait for a person.** A tool in `write_tools`, or one an agent's rules set to `approve`, never runs on the model's word: in a run answering its person it runs only after they approve the pending `connector:` action in the chat (one execution, claimed in the database, so the action loop and the run cannot both run it); in any other run it is only recorded. A call the CLI makes without asking first is refused, not run. A `deny` tool is not offered.
- **What a compromised model can do:** call any `auto` tool with any arguments, as often as its turn allows: read what the person's account there can read, and, with `WebFetch`, send it anywhere. Treat an `auto` tool like a source the model chooses the query of. A server's tool descriptions and results are data written by someone else: they reach the model like any input, and a server may describe a write tool as read-only, which is why `write_tools` decides and the server's hints do not.
- **OAuth.** A sign-in is started by an authenticated person (`POST /connectors/{name}/authorize`); its state (PKCE verifier, client, metadata, the person) is sealed under `ORBIT_CONNECTOR_KEY` with a 15-minute expiry, so the unauthenticated callback accepts only a state orbit made. Tokens are sealed in `connector_tokens` under the same key, so the database and its backups hold none in the clear; a host holding the key and the database can use them, which is why only hosts advertising `connectors` get the key. Every version is kept with who signed in; a person signs in again to replace one.

## Web Push

- A push leaves orbit for a third party (the browser vendor's push
  service, then the device's lock screen), so it carries only the
  attention row's title (an agent name and a fixed phrase) and a link into
  the app. Never a report, a question, an error or an action's parameters:
  agents read finance data. The payload is encrypted to the device, so the
  push service sees only its size.
- A subscription's endpoint is a URL a browser chose, and orbit posts to it
  from inside the network, so registration accepts only https URLs on the
  known push services (`push.AllowedEndpoint`).

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
- **Every key orbit signs or seals with is derived from the operator token** by `secret.Key.Derive(purpose)` (`internal/secret`): HMAC-SHA256 of a purpose label under the token, one label per use, so no two uses share a key and rotating the token voids all of them together. The purposes are `secret.Approval(id)` (`action:<id>`, whose output is the approval link's signature), `secret.Mail(id)` (`mail:<id>`, whose first 12 hex digits sign a mail token), `secret.UploadURL` (`orbit upload url`, the key of signed upload URLs), `secret.WorkspaceUploadURL` (`orbit workspace upload url`, the key of signed workspace upload URLs) and `secret.MCPOAuth` (`orbit mcp oauth`, the OAuth proxy's sealing key) and `secret.TemplateLink` (`orbit template link`, the key of template links). Each is byte-identical to what orbit signed before the package existed, and golden tests (`internal/secret`, `internal/mail`, `internal/engine`, `internal/api` `derive_test.go`) hold them there: changing a label voids every approval link, reply address, upload URL and in-flight OAuth sign-in already sent. A new use gets a new label; an existing label never changes.
- **Question forms** are written by a model, so they are data like a question's text: code checks each form (`forms.Validate`) and each answer against it (`forms.Answers`) before anything is stored, and a file answer can name only an upload of the conversation the question is in, so a form cannot reach another conversation's files. The answers reach the agent as the reply's text, which is the person's words.
- `POST /api/v1/drain` takes an instance out of service; it exists only on
  the internal listener, so nobody on the public one can, whatever their
  token.
- Only asymmetric signatures are accepted. An `HS*` token would be verified
  with the client secret, which every client holds, so any client could mint
  any person.
- `/ui/actions/{id}` accepts `sig`, the action's approval purpose derived from the
  operator token, so a phone can approve from a notification without a
  login. The GET only shows; deciding is a POST from the page, so a link
  preview cannot approve anything. Rotating the token invalidates every
  outstanding link.
- **Signed upload URLs** (`PUT /api/v1/uploads/signed`, docs/api.md) are the one `/api/` route without a token: the HMAC in the URL, keyed from `ORBIT_API_TOKEN`, names one conversation, file name, type, size bound, person and a 15-minute expiry, and its nonce is unique in `uploads`, so a URL writes one file once. Anyone holding a live URL can upload that one file into that conversation; nothing else. Uploaded files are served as downloads (images inline) with `nosniff` and a CSP that runs nothing, never as pages.
- **Webhooks** (`POST /hooks/{agent}/{trigger}`, docs/api.md) are the other route without a token, and traefik routes `/hooks/` past forward-auth: senders cannot sign in. The HMAC over the body, under a secret orbit.yaml binds to the agents that may name it (`webhook_secrets`), is the credential, compared in constant time before anything is stored; a `standard` delivery's signed timestamp must be within 5 minutes, and every delivery id acts once, so a captured request replays nothing. A trigger whose secret is unset answers `503`, never accepts unsigned. What passes is the sender's text: it reaches the model only as a `webhook` source, untrusted input like any fetched page, and a webhook run is unattended (its agent's rules apply as written). The body is bounded by `max_bytes` (at most 8 MiB) before it is read whole.
- **Template links** (`GET /api/v1/templates/shared/{token}`) are another `/api/` route without a token: the HMAC names exact template versions and an expiry of at most a week, so anyone holding a live link reads those templates (a template holds no secret: a `${VAR}` is a name, its value is orbit.yaml's) and nothing else.
- **Imported templates are someone else's text.** `share.Read` refuses Unicode tag characters and bidirectional controls anywhere in a bundle, since either makes a spec read to a person differently from what it says to a model, and lowers what arrives so it grants nothing: every action rule that is not `deny` becomes `approve`, and `connectors` are removed. Importing from a link fetches only a public `https` address (`egress.Public`, checked on the connection), without credentials or redirects, so a link cannot aim orbit at the tailnet.
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
from it names one repository and one permission
set from `internal/github` (never an empty set, which would mean all of the
installation's), is revoked after its operation, and is refused if GitHub
grants more than was asked. The repository is one of `github.repositories` for tickets, builds and merges, and one of `github.read_repositories` for agents that read (below); the two lists are separate, so reading a repository never lets anything write to it. `github:merge` lets a compromised model ask to
merge an orbit PR whose checks are green; the executor re-checks the head
commit, the branch and every check itself, and the rule decides whether a
person approves first. In the fleet a merge is a deploy: keep it `approve`. A chat agent's `approve` rules are satisfied by its person's message in the conversation (`instructed_by`): a prompt injected into what the agent read during that turn can therefore get an approve-rule action executed while the person is chatting, as it could make Claude Code do anything its permissions allow. Unattended runs never get this. `github:pr` pushes only `orbit/chat/<branch>`, built by orbit from the working tree (the model's `.git` is never read or run), held to the build's diff policy; merging still needs every required check green.

`github:comment` posts as the App in one of `github.comment_repositories` with a token of `issues: write` alone, minted for the comment and revoked after it; the executor re-checks the repository at execution. A compromised model can post any text (at most 20000 bytes, attribution lines scrubbed) on any issue or pull request of those repositories, under the rule, where every reader of the repository sees it: keep it `approve` for an agent whose inputs others write. `issues: write` also lets a token edit, label, close or lock issues; orbit's code posts only `POST /issues/{n}/comments` with it.

`actions: write` (rerunning failed jobs) also covers dispatching and cancelling workflows; orbit mints it alone, only in the ticket loop, for `RerunJob` on the job ids of checks that failed, and no model sees it.

## GitHub and Loki sources

A `github` source is fetched by orbit as the App with a token of one resource's read permissions (`issues: read`, `pull_requests: read`, `checks: read` with `statuses: read`, or `contents: read` for releases), minted for one repository of `github.read_repositories` for that fetch alone and revoked after it; the model sees only the projected JSON. A compromised model gains nothing it did not already read: it cannot change the repository, the resource or the permissions, which are its spec's, and an `agent:define` giving a database agent one is held to `read_repositories` and waits for a person as any added source does. Issue and pull request text is written by whoever can open one on that repository: data, like any input. A `loki` source is a GET of `query_range` with the configuration's LogQL; log lines carry whatever services logged, request paths and user agents from the internet included.

## Repositories agents read

An agent's `repos` (configuration.md, Repositories) are read by orbit, never by the model with a credential (`internal/repos`, `engine/repos.go`):

- **Tokens.** Two permission sets, each minted for one repository of `github.read_repositories`, for one operation, and revoked after it: `ReadContents` (`contents: read`) for each fetch into the mirror, and `ReadPulls` (`pull_requests: read`, `checks: read`, `statuses: read`) for each `pr_list` or `pr_view` call. Neither can push, comment, merge, rerun or change anything; `ReadContents` reads every file and branch of that one repository, which is the point. A token lives in orbit's memory and in the environment of the one `git fetch` or `ls-remote` that uses it (`GIT_CONFIG_*`, as for builds), never in a file, a remote URL or the mirror's config (the remote is given on the command line), and never in a model's process or a sandbox.
- **The mirror** (`<data>/mirrors/<owner>__<name>.git`) is bare and written only by orbit's git, run with no system or global config and no hooks. A run gets the tree at one commit through `git archive`, unpacked through `os.Root` into its inputs: regular files and directories only (symlinks and submodules left out, so nothing in a repository can point outside the run's directory), no `.git`, at most `repos.MaxTreeBytes` and `repos.MaxTreeEntries`.
- **The tools** (`git_log`, `git_show`, `git_diff`, `git_blame`, `pr_list`, `pr_view`) run in orbit's process: for a report agent the tool server asks the engine over a per-agent unix socket (as search does), for a chat agent the engine answers the SDK tool call itself. The engine reads the agent's spec in force and refuses any repository it does not name. A model's revision or path is never read as an option (a leading `-` is refused, and every command has `--end-of-options` or `--`), only reads are ever run, and an answer is at most `repos.MaxOutput`. They need no Bash, no WebFetch and no token in the model's reach.
- **What a compromised model gains:** the contents and history of the repositories its agent names, and their pull requests and checks, which its run already reads. A repository's text is data like any input: a file can carry instructions. A network tool (`WebFetch`, a workstation's egress) could carry what it read out; give `repos` naming a private repository only to agents whose tools you would let see it leave. `agent:define` can give a database agent `repos`, but only within `github.read_repositories`.
- **Workers** hold no App key, so an agent with `repos` must need `github`, a capability only control advertises.

## The Prometheus tool

`prometheus_query` (docs/api.md, Prometheus) lets a model choose a PromQL query and its times, never the URL, a header or the HTTP method: orbit sends a GET of `/api/v1/query` or `/api/v1/query_range` to the agent's prometheus source, with that source's credentials under their binding, and the engine reads the source from the agent's spec in force on every call. The query is a value, never rendered, so a model cannot make orbit expand a `${VAR}` into it. What a compromised model gains is every series that Prometheus holds, which is usually more than the source's own `expr` reads (on hetzner: every host's metrics, container names, scrape targets), and the load of the queries the bounds allow (31 days, 1000 points, 500 series, a 20 s evaluation timeout). Give a prometheus source only to agents that may read the whole Prometheus.

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
  reach the host's networks. A build tunnel must open with a TLS ClientHello whose server name is the allowed host, read before anything is dialed, so a tunnel to an allowed front (a CDN, Google's module proxy) cannot ask it for another name (`TestServerName`).
- A build never shares a network with the workstation profile's open proxy: workstation sandboxes join `ORBIT_SANDBOX_WORKSTATION_NETWORK`, which sandboxd requires with the open proxy and refuses to be the build network, and both must be internal (`TestWorkstationOwnNetwork`, `TestConfigValidate`). Otherwise code in a build could name the open proxy and leave its allowlist behind.
- The environment is sandboxd's fixed list plus a credential proxy token in one backend credential's place; no orbit secret, no GitHub token, and never the credential's value. orbit chooses the credential as for an ordinary run, for builds and sandboxed chats alike (`build.SandboxEnv` through `claudeproto.Credential` and `claudeproto.Env`): the subscription token when set, else the API key; `sandbox.AllowedEnv` permits `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY` in an attach, refuses one carrying both, and sandboxd replaces the value with the token before the process starts (`TestServerFlow` opens the token sandboxd sent; `TestIntegrationIsolation` reads the variable inside a real sandbox).
- **The backend is reached only through `orbit credproxy`** (`internal/credproxy`): `ANTHROPIC_BASE_URL` points at it and `NO_PROXY` names it. The token is the credential sealed (AES-256-GCM) under `ORBIT_CREDPROXY_KEY`, with the container's address on the sandbox network and the container's expiry. The proxy forwards only `POST /v1/messages` and `POST /v1/messages/count_tokens` (no query but `beta=true`) to `api.anthropic.com`, and only for a token that opens, presented in its own kind's header, from that address, before that time; everything else is 401 or 403 and never reaches the backend. It strips cookies, proxy and forwarding headers and any credential the request carried, dials only a public address for the backend's name, and streams the answer unbuffered. Unit-tested in `internal/credproxy` (every refusal, streaming, the public-address dial) and from inside a real sandbox by `TestIntegrationIsolation` (its token forwarded to the Messages API, another path and a forged token refused by the proxy). sandboxd refuses to start without `ORBIT_SANDBOX_CREDPROXY` and `ORBIT_CREDPROXY_KEY`, so no configuration hands a sandbox the credential.
- Callers cannot pass docker options: the create request is the tar and a
  run id, the attach request is argv and env (unknown fields refused); the
  unit tests pin the full create document and reject any container the
  daemon made differently (`verifyCreated`). The API answers "no such
  sandbox" for any container without sandboxd's label, including every
  other container on its host.
- Lifetime: PID 1 is `sleep 1h`, so the kernel ends every process at
  expiry; the reaper removes the container 30 minutes later. At most two
  exist at once, each limited to 6 GiB of memory (tmpfs included), 2 CPUs
  and 1024 processes.
- The workdir crosses the boundary only through `tarfilter.go`: no devices
  or FIFOs, no names outside it, ownership and special mode bits rewritten.

**Not guaranteed:**

- **The token spends the credential while the sandbox lives.** Code the model runs can read it (same uid; `/proc/<pid>/environ`) and call the Messages API through the proxy until the container's expiry, as the CLI itself does; it cannot read the credential, use the token from anywhere else, or reach another backend endpoint with it. A leaked token is ciphertext of the credential: whoever also holds `ORBIT_CREDPROXY_KEY` (sandboxd's and the proxy's environment only) recovers the credential from it, so the diff policy refuses the token's pattern as it refuses the credential's exact value.
- **An allowed host is still a channel for anything the session read**: the Go module proxy fetches any module path a request names, from a host the path names, so a request for a crafted path is a message to whoever owns that host. Hence tickets.md's precondition (only `kensac/orbit`, whose diff is reviewed, until the credential proxy is deployed) and the diff policy's scans before anything is committed. Treat every allowlisted host as a channel for anything the sandbox holds. With the proxy deployed the build allowlist does not need `api.anthropic.com` and should not have it (deployment.md): an open route there carries data out under any API key the model brings.
- **The kernel is shared** with the host, hetzner today. A container escape is root on hetzner,
  which holds every other secret in the fleet; `ORBIT_SANDBOX_RUNTIME=
  runsc` (gVisor) narrows this where installed; deployment.md, Box isolation on a host, is the planned layout (a second daemon whose only runtime is runsc, and nftables tables dropping what egress refuses).
- **`/work` has no disk quota**: a sandbox can fill the host's docker volume
  until its lifetime ends. The readback refuses more than 256 MiB.
- **sandboxd holds the docker socket**, so a bug in it is root on its host. Its
  surface is its handlers (`server.go`, `box.go`) and `docker.go`; it listens only on one address (on hetzner, an internal docker network only orbit shares)
  or a unix socket (`validListen` refuses every interface)
  and requires the token on every request.
- **A dropped attach does not stop the process**; the lifetime or a destroy
  does.
- Tunnelled traffic is opaque: the proxy sees hostnames, not requests, so
  it cannot tell a download from an upload to an allowed host.

## Sandboxed chats

A sandboxed chat agent (`sandbox: workstation | build`, the built-in `orbit` as a workstation when sandboxd is configured) runs Claude Code in a sandboxd container, at `full` (`bypassPermissions`) unless its `permissions` say otherwise; a workstation has every tool. There is no tool policy, the container is the whole boundary, and everything "The build sandbox" guarantees and does not guarantee holds. In particular the container holds a credential proxy token, not the backend credential: any command the model runs can spend the credential through the proxy while the sandbox lives, and cannot carry it out. A person's own `claude` terminal in a box (docs/workspace.md, direct mode) holds the real subscription credential, by choice; orbit's runs never do. A workstation's egress is the open web (Workstation egress, below), so anything the session read can leave the box, by design: it holds no GitHub token, no orbit secret, and nothing of the person's beyond what they gave the conversation. Pushing, merging, mail and spend stay orbit's actions, under the agent's rules. What crosses back is the workspace tar, unpacked through `os.Root` (`internal/workspace`), so no entry, symlinks included, lands outside the conversation's directory or HOME's project directory; orbit never executes anything from it. orbit's tools (memory, questions, actions) run in orbit's process over the CLI's control channel and do only what the agent's rules allow; the sandbox has no route to orbit's API.

## Workstation egress

`orbit egress` with `ORBIT_EGRESS_PROFILE=workstation` (`internal/egress`, `TestWorkstationProfile`, `TestRebinding`, `TestServerName`, `TestPlainHTTP`) is a workstation's or box's only route out: a laptop's open web, minus the places a box on a fleet host must never reach.

**Guaranteed by egress's code:**

- Any public host on any port is tunnelled (CONNECT), and plain HTTP is forwarded with the URL's host as its `Host`, except SMTP's ports (25, 465, 587), in every profile.
- Never dialed: loopback, RFC 1918 and ULA (every docker network), link-local (`169.254.0.0/16`, cloud metadata included, and `fe80::/10`), CGNAT (`100.64.0.0/10`, every tailnet address, so orbit-db and orbit's internal listener), multicast and reserved ranges (`egress.Public`), and every prefix in `ORBIT_EGRESS_DENY_NETS` (the fleet hosts' own public addresses, where hetzner's public ports and orbit's origin listen).
- Never tunnelled: a name in `ORBIT_EGRESS_DENY_HOSTS` (`name` exactly, `.name` for it and everything under it; the person's zone, so orbit's API, authentik and Infisical), as the CONNECT target, as a plain request's host, or as the TLS server name of a tunnel on 443. On 443 the ClientHello is read before anything is dialed, and a server name other than the name asked for is refused, so a tunnel to any CDN address cannot be fronted to a denied host.
- DNS rebinding: a name is resolved once, its answers are checked, and the checked address is the one dialed; an answer naming any refused range is skipped, and a name with no other answer is refused.

**Not guaranteed:**

- **Data leaves.** The open web is the point: anything the box holds can be sent anywhere public.
- **A non-TLS tunnel, or TLS on another port, is not inspected.** Plain HTTP over a CONNECT tunnel, or TLS to port 8443, can name a denied host in its own `Host` or server name if the address it reaches serves that host (a CDN). Denied hosts behind a CDN still enforce their own authentication (orbit's API checks an authentik token, authentik and Infisical their logins), which is what stands between a box and them there.
- **The deny lists are configuration.** A fleet host's public address missing from `ORBIT_EGRESS_DENY_NETS` is reachable, with every public port on it (hetzner has no firewall); deployment.md lists what each host's egress must carry; the planned nftables tables (deployment.md, Box isolation on a host) drop the same ranges in the kernel for traffic that does not go through a proxy.
- **Abuse from the fleet's address.** Traffic leaves from the host's public IP, so what a box sends is attributed to it.

## Permission levels

A chat agent's `permissions` level (configuration.md, Permissions) decides which of its tools' calls run without a person; it never grants a tool. `full` and `auto` need a sandbox: both let a call run unasked (auto's classifier is the model's judgment, not a boundary), and on orbit's host that call is beside every `ORBIT_*` secret. Without a sandbox the widest level is `edits`, which runs only file edits inside the workspace unasked, and orbit's Read deny rules hold at every level. A level or ceiling an agent raises is a widening, so an `agent:define` that raises one waits for a person. A process whose ceiling is `full` is launched with `--allow-dangerously-skip-permissions`, which lets a set_permission_mode raise it to bypass; orbit sends that only for a level the ceiling allows, and only a sandboxed process gets the flag.

## Boxes and terminals

An agent's box (sandboxd's `box` profile, `internal/sandboxd/box.go`; docs/workspace.md) is a persistent container from the workstation image where a person drives a shell or Claude Code (direct) from the browser. It gives up three of a build sandbox's options, on purpose: the root filesystem is writable, the capabilities are docker's default set less `MKNOD` and `NET_RAW` (not `ALL`), and there is no `no-new-privileges`, so the image's passwordless sudo works: root inside the box is real container root. It keeps the rest: uid 10001 for every exec, no privileged mode, no added capability, no bind mount (only its named volumes: `orbit-box-<agent>-home`, `orbit-box-<agent>-work` and one `orbit-box-<agent>-acct-<slot>` per Claude account, each at its own path), no host namespace, the internal network with the workstation proxy as its only route out, memory, CPU and process limits. `verifyBox` checks each of these on every container it finds under the box's name, so a container of that name sandboxd did not make is refused, and a box never carries `orbit.sandbox`, so the sandbox API, the reaper and the sandbox count never touch it. The runtime is runc until `ORBIT_SANDBOX_BOX_RUNTIME=runsc`; under runc a kernel escape from root in the box is root on the host, which is the accepted cost of "build first".

- **What runs**: a TTY's argv is fixed by sandboxd from a preset (`shell`, `claude`, `login`), a tmux session name orbit derives from the terminal's id and, for an account, the account's slot (12 hex digits); the request has no field for an argv, a user or an option (`TestTTYExecSpec`, `TestAccountTTYSpec`). The other execs are `tmux has-session`, `tmux kill-session` and `kill -HUP <pid>` of the tmux client sandboxd started, with exact targets, and the fixed scripts in `internal/sandboxd/accounts.go` whose only input is a slot.
- **Credentials**: a `claude` terminal without an account carries `CLAUDE_CODE_OAUTH_TOKEN` (the subscription, as a chat sandbox's CLI does); tmux copies it into that session (`update-environment`), and a tmux server first started by such a terminal holds it for every session after. An account terminal receives no subscription credential, and its command runs with every `ANTHROPIC_` variable unset. Any process in the box can read credentials available to its account or the subscription token. No GitHub token reaches the box yet.
- **Claude accounts**: an account's login (its `.credentials.json`, with an access token and a refresh token that rotates) lives only in its own volume in the box, written there by `claude auth login` in a terminal; it is never in orbit's database, its logs, git, or a request sandboxd answers. orbit's `box_accounts` row holds what `statusScript` selects with jq inside the box (email, organization, subscription type, rate-limit tier, expiries, whether a refresh token exists), and sandboxd decodes that output strictly, so an unknown field is an error rather than passed on (`TestAccountScripts` feeds it files holding tokens and checks none comes out). A terminal under an account is given no credential (`TTYRequest.Validate` refuses one), and its command runs with no subscription token and every `ANTHROPIC_` variable unset. Kanishk accepts a full login in his box: any process there, a model's included, can read every account's credentials, as it can the subscription token. Never copy one login into two directories: the refresh token rotates, and two copies log each other out. Account volumes, like the box's others, are never pruned or removed by orbit. Usage comes from the status line's record (rate limits, no credential) or, with `ORBIT_ACCOUNT_USAGE_POLL` set, from `pollScript`, which reads the access token with jq and hands it to curl on stdin inside the box, so it is in no argv and never reaches sandboxd; sandboxd parses either answer into fixed fields (`sandbox.ParseStatuslineUsage`, `ParseOAuthUsage`).
- The local `orbit sync claude --bundle` export contains only `CLAUDE.md` and skill files. It excludes account state, settings, plugins, and MCP configuration so login state and environment-backed credentials are not part of the bundle. The archive is only a local preparation step; orbit does not upload or install it into a box.
- **The socket**: `GET /api/v1/terminals/{id}/ws` is on the public listener without a bearer token. Its credential is a ticket: 32 random bytes, stored only as a sha256, minted by a person's authentik token through `POST /terminals/{id}/attach`, valid 30 s, used once (a guarded UPDATE), and bound to that terminal and to the Origin it names, or, minted server side without one, to every Origin in `ORBIT_WORKSPACE_ORIGINS`. The socket refuses any Origin outside that list before reading the ticket, so a cross-site page cannot open one. The ticket travels in `Sec-WebSocket-Protocol`, so it stays out of access logs; the `?ticket=` fallback can reach them, which a single use and 30 s make worthless. Tickets are limited to 10 a minute per person. Terminal input is never logged or stored.

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
- **Setup and cleanup scripts run only by content hash.** They are repository code orbit runs in the sandbox outside the tool policy, so each must hash to a value in `trusted_scripts`, which only a person's configuration change adds; a build that changed one on its branch cannot get its version run. They get no credential at all.
- **A kept clone is orbit's**: the checkout pool reuses a ticket's clone across its builds, cleaned before each use, its `.git` written only by orbit's git.
- **A restack is code**: a stacked ticket's branch moved onto a new base is `git rebase --onto` in orbit's clone, pushed with the same one refspec; the read and delivery tokens are minted for it and revoked after, and no model runs.
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
- **Not guaranteed**: the token spends the credential while the sandbox lives (above);
  everything the sandbox section says it does not guarantee holds for a
  build.

## Prechecks

A `precheck` gate runs a shell command from an agent's spec in orbit's own process tree, as orbit's user, before the model is called. Its environment is the fixed list in `engine.precheckEnv` (no secret, no credential), but the same user can read what orbit can, `/proc/<orbit>/environ` included. So a precheck is accepted only in an agent defined in the configuration files, which a person writes and reviews in git: a database agent (written through the API, or by an agent's `agent:define`) with one fails validation, so nothing a model writes ever becomes a command. Its output reaches only the run's events, never a prompt.

## Adding a capability

A new tool grant, action type, channel or source is a security change. Say
in the PR what a compromised model could do with it.
