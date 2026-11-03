# Orbit agent packs

`orbit pack <agent> --output <directory>` exports one currently defined agent and its active Orbit library skills as a local Claude Code plugin. The output directory must be new. The command reads the agent and skill library through the Orbit API and does not change Orbit.

Start the plugin from its directory with `claude --plugin-dir .`. Claude Code exposes the agent as a plugin subagent. Review `orbit.json` before running it. `orbit pack inspect <directory>` verifies the versioned manifest, embedded agent digest, file inventory, paths, symlinks, and skill digests without contacting Orbit.

The manifest preserves the full agent spec, including its sources, explicit action and tool grants, and skill references. It contains environment variable names referenced by sources but never their values. This package excludes schedules outside the agent spec, connectors' endpoint definitions, credentials, Orbit memory, conversations, and account state. Inline credential-shaped source values are rejected rather than exported. A built-in skill is exportable only when it is also available in the Orbit skill library.

Claude Code does not enforce Orbit grants. The exported subagent is restricted to `Read`, `Glob`, and `Grep`; Orbit grants remain review metadata and do not turn into Claude Code permissions. The pack does not copy Orbit hooks or configure an Orbit MCP server. Configure any external connection and credentials separately, after reviewing the package.

The plugin directory contains `.claude-plugin/plugin.json`, `agents/<agent>.md`, `skills/<skill>/`, `README.md`, and `orbit.json`. Files are written with owner-only permissions. Output is staged in a sibling temporary directory and published by rename; an existing output path is refused.
