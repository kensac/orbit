package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kensac/orbit/internal/journal"
)

// The journal (internal/journal, migration 00010) is written from
// run_events: each appended event is projected in the transaction that
// stores it, and syncAttempt catches up anything a writer did not project
// (rows from before the journal existed, or written by an instance that
// predates it during a deploy). Projection is a pure function of an
// attempt's events in order, so the two paths write the same rows.

// ErrJournal wraps a projection failure. The event it came from was
// stored; the journal catches up on a later sync.
var ErrJournal = errors.New("journal")

// syncPage bounds the events one read projects; a sync loops until caught up.
const syncPage = 500

// lockJournal serializes every journal write of one run: the live writer,
// a closing transition and a sync on another instance would otherwise race
// for the next seq.
func lockJournal(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('orbit.journal/' || $1, 0))`, runID)
	return err
}

// journalWriter appends revisions to one epoch of a run's journal.
type journalWriter struct {
	tx      pgx.Tx
	run     string
	epoch   int
	attempt int
	next    int
}

// lookup is journal.Lookup over the epoch, seeing this transaction's own
// writes.
func (j *journalWriter) lookup(ctx context.Context) journal.Lookup {
	return func(id string) (*journal.Item, error) {
		rows, err := j.tx.Query(ctx, journalSelect+`
			WHERE run_id = $1 AND epoch = $2 AND item_id = $3 ORDER BY revision DESC LIMIT 1`, j.run, j.epoch, id)
		if err != nil {
			return nil, err
		}
		items, err := collectItems(rows)
		if err != nil || len(items) == 0 {
			return nil, err
		}
		return &items[0], nil
	}
}

func (j *journalWriter) put(ctx context.Context, w journal.Write, eventSeq *int) error {
	var rev int
	if err := j.tx.QueryRow(ctx, `SELECT COALESCE(max(revision), 0) + 1 FROM journal_items
		WHERE run_id = $1 AND epoch = $2 AND item_id = $3`, j.run, j.epoch, w.ID).Scan(&rev); err != nil {
		return err
	}
	body, err := json.Marshal(w.Body)
	if err != nil {
		return err
	}
	digests := []string{}
	for _, b := range w.Blobs {
		// A head that is the whole value needs no second copy.
		if len(b.Content) <= journal.HeadMax {
			continue
		}
		digests = append(digests, b.SHA256)
		if _, err := j.tx.Exec(ctx, `INSERT INTO journal_bodies (sha256, bytes, content) VALUES ($1, $2, $3)
			ON CONFLICT (sha256) DO NOTHING`, b.SHA256, len(b.Content), b.Content); err != nil {
			return err
		}
	}
	j.next++
	_, err = j.tx.Exec(ctx, `
		INSERT INTO journal_items (run_id, epoch, seq, attempt, event_seq, item_id, revision, kind, turn_id, parent_id, at, v, body, bodies)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''), $11, $12, $13, $14)`,
		j.run, j.epoch, j.next, j.attempt, eventSeq, w.ID, rev, w.Kind, w.Turn, w.Parent, w.At, journal.Version, body, digests)
	return err
}

// syncAttempt projects one attempt's events that are not yet in the
// journal and, once the attempt has ended, closes the turn it left open.
// The caller holds lockJournal.
func syncAttempt(ctx context.Context, tx pgx.Tx, runID string, attempt int) error {
	var (
		epoch, through int
		state          journal.AttemptState
	)
	err := tx.QueryRow(ctx, `SELECT epoch, through, state FROM journal_sources WHERE run_id = $1 AND attempt = $2`,
		runID, attempt).Scan(&epoch, &through, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		// The engine records whether an attempt continues an earlier one
		// before its first event, so the epoch read here is final.
		if err = tx.QueryRow(ctx, `SELECT orbit_attempt_epoch($1, $2)`, runID, attempt).Scan(&epoch); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO journal_sources (run_id, attempt, epoch) VALUES ($1, $2, $3)`, runID, attempt, epoch)
	}
	if err != nil {
		return err
	}
	j := &journalWriter{tx: tx, run: runID, epoch: epoch, attempt: attempt}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM journal_items WHERE run_id = $1 AND epoch = $2`,
		runID, epoch).Scan(&j.next); err != nil {
		return err
	}
	look := j.lookup(ctx)
	for {
		rows, err := tx.Query(ctx, `SELECT attempt, seq, at, kind, payload FROM run_events
			WHERE run_id = $1 AND attempt = $2 AND seq > $3 ORDER BY seq LIMIT $4`, runID, attempt, through, syncPage)
		if err != nil {
			return err
		}
		evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (journal.Event, error) {
			var e journal.Event
			err := r.Scan(&e.Attempt, &e.Seq, &e.At, &e.Kind, &e.Payload)
			return e, err
		})
		if err != nil {
			return err
		}
		for _, ev := range evs {
			writes, err := journal.Project(&state, ev, look)
			if err != nil {
				return err
			}
			for _, w := range writes {
				if err := j.put(ctx, w, &ev.Seq); err != nil {
					return err
				}
			}
			through = ev.Seq
		}
		if len(evs) < syncPage {
			break
		}
	}
	outcome, endedAt, ended, err := attemptEnd(ctx, tx, runID, attempt)
	if err != nil {
		return err
	}
	if ended {
		writes, err := journal.Close(&state, outcome, endedAt, look)
		if err != nil {
			return err
		}
		for _, w := range writes {
			if err := j.put(ctx, w, nil); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(ctx, `UPDATE journal_sources SET through = $3, state = $4, closed = $5
		WHERE run_id = $1 AND attempt = $2`, runID, attempt, through, state, ended)
	return err
}

// attemptEnd says whether an attempt has ended, how, and when. An attempt
// with no run_attempts row (claimed before they existed) has ended when a
// later claim exists, with nothing known of how, or when its run is final.
func attemptEnd(ctx context.Context, tx pgx.Tx, runID string, attempt int) (outcome string, at time.Time, ended bool, err error) {
	var (
		o          *string
		endedAt    *time.Time
		hasRow     bool
		runAttempt int
		status     string
		finishedAt *time.Time
		lastEvent  *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT a.outcome, a.ended_at, a.run_id IS NOT NULL, r.attempt, r.status, r.finished_at,
		       (SELECT max(e.at) FROM run_events e WHERE e.run_id = r.id AND e.attempt = $2)
		FROM runs r LEFT JOIN run_attempts a ON a.run_id = r.id AND a.attempt = $2
		WHERE r.id = $1`, runID, attempt).Scan(&o, &endedAt, &hasRow, &runAttempt, &status, &finishedAt, &lastEvent)
	if err != nil {
		return "", time.Time{}, false, err
	}
	switch {
	case hasRow && o != nil && endedAt != nil:
		return *o, *endedAt, true, nil
	case hasRow:
		return "", time.Time{}, false, nil
	case runAttempt > attempt && lastEvent != nil:
		return "", *lastEvent, true, nil
	case runAttempt == attempt && Terminal(status) && finishedAt != nil:
		return status, *finishedAt, true, nil
	}
	return "", time.Time{}, false, nil
}

