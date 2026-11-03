# Sandboxed extensions

This document describes the extension design and its implementation boundary. Orbit supports off-by-default deployment-pinned native pure-source operations declared explicitly in an agent's `sources`, plus package-pinned read-only MCP stdio connectors. Extension tools or actions, WASM, install/update commands, durable invocation records, and antivirus signature scanning remain unimplemented. Package code never runs in the service process. The design preserves the [architecture](ARCHITECTURE.md), [security boundaries](agents/security.md) and [configuration rules](agents/configuration.md).

## Implemented first slice

An operator may configure `extensions.enabled: true` and exact `{path, sha256}` package pins in `orbit.yaml`. An agent grants one pinned `source` operation by declaring a source with `kind: extension`, package, operation, static parameters and bindings from every declared input slot to an earlier source. Orbit validates the package and digest while loading configuration, validates parameters and results against the manifest's bounded schema subset, and exposes only the JSON result as that source's input artifact. An extension source is limited to eight invocations per run.

Invocation uses sandboxd's fixed extension profile, not the Orbit process. The profile has no network, credential proxy, backend token, sandbox token, host socket or host mount. It runs as the fixed unprivileged user with a read-only root, no capabilities, no-new-privileges, bounded CPU, memory, process count, tmpfs and wall time. Package and input trees live in a separate root-owned volume whose top directory is not writable or chmod-able by the extension; only bounded tmpfs mounts at `/work/out` and `/tmp` are writable. Each invocation's container is destroyed after completion or failure. The configured package path must remain immutable while Orbit uses it; the digest is rechecked immediately before each invocation.

For an stdio connector, orbit starts the pinned package entrypoint in the same
profile and keeps one sandbox for the connector session. The package must
declare only tool operations, with no inputs or endpoint bindings. Its memory
limit is the lowest declared operation limit. Orbit rejects a changed package
digest, mismatched live tool list, invalid arguments, missing structured
results, schema violations and oversized results. The manifest supplies the
metadata shown to the model. Package traversal, links, file types, size, hash,
manifest fields and hidden Unicode controls are checked; these checks do not
detect malicious program behavior. Operators must inspect a package before
pinning it. MCP result content remains untrusted.

The remaining sections describe protocol guarantees and planned follow-up work. They are not claims that action, tool, package installation, durable audit, worker eligibility or adversarial real-container acceptance coverage exists.

## Decision and scope

Use one versioned JSON protocol for short-lived, networkless sandbox processes. Support native executables first; reserve a runtime tag for WASM with the same protocol, but reject it until a pinned runtime and its isolation tests exist. A native executable can reuse the container boundary; WASM can improve portability and resource accounting later but is not a substitute for that boundary. Neither runtime loads code into orbit, shares a process with a run, or receives a backend credential.

Extensions perform deterministic parsing, reduction, validation and payload construction. The harness fetches remote inputs and executes approved external writes using deployment configuration. This deliberately excludes arbitrary SDK calls that require handing an extension a secret or unrestricted network access. New service integrations that cannot fit a reviewed fetch or action adapter still require core code. No changes to credential proxies, agent self-edit rights, or deployment are part of this design.

## Package and manifest

An operator installs a package containing `manifest.json`, its entrypoint and any declared static files. Installation accepts only regular files beneath the package root: reject absolute paths, traversal, symlinks, hard links, devices, executable search through PATH, and install hooks. Native entrypoints must be Linux executables for the sandbox image's architecture. No package downloads or dependency installation occur at invocation time. Package bytes and the pinned sandbox image are addressed by SHA-256; a changed byte requires a new installation record and explicit deployment pin.

The manifest fields are `protocol` (exactly `orbit.extension.v1`), `id`, `version`, `runtime` (`native`, with `wasm` reserved), `entrypoint` (relative path), and `operations`. IDs and operation names use the same restricted lowercase-name syntax as agents. Each operation declares `name`, `kind` (`source`, `action`, `tool`), a short description, inline input and output schemas, and limits. Unknown fields, duplicate names or JSON keys, unsupported versions, remote schema references, recursive references and conflicting registrations fail validation. The bounded schema subset supports objects, arrays, strings, booleans, null, integers and decimal numbers; required fields, enums, numeric bounds and explicit length/item bounds; `additionalProperties: false` on objects. The first runtime invokes only `source` operations. No coercion or extension-supplied validator code runs in the harness.

