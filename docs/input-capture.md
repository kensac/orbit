# Captured evaluation inputs

`go run ./cmd/orbitcapture [flags] capture RUN ATTEMPT NEW_DIRECTORY` saves only the model-visible inputs from one explicit positive attempt. Set `ORBIT_URL`, `ORBIT_OIDC_ISSUER` and `ORBIT_OIDC_CLIENT_ID` for the public HTTPS API and device provider. Each invocation signs in separately and keeps its token in memory. The overall timeout defaults to five minutes and cannot exceed ten.

The command reads `GET /runs/{id}/artifacts` and then the selected `GET /artifacts/{id}` contents. Only `direction: in` names under canonical `inputs/` paths qualify. This includes recorded durable memory when the run received it as an `inputs/_memory.md` artifact (possibly under a session prefix). It never fetches `sources/` content, runs a backend, refetches a source or applies proposed changes. No database access is involved.

The new directory is private (0700) and files are private (0600). Existing paths are refused. Fixed `input-NNN.bin` filenames avoid interpreting API names as local paths. `manifest.json` maps those files to the original run, attempt, artifact ID, name, media type, byte count and SHA256. Every saved input must match the API's declared size and digest. These hashes establish agreement with the API response, not independent authenticity.

Capture is bounded to two minutes after sign-in, a 2 MiB listing, fewer than 128 listing rows, 16 MiB per input and 64 MiB total input content. A listing at the row cap fails because completeness cannot be established. The listing endpoint has no pagination or snapshot token; server-side omissions cannot be detected. Separate reads are not an atomic snapshot.

A remote failure, cancellation, invalid metadata or bound violation leaves a sanitized incomplete manifest and already verified inputs, and exits nonzero. Local filesystem failures can prevent the manifest from being written. Inspect `complete` before using any material. A failed directory is retained; choose a new directory to retry.

This is capture material for authoring an evaluation fixture, not a runnable evaluation suite. Historical charter, memory outside the recorded inputs, messages, skills, session state, output and expected properties are absent. `orbiteval --replay` can run an authored case against the current charter and active skills, but it has no sources, memory, messages, tools, connectors, hooks, sessions or actions. Neither complete engine equivalence nor reproducible model output follows from this capture. Supply and review deterministic expectations separately before evaluation.
