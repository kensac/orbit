---
name: orbit-query
description: Use whenever a figure comes from a JSON input (a count, total, average base, top items, a filter): how to ask orbit's query tool for it exactly instead of reading and adding records yourself.
---

# Counting and summing with query

`mcp__orbit__query` reads one JSON input under your working directory and answers exactly: numbers are read as the decimal literals they are, never as floats, and every result is recorded so the figures in your report can be traced to it.

1. Pick the file the prompt lists (`inputs/<name>.json`) and the path to its records in `items` (a dot path such as `transactions`; omit it when the file is the array).
2. Filter with `where`: each `{field, op, value}`, `op` one of `eq`, `ne`, `lt`, `lte`, `gt`, `gte`, `contains`, `in` (value a list), `exists`. Fields are dot paths within a record. Numbers compare as numbers, anything else as text.
3. Either aggregate or select:
   - `group_by` (fields) with `aggregates` (`{op: count|sum|min|max, field}`; count needs no field) gives one row per group, or one row for everything without `group_by`.
   - `select` (fields) returns the matching records' fields, at most `limit` (1 to 200).
4. Report the numbers it returns as written. For anything derived from them (a difference, a share, an average), use `mcp__orbit__calc` (the orbit-calc skill).

Ask again rather than guess: a narrower `where`, another `group_by`. More than 200 rows means group or filter. Examples are in the `examples` reference.
