# Roadmap

The ordered plan for orbit. [WORKBOARD.md](WORKBOARD.md) says who owns what right now; this file says what comes next and why. Kanishk sets the order; update it in the same PR as the work.

The bar for everything below: orbit at kanishksachdev.com/orbit is the only surface, and it must feel like one clean app (chat, workspace, editor, terminals, diffs, agents, memory, accounts) rather than panels bolted together.

## Now: ship what is built

Production runs an image from before most of the merged work (delegation, memory and skills, push and inbox, search, hooks, schedules, reviews, stacked PRs). The next pin waits on the agent-definition security fix (#71). Then: pin and roll out, import Kanishk's Claude Code memories and skills, turn on push, give builder repository access.

## Next: a laptop-grade Claude Code in the browser

1. **Terminal MVP.** A persistent box per agent from the workstation image, a TTY over a WebSocket, and "Claude Code (direct)": the real interactive `claude` in the portfolio (orbit server: in progress; portfolio terminal panel next).
2. **Unified workspace shell** in the portfolio: one layout, palette, tokens and `open(ref)` that every panel plugs into (in progress).
3. **Chat that shows its work:** the real answer instead of a stub (in progress), the live journal stream, grouped tool runs with a changed-files rollup, answerable question cards, a visible queue, Stop that stops.
4. **Files, editor, diffs:** explorer, CodeMirror 6 editor, diff view with line comments to the agent (docs/workspace.md W1 to W8).
5. **Accounts and connectors:** one Claude config per account with one-click switching and usage bars; full `/login` in the box so claude.ai connectors, plugins, skills and MCP servers load as on a laptop; `gh` authenticated in every box.
6. **Isolation:** gVisor for boxes on hetzner and oci, egress rules, then the credential proxy.

## Then: agents that act

Connectors for agents (orbit holds the credentials, read-only tools run freely, writes need approval), agents that create agents and schedules by talking, templates, typed actions (Sure, Gmail drafts, Calendar, GitHub comments), event triggers, steering a running reply, a fleet board of every conversation's state.

## Later

A token and disk efficiency pass (cache-hit rate per run, a stable prompt prefix), race builds, voice, more channels, deploying the worker fleet.

## Ongoing

Maintainability work runs alongside: one decoder, shared protocol code, store-only SQL, docs that match the code. The coherence audit's list is in the workboard's Hygiene row.
