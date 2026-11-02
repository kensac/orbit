# orbit

Standing agents for one person: each has a charter, wakes on a schedule or a
message, reads data the harness fetches for it, keeps memory between runs,
asks questions, and proposes actions that run automatically, wait for
approval, or are refused, by rule. Backends are interchangeable (Claude Code,
Codex, the Anthropic API), and every run's inputs, transcript and outputs are
kept in Postgres.

- Design: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- User guide: [docs/guide/](docs/guide/)
- Working here: [AGENTS.md](AGENTS.md)
- Example configuration: [examples/](examples/)

```
make testdb-up && make check
go run ./cmd/orbit validate examples
```
