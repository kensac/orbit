# Engineering standards

## Correctness first

- No workarounds. If a design is wrong, change the design.
- **A write that must be atomic is one transaction in `internal/store`.** A run
  succeeds together with its memory, questions and actions (`RecordOutcome`);
  a failed or requeued run hands its messages back in the same transaction.
- **Invariants the schema can hold, it holds**: one running run per agent
  (`runs_one_running`), one run per schedule fire (`runs_one_per_fire`). Code
  that relies on one says which index enforces it.
- Errors carry their cause to the person: a failed run's `error` is what the
  notification shows, so it names the source, the backend message, or the
  schema location that failed.

## Constants

Name where a number comes from beside it (a downstream limit, a measured
size, another constant it must stay inside). `heartbeatEvery` and `StaleAfter`
are the worked example: one is derived from the other.

## Comments

Short statements of the current rule and its reason; what breaks if it is
reversed. No narration of history. A comment that is wrong is fixed when
found.

## Dependencies

`go.mod`/`go.sum` and `deploy/cli/package-lock.json` are committed; the image
installs exactly them. A CLI version bump is reviewed, never automerged
(`renovate.json`): orbit parses the CLI's flags and event stream, and a
release can change either. Re-run the real-CLI smoke in testing.md after one.

## Go

`gofmt`, `go vet`, staticcheck and golangci-lint (`.golangci.yml`) are gates.
Prefer the standard library. Exported identifiers are documented.
