# Roadmap

This is orbit's ordered product plan. [WORKBOARD.md](WORKBOARD.md) records implementation status and ownership; update it as work lands. Kanishk sets the order.

The product goal is one coherent orbit surface at kanishksachdev.com/orbit: chat, workspace, editor, terminals, diffs, agents, memory, and accounts should feel like parts of one app.

## First: finish the current portfolio work

Complete the portfolio changes and integrations listed in the workboard, in dependency order: the shared workspace shell, explorer, editor, diffs, and terminal experience. The orbit-side routes and terminal pieces have their own built and open status in the workboard and [workspace plan](workspace.md).

## Next: use Claude Code from the browser

Finish the direct terminal experience, account switching, and usage display in the portfolio. Keep a person's direct terminal login separate from orbit's credential proxy for unattended runs. Add connector setup and status after the account work.

## Then: expand agent capabilities

Build out templates, typed actions and sources, event triggers, and agents that can create agents and schedules through rule-gated actions. Skill writes already use the `skill:write` action and an agent's rules; the workboard tracks that capability and the remaining template work.

## Later

Evaluate gVisor isolation for boxes on hetzner and oci, improve token and disk efficiency, add voice and messaging channels, and deploy the worker fleet. These are roadmap items, not claims about current production availability.

## Ongoing

Keep implementation maintainable: one decoder, shared protocol code, store-only SQL, and documentation that matches the code. The workboard's Hygiene row tracks that work.

## Deployment status

The workboard distinguishes code that is built from code that is deployed. Check the homelab configuration for the current production pin; a merge or green check alone does not establish production availability.
