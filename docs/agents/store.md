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

- run: `queued -> running -> succeeded | failed | skipped`, `queued -> cancelled`,
  `running -> queued` (a hand-back, a lost worker under `MaxAttempts`).
  Every write by a running attempt also guards `attempt = <its own>`, so an
  attempt superseded by a newer claim writes nothing. `skipped` is a run
  that ended without a backend call, and names what ended it in `gate`
  (`runs_skipped_names_gate`): a build that delivered an earlier attempt's
  pushed commit (`pushed`). It counts as a success for staleness
  (`orbit_agent_last_success_timestamp_seconds`, `/health`'s
  `last_success`); `orbit_runs_skipped_total` counts them by gate.
- attempt (`run_attempts`, one per claim, written in `ClaimRun`'s
  transaction): open, then `succeeded | failed | skipped` with its run, `handed_back`
  (`HandBack`) or `lost` (`ReapStale`).
- action: `pending -> approved | rejected`, `approved -> executing ->
  executed | failed`; `denied` and rule-approved are set at creation.
- ticket: `ticketEdges` in `tickets.go` is the table (diagram in
  docs/tickets.md); into `planned` and `approved` only through
  `ProposePlan`/`ApprovePlan`, which carry the plan version. Each transition
  writes its `ticket_events` row in the same transaction.
- build: `StartBuild` enqueues a build run and moves its ticket to
  `building` in one transaction; `FinishBuild` and `FailBuild` finish the
  run and move the ticket together. `build_decisions` and `build_sandboxes`
  rows are written only by the attempt holding the run's claim.

A client request id (`receipts`) is written in the same transaction as the
message or decision it records, so a retry returns the first result and a
failure leaves no receipt.

A message and a run are in exactly one conversation of their own agent
(`(conversation_id, agent)` foreign keys); a row inserted without one goes
to the agent's default conversation (`orbit_conversation_fill`, which also
moves `last_activity_at`). Every query that reads a thread for a run filters
by the run's conversation. Conversations are archived, never deleted, and
the default one cannot be archived.

A mail (`mail_in`, keyed by Message-ID) is recorded in the same transaction as the message and run it becomes (`IngestMail`), so a mail fetched twice, by two instances or after a crash before it was marked seen, is acted on once. An ignored mail is recorded with its reason; every row keeps the raw mail.

Every transition is a guarded UPDATE (`WHERE status = ...`); zero rows
affected is `ErrConflict`, not success.