The manifest declares required named input slots and, for an action, at most one named deployment endpoint binding. It contains neither URLs nor environment-variable references nor secrets. An endpoint binding is an opaque operation-local slot that deployment maps to a fixed URL, method, credentials and payload schema. An operation cannot request a different destination through its parameters or result. Tool descriptions and schemas are operator-reviewed package content; results are untrusted data and confer no permission.

## Deployment pins and agent grants

Deployment configuration would map an installed package ID to its exact version, package digest, sandbox image digest, enabled operations, reduced limits, and named input/endpoint bindings. Local paths are installation inputs, never agent parameters. Config loading verifies every digest, binding and schema before publishing a configuration revision. An unavailable extension fails configuration validation; it does not fall back to a host command. Agents cannot install packages, change pins, register operations or introduce endpoint bindings.

An agent must explicitly select a source operation, explicitly grant a tool operation, or name an action type and rule. Proposed names are `plugin:<package>:<operation>`; they cannot shadow built-ins. The deployment allowlist and agent selection are both required. Actions remain registered types in `internal/actions`, with a core adapter providing their strict parameter validator and executor; an absent rule is `deny`, and denied actions are never offered. Tools are read-only and cannot dispatch actions. Every external write, even one requested through a tool's result, must become a normal stored action proposal and pass its rule. Existing agent-definition authority remains unchanged; deployment pins and bindings are the ceiling even if an agent definition changes.

Source operations receive only their configured, already-fetched inputs before model execution. They return a derived source artifact that existing transforms and gates can consume. They cannot fetch URLs themselves. Read-only tool operations receive bounded copies of the exact run inputs authorized for that invocation, never the whole persistent workspace, session home or another conversation. Core code resolves and copies files; extensions choose no host paths. A tool returns data or derived run artifacts only, without enqueueing runs, editing memory, changing configuration or causing any external effect.

An action operation runs only after approval or an `auto` rule has authorized its exact parameters. It computes an endpoint payload without performing the write; the core action adapter validates that payload against the deployment's schema, then sends it to the one configured endpoint. The recorded approval binds the parameter bytes, package and manifest digests, operation, configuration revision and endpoint-binding identity. If a pin or destination changes while approval waits, execution refuses the action and requires a fresh proposal, rather than silently using new code or a new destination. The current interrupted-action policy remains: do not automatically retry an uncertain external write.

## Invocation protocol

One process handles one invocation. The harness writes one UTF-8 JSON request to stdin and closes it. The request contains `protocol`, a harness-minted invocation ID, operation name, strictly validated parameters, explicit logical time/timezone when needed, and input descriptors naming sandbox-relative files plus their media types, byte lengths and SHA-256 values. Input file names are minted by the harness. No database URL, token, source header, endpoint URL, host path or environment expansion crosses this boundary. The environment is a fixed locale and minimal PATH, not orbit's environment.

The process writes exactly one JSON response to stdout and exits zero. It echoes protocol and invocation ID, reports either `ok` with a schema-valid result or `error` with a bounded code and message, and may describe output files relative to `/work/out`. Reject trailing bytes, duplicate keys, non-finite numbers, mismatched IDs, unexpected fields, invalid schemas, nonzero exits and missing output. Preserve decimal literals when decoding numbers so reducers never round through binary floating point by accident. Stderr is bounded diagnostic data, never interpreted as protocol or instructions. Errors identify the package and operation without including credentials.

The harness validates and copies each declared output from the sandbox: regular files under `/work/out` only, no traversal, links or special files, and matching declared length and hash. The extension cannot name artifact storage paths; core code supplies stable artifact names. It may write scratch data within its invocation workdir, but neither tool results nor output-file declarations authorize writes outside it. No shell interpolation is involved in invocation: argv contains the pinned absolute entrypoint and fixed protocol argument only.

Given identical package bytes, request and input bytes, an operation must produce identical output bytes. Time is an explicit request field, and randomness, host clocks and environment discovery are outside the contract. Native containment cannot prove determinism; fixture replay detects divergence and disables an operation that fails its conformance checks. Deterministic fetch/filter/join/total/diff decisions remain code, with no model call added by the extension protocol.

## Inert source envelope codec

`internal/extensionproto` implements only bounded encoding and decoding of the ordinary-source envelope below. It reads no descriptor paths, creates no artifacts, validates no schema instances and executes no package. Its successful validation grants no authority. Operation selection and declared input-slot names are explicit decoder arguments; the caller must obtain them from the reviewed source operation. Manifest validation shares its duplicate-key, nesting and exact-number helpers through `internal/jsonstrict`.

