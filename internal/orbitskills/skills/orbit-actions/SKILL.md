---
name: orbit-actions
description: Use before proposing any action (notify, webhook, delegate, agent:define, skill:write, a merge): how orbit's actions, rules and approvals work, and how to propose one so it is carried out.
---

# Actions

You never act outside your run yourself: you propose an action in your result's `actions` (a chat agent calls the `act` tool), and orbit carries it out by code if its rule allows. Each type has a schema for its `params`; the system prompt lists the types you have, each with its rule.

- **Rules.** `auto` runs when your run finishes. `approve` waits for your person, who sees your `reason` and the params (in a run answering their own message, some types apply at once because they asked). A type you do not have is not offered and cannot be proposed.
- **reason** is what your person reads when deciding: say what it does and why now, in a sentence or two.
- **Never claim it happened.** Proposing is all you can do. Say in your report that you proposed it and what waits for approval.
- **One malformed proposal is denied on its own**, with the reason recorded; the rest of your result stands. Propose it again, fixed, in a later run.
- **Limits.** At most 20 proposals a run. Some types check more when proposed or executed (a merge re-checks the PR, an agent change that widens an agent waits for a person whatever the rule).
- **Results.** What an executed action returned is on it (`GET /actions/{id}`); a later run sees its outcome in the conversation.

## Writing a skill (skill:write)

Write one when you have worked out a procedure your person will want done the same way again, or improve one you have. `op` `write` gives `content`, the whole `SKILL.md`: frontmatter with `name` (the skill's) and a `description` saying when to use it, then the steps. A skill you have keeps its other files; a new one has none. `op` `retire` ends one you have (`content` empty). `note` says, in a sentence, why. At most 3 a run; the person approving it sees the change line by line and every script the skill holds.
