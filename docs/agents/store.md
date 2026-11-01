# The store

Every database read and write is a method in `internal/store`. Handlers and
the engine never write SQL.

## Migrations

`internal/store/migrations/` is the schema, applied by `orbit serve` at start
(goose, embedded). A migration is additive and never edited once merged. A
change that drops or renames is expand, then contract, across two releases,
because the previous image may still be running during a deploy.

## Retention

Nothing ages out. Rows that look disposable are the record: `run_events` is
the transcript, `artifacts` is what the model saw, `memory_versions` is how
an agent's beliefs changed.

## State machines

- run: `queued -> running -> succeeded | failed`, `queued -> cancelled`,
  `running -> queued` (shutdown, lost worker under `MaxAttempts`).
- action: `pending -> approved | rejected`, `approved -> executing ->
  executed | failed`; `denied` and rule-approved are set at creation.

A client request id (`receipts`) is written in the same transaction as the
message or decision it records, so a retry returns the first result and a
failure leaves no receipt.

A message and a run are in exactly one conversation of their own agent
(`(conversation_id, agent)` foreign keys); a row inserted without one goes
to the agent's default conversation (`orbit_conversation_fill`, which also
moves `last_activity_at`). Every query that reads a thread for a run filters
by the run's conversation. Conversations are archived, never deleted, and
the default one cannot be archived.

Every transition is a guarded UPDATE (`WHERE status = ...`); zero rows
affected is `ErrConflict`, not success.
