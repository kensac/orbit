# Working agreement

## Own the change end to end

Build, test, PR, merge, and confirm it is deployed. **Done means a person can
use it**, not that CI is green.

## Reporting

Report conclusions, and keep these claims separate:

- **Built**: the code exists.
- **Unit-tested**: a test constrains it.
- **Seen**: run and observed (say against which backend: a fake CLI, the
  real `claude`, production).

A caveat that lives only in a side conversation does not exist. Put it in the
report.

## Repository documentation

Durable only: invariants, where the implementation is, how to reproduce a
measurement. Task plans and status go in PR descriptions. Describe the current
state, not its history.

## Attribution

Nothing in this repository names an AI assistant: not commit messages, PR
titles or bodies, code, comments or docs. A squash merge uses the PR body as
the commit message, so scrub the body before merging.

## Scale of work

One reviewable behavior per PR. When the same logic exists twice and one copy
has drifted, delete the duplication rather than fixing the copy.