The request is an object with exactly these required, non-null fields:

```json
{"protocol":"orbit.extension.v1","invocation_id":"inv_1","operation":"reduce","parameters":{},"inputs":[]}
```

`invocation_id` is 1 through 128 ASCII letters, digits, underscores or hyphens. `operation` and logical file names use the manifest's lowercase-name syntax. The request operation must equal the selected operation, and its input names must match the declared slots exactly, with no duplicates. `parameters` is any non-null JSON value at the protocol layer; the runtime validates it against the selected manifest schema before invoking the package. This slice carries no clock fields; an operation requiring logical time cannot be invoked through this contract yet.

A response echoes the exact protocol and invocation ID and uses one of these exclusive forms:

```json
{"protocol":"orbit.extension.v1","invocation_id":"inv_1","status":"ok","result":null,"outputs":[]}
{"protocol":"orbit.extension.v1","invocation_id":"inv_1","status":"error","error":{"code":"bad-input","message":"Invalid data"}}
```

`ok` requires a present `result` (including null for a future null schema), forbids `error`, and permits optional non-null `outputs`. `error` requires only an `error` object with exactly non-null `code` and `message` fields, forbids `result`, and permits no output files. Its code uses the lowercase-name syntax; its message is nonblank and at most 4096 UTF-8 bytes. Empty `outputs` is permitted on an error response. Diagnostics remain untrusted text.

Each input or output descriptor has exactly these required, non-null fields:

```json
{"name":"data","path":"inputs/data.json","media_type":"application/json","bytes":3,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
```

Paths are clean relative slash paths of at most 256 UTF-8 bytes, with no absolute path, parent traversal, backslash or NUL. `.` is rejected. Input paths are relative to the invocation's input root; output paths are relative to `/work/out`. Names and paths are unique within each list. `media_type` is nonempty, at most 128 UTF-8 bytes and contains no CR, LF or NUL. It is metadata, not a content-type verification. `bytes` is an integer from zero through 16 MiB; each list has at most 32 files and 16 MiB declared total bytes. `sha256` is exactly 64 lowercase hexadecimal characters. The first runtime passes input files from a sanitized archive and rejects output files; protocol output descriptors are not yet copied or retained.

Both envelopes are bounded to 1 MiB while reading (the reader consumes at most one extra byte to detect overflow). JSON must be UTF-8, contain one value with only trailing whitespace, use exact case-sensitive field names, and have no duplicate keys, unknown envelope/descriptor/error fields or nesting beyond the shared 64-level JSON limit. The runtime validates parameter and result instances against the manifest's schema before exposing or invoking them. Decimal literals are retained as raw JSON; numbers use the manifest's bounded exact-rational policy (at most 128 text bytes and exponents from -308 through 308), never binary floating point. Encoding the same struct produces identical bytes; this does not prove a package's determinism.

Run `go test -race ./internal/extensionproto ./internal/extensions ./cmd/orbitextension` for codec adversarial cases and existing manifest/package regression coverage. These checks establish no process, isolation, cancellation, stderr, durable record, grant or external-action behavior.

## Resource limits and isolation

The following are proposed initial hard ceilings, subject to measurement before implementation is accepted. Deployment and manifest values may lower them; effective limits are their minimum and the remaining run deadline. A limit is enforced while reading or executing, not after buffering unbounded data.

| Resource | Ceiling | Reason |
|---|---|---|
| Manifest and schemas together | 256 KiB, 32 operations, schema depth 16 | Keep config validation and offered tool descriptions bounded |
| Installed package | 32 MiB, 256 files | Bound distribution and unpacking; reject oversized native dependencies |
| Request JSON / response JSON | 1 MiB each | Match the existing sandbox transport's maximum frame size |
| Input files / output files | 16 MiB total each, 32 files each | Allow moderate tabular reducers while bounding copy and artifact storage |
| Stderr | 64 KiB | Keep useful failure diagnostics without log flooding |
| Invocation | 30 s wall time, one CPU, 256 MiB memory, 32 processes | Isolate interactive work; longer batch jobs need a separately reviewed limit change |
| Invocations | 8 per run, 2 concurrent per host | Bound fan-out and keep extensions from starving ordinary work |

