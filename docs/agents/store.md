# The store

Every database read and write is a method in `internal/store`. Handlers and
the engine never write SQL.

## Migrations

`internal/store/migrations/` is the schema, applied by `orbit serve` at start
(goose, embedded). A migration is additive and never edited once merged. Name a new migration by UTC timestamp, `YYYYMMDDHHMMSS_name.sql` (`date -u +%Y%m%d%H%M%S`), never the next small number: agents merge in parallel and two of them picking the same number made every orbit start fail. Out-of-order apply is on, and `TestMigrationVersionsAreUnique` fails on a duplicate. A
change that drops or renames is expand, then contract, across two releases,
because the previous image may still be running during a deploy.

- **Expand only.** `store.ExpandFindings` reads a migration's Up section and flags what would break the image still running beside the new one: a `DROP` of a table, column, view, type or default, a `RENAME`, a column type change, `SET NOT NULL`, and `ADD COLUMN ... NOT NULL` without a `DEFAULT`. `TestMigrationsAreExpandOnly` fails on any of them. The contract half, merged once the previous release no longer reads what it drops, carries a `-- orbit:contract: <why>` line; the check then reports it instead of failing. It reads SQL, not data: a new constraint the previous image would violate is caught only by `deploy/compat.sh` below.
- **No `NO TRANSACTION` migrations** (`CREATE INDEX CONCURRENTLY` and the like): `orbit preflight` cannot dry-run them.
- **`orbit preflight [--config DIR]`** (run from the new image against the live database, before the new instance starts; docs/agents/deployment.md, Rolling out): lists the applied, pending and ahead versions (`PlanMigrations`, read only), refuses a pending migration that is not expand-only, then applies every pending one to the live schema and rows in one transaction and rolls it back (`DryRunMigrations`, each lock waited for at most `DryRunLockTimeout`), and with `--config` loads that configuration. It changes nothing.
- **`make compat BASE=<commit>`** (`deploy/compat.sh`, in CI's check job against a pull request's base or the commit a push moved from): the previous release migrates a database and queues a run, this tree preflights and migrates it, and the previous binary then serves on the new schema: ready, two runs claimed and finished, the API answering, a clean drain, and no error logged outside the ticket loop.

## Stored files

`uploads.content` and `artifacts.content` are bytea, never updated. A reader serves one through `store.Blob`, a slice of `blobSlice` bytes per query (`substring`), so a GET holds a slice in memory, not the file, and a `Range` reads only its slices; `uploads.content` is stored uncompressed (`STORAGE EXTERNAL`), because a compressed value is decompressed from its start for every slice. `CreateUpload` finds a free name under a per-conversation advisory lock before its one INSERT, so the content crosses the wire once.

## Retention

Nothing ages out. Rows that look disposable are the record: `run_events` is
the transcript, `artifacts` is what the model saw, `memory_versions` is how
an agent's beliefs changed.

## The journal

`journal_items` is derived from `run_events` by `journal.Project`, a pure function of an attempt's events in order. Keep it pure: the live write (in `AppendEvent`'s transaction) and the backfill (`syncAttempt`, run at start and every 30 s for recent runs, and by a reader before it reads) must write the same rows, and `TestBackfillWritesWhatTheLiveWriterWould` holds them together. Whether a run needs a sync is decided by `unsyncedSQL`: each attempt's newest event is one descent of `run_events`' primary key, compared with `journal_sources.through`, so an up-to-date journal costs a reader one read and no write (`TestAReaderOfAnUpToDateJournalWritesNothing`); never make that check read a run's whole transcript, because the stream runs it every 500 ms per open stream. The backfill skips and reports a run whose sync fails, so one bad run never holds back the others. Rows are append-only: a change is a new revision, never an UPDATE. A body's shape changes only with a new `journal.Version` and an upcaster in `journal.Upcast`; stored rows keep their version. Every journal write of a run holds its advisory lock (`lockJournal`), which is what keeps `seq` gapless. A transition that ends an attempt (`finishRun`, `endAttempt`, `HandBack`) syncs that attempt in its own transaction, so the turn it left open closes with it.

## State machines

