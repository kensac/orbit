package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConnectorToken is one stored version of a connector's OAuth grant; Sealed is the token encrypted by internal/connectors, never readable here.
type ConnectorToken struct {
	Connector string     `json:"connector"`
	Version   int        `json:"version"`
	Sealed    []byte     `json:"-"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Reason    string     `json:"reason"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
}

const connectorGrantCols = `connector, version, sealed, expires_at, reason, created_by, created_at`

// PutConnectorToken stores a new version of a connector's grant (a person signed in) and returns its version.
func (s *Store) PutConnectorToken(ctx context.Context, conn string, sealed []byte, expires *time.Time, by string) (int, error) {
	var v int
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockConnector(ctx, tx, conn); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO connector_tokens (connector, version, sealed, expires_at, reason, created_by)
			SELECT $1, COALESCE(MAX(version), 0) + 1, $2, $3, 'authorize', $4 FROM connector_tokens WHERE connector = $1
			RETURNING version`, conn, sealed, expires, by).Scan(&v)
	})
	return v, err
}

// lockConnector serialises writes of one connector's tokens across instances, so a version number is taken once and two refreshes never race (a rotated refresh token works once).
func lockConnector(ctx context.Context, tx pgx.Tx, conn string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('connector:' || $1, 0))`, conn)
	return err
}

// LatestConnectorToken is a connector's newest version, or nil when none was ever stored.
func (s *Store) LatestConnectorToken(ctx context.Context, conn string) (*ConnectorToken, error) {
	return latestConnectorToken(ctx, s.Pool, conn)
}

func latestConnectorToken(ctx context.Context, db querier, conn string) (*ConnectorToken, error) {
	rows, err := db.Query(ctx, `SELECT `+connectorGrantCols+` FROM connector_tokens WHERE connector = $1 ORDER BY version DESC LIMIT 1`, conn)
	if err != nil {
		return nil, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[ConnectorToken])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

// ConnectorTokens is every connector's newest version, by connector.
func (s *Store) ConnectorTokens(ctx context.Context) (map[string]ConnectorToken, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ON (connector) `+connectorGrantCols+`
		FROM connector_tokens ORDER BY connector, version DESC`)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ConnectorToken])
	if err != nil {
		return nil, err
	}
	out := map[string]ConnectorToken{}
	for _, t := range list {
		out[t.Connector] = t
	}
	return out, nil
}

// RefreshConnectorToken renews a connector's grant once across instances: under the connector's lock, a version newer than seen means another refresh already happened, and it is returned as is; otherwise refresh turns the newest version into the next, stored as reason refresh.
func (s *Store) RefreshConnectorToken(ctx context.Context, conn string, seen int, refresh func(cur ConnectorToken) ([]byte, *time.Time, error)) (*ConnectorToken, error) {
	var out *ConnectorToken
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockConnector(ctx, tx, conn); err != nil {
			return err
		}
		cur, err := latestConnectorToken(ctx, tx, conn)
		if err != nil {
			return err
		}
		if cur == nil {
			return ErrNotFound
		}
		if cur.Version > seen {
			out = cur
			return nil
		}
		sealed, exp, err := refresh(*cur)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			INSERT INTO connector_tokens (connector, version, sealed, expires_at, reason, created_by)
			VALUES ($1, $2, $3, $4, 'refresh', 'orbit') RETURNING `+connectorGrantCols, conn, cur.Version+1, sealed, exp)
		if err != nil {
			return err
		}
		t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[ConnectorToken])
		out = &t
		return err
	})
	return out, err
}

// ProposeAction records an action while its run is still running, for the attempt holding the run's claim: a connector call that waits for a person (pending, with its attention row), or one executing now by rule (executing, so the action loop never takes it). ErrConflict when the attempt no longer holds the run.
func (s *Store) ProposeAction(ctx context.Context, runID string, attempt int, agent string, a ProposedAction, executing bool) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM runs WHERE id = $1 AND status = 'running' AND attempt = $2 FOR UPDATE`,
			runID, attempt).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if executing {
			return tx.QueryRow(ctx, `
				INSERT INTO actions (run_id, agent, type, params, reason, rule, status, decided_by, decided_at, executing_since, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, 'executing', 'rule', now(), now(), clock_timestamp())
				RETURNING id::text`, runID, agent, a.Type, []byte(a.Params), a.Reason, a.Rule).Scan(&id)
		}
		id, err = insertAction(ctx, tx, runID, agent, a)
		return err
	})
	return id, err
}

// ClaimAction moves one approved action to executing, for the process that will execute it; false when it is not approved (still pending, decided otherwise, or already claimed by the action loop).
func (s *Store) ClaimAction(ctx context.Context, id string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE actions SET status = 'executing', executing_since = now()
		WHERE id = $1 AND status = 'approved'`, id)
	return tag.RowsAffected() == 1, err
}