Use a new fixed extension profile in sandboxd, separate from the existing build profile. It needs a read-only root, no network device or egress proxy, no capabilities, no-new-privileges, seccomp, fixed non-root identity, bounded tmpfs, read-only package/input files and a fresh invocation workdir. The extension receives no Docker socket, host mount, database, backend credential, sandbox token, session state or other invocation's data. Orbit's model process cannot attach to this sandbox. The profile exposes no caller-chosen container options, mounts, environment or arbitrary argv. Existing build-container guarantees alone are insufficient: build network access and resource limits are broader than this proposal permits.

Timeout, cancellation, lost run ownership, output overflow or a process limit kills the whole process tree and records a typed failure. The action adapter must recheck its claim before any external effect; late extension output cannot revive a cancelled or superseded claim. A host semaphore limits local concurrency, and extension jobs use separate capacity from build sandboxes; unsupported or exhausted hosts report unavailable rather than running extensions on the host. Worker eligibility requires the pinned digest and extension profile capability. Control remains the only external-action executor.

## Audit, retention and operations

Store invocation identity, run attempt or action identity, configuration revision, package/manifest/image digests, operation, effective limits, input hashes, output bytes/hashes, duration, exit status and bounded diagnostics. All writes go through `internal/store` with the current claim guards. Save inputs and outputs as artifacts and invocation lifecycle as events; define additive timestamped migrations before implementing any new durable records. Never replace or delete stored versions, failures, interrupted attempts or package bytes needed to interpret them. Temporary process files may be cleaned only after durable capture, as run scratch is today.

Disabling an operation stops new invocations and fails waiting actions that name it with a clear reason; existing records remain readable. Upgrades append installation/configuration versions; rollback selects an earlier retained digest without changing old records. No implicit latest version, background updater, marketplace install, or plugin self-update is allowed. Structured logs and metrics identify package, operation, outcome, duration and resource-limit category; do not log raw parameters or inputs by default. Package execution never gains authority from a manifest claiming it is trusted.

## Acceptance tests and delivery boundary

Before any runtime is enabled, require the following evidence in its implementation PRs, following [testing rules](agents/testing.md). This design adds no runnable configuration, migration, HTTP endpoint or deployment change.

1. Manifest/configuration tests reject malformed or duplicate JSON, unknown fields, schema bombs, mismatched digests, unsupported runtimes, shadowed names and undeclared endpoint/input bindings. A missing agent grant denies each kind; action `auto`, `approve` and `deny` preserve the ordinary stored lifecycle.
2. Protocol tests cover golden deterministic fixtures, exact decimal totals, strict schema failures, truncation, trailing stdout, stderr overflow, mismatched invocation IDs, crash, timeout, cancellation and forked children. Replaying the same fixture checks byte-identical results; changing package or config pin invalidates a waiting approval.
3. Real-container isolation tests attempt host and session reads, writes outside scratch, symlink/hard-link escapes, direct and proxied network access, environment/credential discovery, Docker access, process flooding and memory exhaustion. Each is refused or bounded; one malicious invocation cannot inspect or block a concurrent one. The first native runtime slice adds a real-Docker test for read-only package/input/root paths, its two writable tmpfs paths, absent network/proxy/secrets/Docker socket, process capabilities and Docker-inspected memory/process limits, invocation-local scratch, timeout cleanup and no surviving managed container. This is partial evidence for item 3; the broader host/session path, link-escape, process-flood and memory-exhaustion probes remain required before enabling extensions.
4. Store tests against real Postgres race competing claims, cancellation and late results; retain all attempts and artifacts; prove an interrupted action is not retried and an external write occurs only after its recorded authorization. Fake endpoint tests prove the model and extension cannot choose URLs, methods, headers or destinations, and that invalid/oversized payloads cause no write.
5. An end-to-end fixture runs a fetched source through a pure extension and a gate with zero model calls when unchanged, invokes an explicitly granted read tool, then proposes, approves and observes one fixed-endpoint action. Inspect recorded artifacts and failure reasons. This is test evidence, not deployment evidence.

Implementation should begin with manifest validation and a native pure-source invocation behind an off-by-default deployment flag. Add read tools only after isolation and claim tests pass; add action payload adapters only after approval binding and endpoint tests pass. WASM remains rejected until it passes the same suite plus runtime-specific fuel, memory and host-import denial tests. This sequence keeps the reusable boundary reviewable before it can cause an external effect.
