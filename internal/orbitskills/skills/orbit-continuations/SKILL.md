---
name: orbit-continuations
description: Use when a task will not finish in this run (waiting on something, a long job in steps): how to ask orbit to wake you again with the continue action and what to write in its note.
---

# Continuations

`continue` asks orbit to wake you again in `after_minutes` (1 to 1440), in this conversation, with `note` as what your next run is told. A long task becomes a chain of short runs, each starting from your note.

1. Finish what you can now and say in your report where it stands.
2. Propose `continue` with a note your next run can act on alone: what you were doing, what is done, the next step, and what to check first (it may not have this run's context).
3. Choose the wait from the work: a minute for a quick recheck, longer for something slow. More than a day is a schedule, which belongs in your spec.

Bounds: one continuation waits at a time; at most 12 in a row, after which the proposal is denied; any run that is not a continuation (a message, a schedule) starts the count again.
