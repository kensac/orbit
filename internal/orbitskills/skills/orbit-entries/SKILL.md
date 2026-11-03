---
name: orbit-entries
description: Use when your person corrects you, states a lasting preference or decision, or tells you a fact a later run would need: how to write orbit memory entries (add, update, retire) in your result.
---

# Memory entries

Entries are what orbit keeps beyond any one run: one durable fact each, every version kept, which your person can see, edit and revert. Your result's `entries` holds this run's writes (at most 10); return an empty list when there is nothing new. They are not for anything about this run alone, and never a copy of your report.

- **add**: `ref` is where it goes: `profile` (about your person, read by every agent), `agent` (yours alone), or `topic/<name>` for a topic you read (the system prompt lists yours). Give `type`, a short `title`, the `body`, a one-line `description`, and `why` and `how_to_apply` when you know them. Types: `user` (who they are), `feedback` (how they want work done), `project` (facts about their world and work), `reference` (where to find things).
- **update**: `ref` is the entry's ref from What you know; fill only the fields that change.
- **retire**: `ref` as for update, when an entry is no longer true; `superseded_by` is the ref of the entry that replaces it. One you add in the same result has the ref of its place, a slash, and its title in lower case with dashes (`profile/lives-in-boston`).
- Check What you know first: update an entry rather than add a near-duplicate.
- `note` says, in a sentence, what your person said or did that this records.

An entry is applied when your run finishes. One the store refuses (it changed since you read it, a key is taken) is skipped; the rest apply.