// syncInTx brings a run's attempt up to date inside a transaction that
// just ended it, so the journal's closing turn commits with the attempt's
// outcome: a reader who sees the run final sees how its turn ended.
func syncInTx(ctx context.Context, tx pgx.Tx, runID string, attempt int) error {
	if err := lockJournal(ctx, tx, runID); err != nil {
		return err
	}
	return syncAttempt(ctx, tx, runID, attempt)
}

// SyncJournal projects whatever of a run's transcript the journal does not
// hold yet. It is cheap when there is nothing to do: one indexed read.
func (s *Store) SyncJournal(ctx context.Context, runID string) error {
	attempts, err := s.unsyncedAttempts(ctx, runID)
	if err != nil || len(attempts) == 0 {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockJournal(ctx, tx, runID); err != nil {
			return err
		}
		for _, a := range attempts {
			if err := syncAttempt(ctx, tx, runID, a); err != nil {
				return fmt.Errorf("journal of run %s attempt %d: %w", runID, a, err)
			}
		}
		return nil
	})
}

// unsyncedAttempts are a run's attempts with events the journal has not
// projected, or that ended since their last sync.
func (s *Store) unsyncedAttempts(ctx context.Context, runID string) ([]int, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT e.attempt FROM (SELECT attempt, max(seq) AS last FROM run_events WHERE run_id = $1 GROUP BY attempt) e
		LEFT JOIN journal_sources j ON j.run_id = $1 AND j.attempt = e.attempt
		LEFT JOIN run_attempts a ON a.run_id = $1 AND a.attempt = e.attempt
		JOIN runs r ON r.id = $1
		WHERE j.through IS NULL OR j.through < e.last
		   OR (NOT j.closed AND (a.outcome IS NOT NULL
		       OR (a.run_id IS NULL AND (r.attempt > e.attempt OR r.status IN ('succeeded', 'failed', 'cancelled', 'skipped')))))
		ORDER BY e.attempt`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// BackfillJournal syncs every run whose journal is behind its transcript.
// all false limits it to runs running or finished within the last hour,
// which is all a deploy's older instance can still be writing; all true
// is the full pass orbit serve makes once at start. It returns how many
// runs it synced.
func (s *Store) BackfillJournal(ctx context.Context, all bool) (int, error) {
	q := `SELECT r.id::text FROM runs r WHERE (r.status = 'running' OR r.finished_at > now() - interval '1 hour')
		AND EXISTS (SELECT 1 FROM run_events e WHERE e.run_id = r.id) ORDER BY r.created_at, r.id`
	if all {
		q = `SELECT DISTINCT e.run_id::text FROM run_events e
			LEFT JOIN journal_sources j ON j.run_id = e.run_id AND j.attempt = e.attempt
			WHERE j.run_id IS NULL OR j.through < e.seq OR NOT j.closed`
	}
	rows, err := s.Pool.Query(ctx, q)
	if err != nil {
		return 0, err
	}
	runs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range runs {
		before, err := s.unsyncedAttempts(ctx, id)
		if err != nil {
			return n, err
		}
		if len(before) == 0 {
			continue
		}
		if err := s.SyncJournal(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---------------------------------------------------------------- reads

const journalSelect = `SELECT run_id::text, epoch, seq, attempt, item_id, revision, kind, turn_id, parent_id, at, v, body
	FROM journal_items `

func collectItems(rows pgx.Rows) ([]journal.Item, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (journal.Item, error) {
		var it journal.Item
		var body []byte
		if err := r.Scan(&it.RunID, &it.Epoch, &it.Seq, &it.Attempt, &it.ID, &it.Revision, &it.Kind,
			&it.Turn, &it.Parent, &it.At, &it.V, &body); err != nil {
			return it, err
		}
		b, err := journal.Upcast(it.V, it.Kind, body)
		if err != nil {
			return it, err
		}
		it.Body = b
		it.Cursor = journal.Cursor{Epoch: it.Epoch, Seq: it.Seq}.String()
		return it, nil
	})
}

// JournalHead is where one epoch of a run's journal stands: its last seq,
// and every epoch the run has, oldest first.
type JournalHead struct {
	Epoch  int   `json:"epoch"`
	Last   int   `json:"last_seq"`
	Epochs []int `json:"epochs"`
}

// GetJournalHead reads the JournalHead of an epoch of a run, or with epoch
// 0 of its latest attempt's (0 while the run has never started).
// ErrNotFound for no such run.
func (s *Store) GetJournalHead(ctx context.Context, runID string, epoch int) (*JournalHead, error) {
	h := &JournalHead{}
	err := s.Pool.QueryRow(ctx, `
		SELECT CASE WHEN $2 > 0 THEN $2 WHEN r.attempt > 0 THEN orbit_attempt_epoch(r.id, r.attempt) ELSE 0 END,
		       COALESCE((SELECT array_agg(DISTINCT j.epoch ORDER BY j.epoch) FROM journal_items j WHERE j.run_id = r.id), '{}')
		FROM runs r WHERE r.id = $1`, runID, epoch).Scan(&h.Epoch, &h.Epochs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	err = s.Pool.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM journal_items WHERE run_id = $1 AND epoch = $2`,
		runID, h.Epoch).Scan(&h.Last)
	return h, err
}

