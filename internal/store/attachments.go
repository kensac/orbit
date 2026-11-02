package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Attachment kinds: what the API can attach to an agent beside its spec.
const (
	AttachTopic = "topic"
	AttachSkill = "skill"
)

// Attachment is a topic or skill attached to an agent through the API.
type Attachment struct {
	Agent      string    `json:"agent"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	AttachedBy *string   `json:"attached_by"`
	AttachedAt time.Time `json:"attached_at"`
}

func validAttachment(kind, name string) error {
	if kind != AttachTopic && kind != AttachSkill {
		return fmt.Errorf("%w: kind must be topic or skill", ErrInvalid)
	}
	if !ownerRE.MatchString(name) {
		return fmt.Errorf("%w: %s name %q must match %s", ErrInvalid, kind, name, ownerRE)
	}
	return nil
}

// Attach attaches a topic or skill to an agent; attaching one already attached changes nothing (created false).
func (s *Store) Attach(ctx context.Context, agent, kind, name, by string) (created bool, err error) {
	if err := validAttachment(kind, name); err != nil {
		return false, err
	}
	tag, err := s.Pool.Exec(ctx, `INSERT INTO agent_attachments (agent, kind, name, attached_by)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (agent, kind, name) WHERE detached_at IS NULL DO NOTHING`, agent, kind, name, by)
	if isForeignKeyViolation(err) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Detach ends an attachment, keeping its row. One not attached is ErrNotFound.
func (s *Store) Detach(ctx context.Context, agent, kind, name, by string) error {
	if err := validAttachment(kind, name); err != nil {
		return err
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_attachments SET detached_at = now(), detached_by = NULLIF($4, '')
		WHERE agent = $1 AND kind = $2 AND name = $3 AND detached_at IS NULL`, agent, kind, name, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Attached lists an agent's current attachments of one kind ("" for both), by kind and name.
func (s *Store) Attached(ctx context.Context, agent, kind string) ([]Attachment, error) {
	rows, err := s.Pool.Query(ctx, `SELECT agent, kind, name, attached_by, attached_at FROM agent_attachments
		WHERE agent = $1 AND ($2 = '' OR kind = $2) AND detached_at IS NULL ORDER BY kind, name`, agent, kind)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Attachment])
}

// AttachedNames is Attached reduced to names.
func (s *Store) AttachedNames(ctx context.Context, agent, kind string) ([]string, error) {
	list, err := s.Attached(ctx, agent, kind)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, a := range list {
		names = append(names, a.Name)
	}
	return names, nil
}

// Topic is a memory topic: one that holds entries or is attached to an agent through the API.
type Topic struct {
	Name string `json:"name"`
	// Entries counts its active entries.
	Entries int `json:"entries"`
	// Agents are the agents it is attached to through the API, by name.
	Agents []string `json:"agents"`
}

// ListTopics returns every topic, by name.
func (s *Store) ListTopics(ctx context.Context) ([]Topic, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH names AS (
		    SELECT owner AS name FROM memory_entries WHERE scope = 'topic'
		    UNION SELECT name FROM agent_attachments WHERE kind = 'topic' AND detached_at IS NULL)
		SELECT n.name,
		    (SELECT count(*) FROM memory_entries e JOIN memory_entry_versions v
		         ON v.entry_id = e.id AND v.version = e.current_version
		     WHERE e.scope = 'topic' AND e.owner = n.name AND v.status = 'active')::int,
		    COALESCE((SELECT array_agg(a.agent ORDER BY a.agent) FROM agent_attachments a
		     WHERE a.kind = 'topic' AND a.name = n.name AND a.detached_at IS NULL), '{}')
		FROM names n ORDER BY n.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Topic])
}