- run: `queued -> running -> succeeded | failed | skipped | cancelled` (cancelled: a person stopped it, `cancel_requested_at`), `queued -> cancelled`,
  `running -> queued` (a hand-back, a lost worker under `MaxAttempts`).
  Every write by a running attempt also guards `attempt = <its own>`, so an
  attempt superseded by a newer claim writes nothing. `skipped` is a run that ended without a backend call, and names what ended it in `gate` (`runs_skipped_names_gate`): an agent's gate (`unchanged`, `items_above`; configuration.md), a schedule fire claimed past its grace (`missed`; configuration.md, Triggers) or a build that delivered an earlier attempt's pushed commit (`pushed`). It counts as a success for staleness (`orbit_agent_last_success_timestamp_seconds`, `/health`'s `last_success`), except `missed`, where the agent did nothing; `orbit_runs_skipped_total` counts them by gate.
- attempt (`run_attempts`, one per claim, written in `ClaimRun`'s
  transaction): open, then `succeeded | failed | skipped` with its run, `handed_back`
  (`HandBack`) or `lost` (`ReapStale`).
- message read (`message_reads`, one per attempt that read a person's message, written in `StartRunContext`'s transaction): open, then `released_at` when the attempt hands the message back (`releaseMessages`: a failure, a requeue). At most one is open per message (`message_reads_one_holder`); a message with none open waits for the next run (`waitingMessage`). `messages.run_id` names the last run that read it and is never cleared by this image. A message marked read with no read row (`giveComments`, which gives a build its review comments when it is enqueued, or the previous image during a deploy) is held, and `releaseMessages` records it as handed back; the previous image's hand-back still clears `run_id`, which `waitingMessage` treats as waiting.
- action: `pending -> approved | rejected`, `approved -> executing ->
  executed | failed`; `denied` and rule-approved are set at creation.
- ticket: `ticketEdges` in `tickets.go` is the table (diagram in
  docs/tickets.md); into `planned` and `approved` only through
  `ProposePlan`/`ApprovePlan`, which carry the plan version. Each transition
  writes its `ticket_events` row in the same transaction.
- race (`ticket_lanes`, `tickets.race`, `tickets.picked_lane`): `StartRace` enqueues every lane's build and moves its ticket to `building` in one transaction; the last lane to end moves it on in its own transaction (`settleRace`); `PickLane` writes the executed `ticket:pick` action and gives the ticket the lane's branch, PR and head in one transaction, only while it is a race in review with no pick. `picked_lane` is set once and never cleared.
- build: `StartBuild` enqueues a build run and moves its ticket to
  `building` in one transaction; `FinishBuild` and `FailBuild` finish the
  run and move the ticket together. `build_decisions` and `build_sandboxes`
  rows are written only by the attempt holding the run's claim.
- review comment: `draft -> submitted -> delivered`, or `withdrawn` (a draft) or `superseded` (an edit, a new row). The status is derived (`commentSelect`): `message_id` says submitted, a succeeded or skipped run in `ticket_build_comments` says delivered. `StartBuild` gives a build every comment still waiting, in its transaction.

- attention (`attention`): written with its thing (`newAttention`: `finishRun` for a failure, `insertAction` for a pending approval, `RecordOutcome` for a question, `proposePlan` for a plan, `Delegate` for a task's decision), one row per `key`; open is computed from the thing (`attentionOpen`), never stored, so no decision path has to remember to close it. `pushed_at` is set once by `ClaimPush`; `dismissed_at` once by a person.
- push subscriptions (`push_subscriptions`): revoked, never deleted; the same endpoint subscribing again revives its row.
- task (`tasks`, `tasks.go`): `waiting | blocked -> working -> needs_input | done | failed | cancelled`, `needs_input -> working`, `done -> working` (a parent's message reopens it), and `cancelled` from any open state. `Delegate` creates a batch; `ReconcileTasks` (one instance at a time, by advisory lock) makes every other move but `MessageTask` and `CancelTask`, each in one transaction. `report` is kept when a task reopens.

- backend hold (`backend_holds`): open (one per runner, `backend_holds_one_open`; a second failure of the same cause inserts nothing), then closed once with `closed_by` (`probe`, or `run:<id>` for a person's run that succeeded); `ClaimProbe` moves `next_probe_at` so instances probe once per interval. Rows are kept. `ClaimRunFor` skips the unattended runs of agents whose spec's runner has an open hold.
- outbox (`outbox`, written by a worker's engine): unsent, claimed by one control instance (`ClaimOutbox`, reclaimed after `OutboxReclaimAfter`), then `sent_at` set once its deliveries are recorded. Rows are kept.
- read points (`conversations.read_through`): moved only forward and never past now (`MarkRead`, a guarded `GREATEST`). The board's states are derived from rows by `BoardState`, code, never stored.
- change feed (`changes`): derived, not written by any store method. Triggers on `runs`, `messages`, `attention`, `actions`, `tasks` and `conversations` insert one small record per change a client showing every conversation cares about, with the writing transaction's `xid`. Readers (`ChangesAfter`) return only records whose transaction is older than every open one (`pg_snapshot_xmin`), in `(xid, id)` order, because a `bigserial` alone is allocated before commit and a late commit would be skipped by a reader that already passed its id. A transaction left open therefore delays the feed for everyone; keep transactions short. Rows are kept.
- workspaces (`workspaces`): a chat conversation's snapshots, never updated; the newest is the workspace's content. A run's (`source = 'run'`, after each run that changed it, or after a person's write even unchanged, which records that the agent saw it) or a person's (`source = 'person'`, `PutPersonWrite`: no run, refused while a run of the conversation is `running`, and only on top of the snapshot it was made from). The write and a run's restore (`LatestWorkspace`) take the conversation's advisory lock (`workspaceLock`), exclusive and shared.
- workspace trash (`workspace_trash`): written with the person's snapshot that removed the path; its content is the snapshot before (`snapshot_id`). `restored_at` is the only update, set by each restore. There is no delete.
- workspace identity (`workspace_trees`): one row per conversation of a chat agent (`scope = 'conversation'`, made on first ask or with the first snapshot, `PutWorkspace`) or per agent (`scope = 'agent'`, one per agent by `workspace_trees_one_per_agent`; nothing writes one yet); never updated. Its id is a snapshot workspace's `workspace_id`; a box workspace's id is its `boxes` row's.
- workspace audit (`workspace_events`): one row per workspace operation, append-only, written in the write's transaction; a signed upload's `nonce` is unique, so its URL uploads once.
- boxes and terminals (`boxes`, `terminals`, `terminal_tickets`, `terminals.go`): a box row per agent, created on first use, never updated. A terminal is running until `EndTerminal` sets `ended_at` and `end_reason` once (`WHERE ended_at IS NULL`; `terminals_ended` holds them together); `CreateTerminal` counts the running ones under the box row's lock. A ticket is stored as its sha256 and used once by a guarded UPDATE (`used_at IS NULL AND expires_at > now()`, the terminal and origin matching); used and expired rows are kept.
- session transcripts (`session_transcripts`): written once per session id (`ON CONFLICT DO NOTHING`; a resume forks to a new id), never updated.
- imported sessions (`external_sessions`, `external_session_lines`, `external_line_contents`): a line's content is inserted once per digest and never updated; a session's lines are appended per generation and never updated or removed; the session row is updated only by `ImportSessions`, under `FOR UPDATE`, and only when the import continues its stored `offset_bytes` and `prefix_sha256` (or starts a new generation), so a stale client conflicts instead of interleaving.
- deployment (`deployment_specs`, `deployment_current`): every version kept, as `agent_specs`; `SyncDeployment` moves the pointer before `SyncAgents` records the agents loaded with it.

- search (`search_docs`): derived, not written by any store method. Triggers on `messages`, `runs` (summary, report, error), `run_events` (the backends' prose and tool calls), `memory_versions`, `memory_entry_versions` and `external_session_lines` (an imported session's prose lines) insert one document per searchable piece, keyed by the row it came from (`ref`), so every writer indexes alike; a document that cannot be built raises a warning and never fails the write. A new searchable table gets its trigger and its backfill in one migration.

- usage samples (`usage_samples`): one row per poll of the subscription usage endpoint, `ok` with the body as received or not with the error; parsed when read (`usage.ParsePoll`), never rewritten.
- deferral (`run_deferrals`, one per run): `DeferRun` pushes a queued schedule run's `run_after` and upserts its row (`times`, the window and reading) in one transaction, refusing a run no longer queued; `ReleaseDeferrals` clears `run_after` and sets `released_at` together. A run is held only while queued, so a claim is never blocked by anything but `run_after`.

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
