---
name: orbit-calc
description: Use for any arithmetic a report states (a difference, a percentage, an average, a projection): how to compute it exactly with orbit's calc tool rather than in your head.
---

# Arithmetic with calc

`mcp__orbit__calc` evaluates `expression`: decimal numbers with `+ - * /` and parentheses, exactly, over rationals. A quotient that does not terminate is rounded and the result says so. Every result is recorded, so a figure you report can be traced to the call that made it.

- Take operands from your inputs or from `mcp__orbit__query`'s results, as written: `(5914.20 - 4380.15) / 4380.15 * 100` for a change in percent.
- One expression per figure you report; do not chain results in your head between calls.
- Round only when you write the figure, and say what was rounded.
- A dollar amount in your summary or report that is in neither your inputs nor a tool's result may be sent back to you to recompute.
