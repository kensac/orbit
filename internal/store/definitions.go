package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AgentDefinition is one version of an agent defined through the API.
type AgentDefinition struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Version   int             `json:"version"`
	Spec      json.RawMessage `json:"spec"`
	Enabled   bool            `json:"enabled"`
	Archived  bool            `json:"archived"`
	Change    string          `json:"change"`
	Author    *string         `json:"author"`
	CreatedAt time.Time       `json:"created_at"`
}

const definitionCols = `id, name, version, spec, enabled, archived, change, author, created_at`

// ActiveDefinitions returns the newest version of every agent defined in
// the database that is not archived, by name: the definitions in force.
func (s *Store) ActiveDefinitions(ctx context.Context) ([]AgentDefinition, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+definitionCols+` FROM (
		    SELECT DISTINCT ON (name) * FROM agent_definitions ORDER BY name, version DESC
		) d WHERE NOT archived ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[AgentDefinition])
}

// LatestDefinition returns the newest version of name, archived or not,
// or ErrNotFound when the database never defined it.
func (s *Store) LatestDefinition(ctx context.Context, name string) (*AgentDefinition, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+definitionCols+` FROM agent_definitions
		WHERE name = $1 ORDER BY version DESC LIMIT 1`, name)
	if err != nil {
		return nil, err
	}
	d, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[AgentDefinition])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &d, err
}

// DefinitionHistory returns every version of name, newest first.
func (s *Store) DefinitionHistory(ctx context.Context, name string) ([]AgentDefinition, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+definitionCols+` FROM agent_definitions
		WHERE name = $1 ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[AgentDefinition])
}

// DefinitionGeneration is the newest definition's id (0 for none): it
// changes whenever any definition does, so each instance polls it to
// reload what another instance's API wrote.
func (s *Store) DefinitionGeneration(ctx context.Context) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM agent_definitions`).Scan(&n)
	return n, err
}

// NewDefinition is one change to a database agent.
type NewDefinition struct {
	Name    string
	Spec    json.RawMessage
	Enabled bool
	// Archived ends the agent; its rows and conversations stay.
	Archived bool
	// Change is create, update, enable, disable or archive.
	Change string
	Author string
	// After is the version this change follows: 0 for a create (the name
	// is new, or its newest version archived it), else the newest version
	// the caller read. A concurrent change makes it stale: ErrConflict.
	After int
}

// PutDefinition appends the next version of an agent. It is ErrConflict
// when After is not the newest version (someone else changed the agent
// first), or when a create names an agent the database defines and has
// not archived.
func (s *Store) PutDefinition(ctx context.Context, d NewDefinition) (*AgentDefinition, error) {
	var out *AgentDefinition
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// One writer per name at a time; UNIQUE (name, version) holds it too.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('agent_definitions:' || $1))`, d.Name); err != nil {
			return err
		}
		var latest int
		var archived bool
		err := tx.QueryRow(ctx, `SELECT version, archived FROM agent_definitions WHERE name = $1
			ORDER BY version DESC LIMIT 1`, d.Name).Scan(&latest, &archived)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		switch {
		case d.Change == "create" && latest > 0 && !archived:
			return fmt.Errorf("agent %q is already defined: %w", d.Name, ErrConflict)
		case d.Change != "create" && (latest == 0 || archived):
			return fmt.Errorf("agent %q: %w", d.Name, ErrNotFound)
		case d.Change != "create" && d.After != latest:
			return fmt.Errorf("agent %q changed since version %d (now %d): %w", d.Name, d.After, latest, ErrConflict)
		}
		rows, err := tx.Query(ctx, `INSERT INTO agent_definitions (name, version, spec, enabled, archived, change, author)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')) RETURNING `+definitionCols,
			d.Name, latest+1, []byte(d.Spec), d.Enabled, d.Archived, d.Change, d.Author)
		if err != nil {
			return err
		}
		v, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[AgentDefinition])
		if isUniqueViolation(err) {
			return ErrConflict
		}
		out = &v
		return err
	})
	return out, err
}
