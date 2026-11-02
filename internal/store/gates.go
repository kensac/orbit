package store

import (
	"context"
	"errors"
	"path"

	"github.com/jackc/pgx/v5"
)

// LastSuccessInputs is what an agent's newest succeeded run showed its model,
// for the unchanged gate: the run, the spec it ran under, and each input
// of its final attempt by file name with its sha256. A skipped run is not
// one: it judged nothing.
type LastSuccessInputs struct {
	RunID   string
	SpecSHA string
	Inputs  map[string]string
}

// LastSucceededInputs returns the agent's newest succeeded run's inputs, or
// nil when it has none. Inputs are keyed by base name, so an agent with a
// session (inputs/<attempt>/...) compares like one without.
func (s *Store) LastSucceededInputs(ctx context.Context, agent string) (*LastSuccessInputs, error) {
	var l LastSuccessInputs
	var attempt int
	// runs_agent_finished serves this.
	err := s.Pool.QueryRow(ctx, `SELECT id::text, spec_sha, attempt FROM runs
		WHERE agent = $1 AND status = 'succeeded' ORDER BY finished_at DESC LIMIT 1`, agent).Scan(&l.RunID, &l.SpecSHA, &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT name, sha256 FROM artifacts
		WHERE run_id = $1 AND attempt = $2 AND direction = 'in' AND name LIKE 'inputs/%'`, l.RunID, attempt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	l.Inputs = map[string]string{}
	for rows.Next() {
		var name, sum string
		if err := rows.Scan(&name, &sum); err != nil {
			return nil, err
		}
		l.Inputs[path.Base(name)] = sum
	}
	return &l, rows.Err()
}