// Journal page sizes.
const (
	DefaultJournalLimit = 500
	MaxJournalLimit     = 2000
)

// ReadJournal returns an epoch's revisions after seq after, in seq order,
// and whether more follow.
func (s *Store) ReadJournal(ctx context.Context, runID string, epoch, after, limit int) ([]journal.Item, bool, error) {
	limit = clampLimit(limit, DefaultJournalLimit, MaxJournalLimit)
	rows, err := s.Pool.Query(ctx, journalSelect+`WHERE run_id = $1 AND epoch = $2 AND seq > $3 ORDER BY seq LIMIT $4`,
		runID, epoch, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	items, err := collectItems(rows)
	if err != nil || len(items) <= limit {
		return items, false, err
	}
	return items[:limit], true, nil
}

// JournalItem returns an item's latest revision in an epoch. ErrNotFound
// when the epoch has no such item.
func (s *Store) JournalItem(ctx context.Context, runID string, epoch int, itemID string) (*journal.Item, error) {
	rows, err := s.Pool.Query(ctx, journalSelect+`WHERE run_id = $1 AND epoch = $2 AND item_id = $3
		ORDER BY revision DESC LIMIT 1`, runID, epoch, itemID)
	if err != nil {
		return nil, err
	}
	items, err := collectItems(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return &items[0], nil
}

// JournalBody is the whole value behind a bounded head of one of a run's
// rows. ErrNotFound when no row of that run cut that digest short.
func (s *Store) JournalBody(ctx context.Context, runID, digest string) ([]byte, error) {
	var b []byte
	err := s.Pool.QueryRow(ctx, `SELECT b.content FROM journal_bodies b WHERE b.sha256 = $2
		AND EXISTS (SELECT 1 FROM journal_items j WHERE j.run_id = $1 AND $2 = ANY (j.bodies))`, runID, digest).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}
