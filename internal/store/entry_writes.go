package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// EntryWrite is one memory entry change a run made: an agent writes its entries directly, without approval, and every version is kept, so a person reverts one through the API rather than vetting it first.
type EntryWrite struct {
	// Op is add, update or retire.
	Op string
	// New is the entry an add creates. Its key is checked, not numbered: an add whose key is taken is refused, so a run cannot pile up near-duplicates of an entry it should have updated.
	New NewEntry
	// ID and Expect name the entry an update or retire changes and the version the run read; a version written since (a person's edit) refuses the write.
	ID     string
	Expect int
	Edit   EntryEdit
	// SupersededBy is the entry a retire is replaced by: an existing one (ID), or one an earlier add of the same run created (Index, -1 for none).
	SupersededBy      *string
	SupersededByIndex int
	Note              string

	// Result is set by RecordOutcome.
	Result EntryWriteResult
}

// EntryWriteResult is what became of one write.
type EntryWriteResult struct {
	// Outcome is written, unchanged (nothing differed) or refused.
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
	ID      string `json:"id,omitempty"`
	Key     string `json:"key,omitempty"`
	Version int    `json:"version,omitempty"`
}

// applyEntryWrites runs each write in a savepoint of the outcome's transaction: one the store refuses on its content (invalid, a key taken, a stale version, an entry gone) is rolled back alone and reported, and anything else fails the outcome.
func applyEntryWrites(ctx context.Context, tx pgx.Tx, runID string, writes []*EntryWrite) error {
	if len(writes) == 0 {
		return nil
	}
	var conversation string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(learns_from, conversation_id)::text FROM runs WHERE id = $1`, runID).Scan(&conversation); err != nil {
		return err
	}
	for i, w := range writes {
		p := Provenance{Source: SourceLearned, RunID: runID, ConversationID: conversation, Note: w.Note}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		e, before, err := applyEntryWrite(ctx, sp, writes[:i], w, p)
		if err == nil {
			err = sp.Commit(ctx)
		}
		switch {
		case err == nil:
			w.Result = EntryWriteResult{Outcome: "written", ID: e.ID, Key: e.Key, Version: e.Version}
			if before == e.Version {
				w.Result.Outcome = "unchanged"
			}
		case errors.Is(err, ErrInvalid), errors.Is(err, ErrConflict), errors.Is(err, ErrNotFound):
			if rerr := sp.Rollback(ctx); rerr != nil {
				return rerr
			}
			w.Result = EntryWriteResult{Outcome: "refused", Error: err.Error()}
		default:
			return err
		}
	}
	return nil
}

func applyEntryWrite(ctx context.Context, tx pgx.Tx, earlier []*EntryWrite, w *EntryWrite, p Provenance) (*Entry, int, error) {
	switch w.Op {
	case "add":
		n := w.New
		if n.Key == "" {
			n.Key = EntryKey(n.Title)
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memory_entries WHERE scope = $1 AND owner = $2 AND key = $3)`,
			n.Scope, n.Owner, n.Key).Scan(&taken); err != nil {
			return nil, 0, err
		}
		if taken {
			return nil, 0, fmt.Errorf("an entry with key %q exists in this scope; update it instead: %w", n.Key, ErrConflict)
		}
		e, err := createEntry(ctx, tx, n, p)
		return e, 0, err
	case "update":
		e, err := updateEntry(ctx, tx, w.ID, w.Expect, w.Edit, p)
		return e, w.Expect, err
	case "retire":
		by := w.SupersededBy
		if w.SupersededByIndex >= 0 {
			if w.SupersededByIndex >= len(earlier) || earlier[w.SupersededByIndex].Result.ID == "" {
				return nil, 0, fmt.Errorf("%w: superseded_by names an add of this run that was not written", ErrInvalid)
			}
			id := earlier[w.SupersededByIndex].Result.ID
			by = &id
		}
		e, err := retireEntry(ctx, tx, w.ID, w.Expect, by, p)
		return e, w.Expect, err
	}
	return nil, 0, fmt.Errorf("%w: op must be add, update or retire", ErrInvalid)
}
