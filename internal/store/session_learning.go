package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Learning over imported sessions: a learning run (trigger learn) that reads one imported session's lines between the previous succeeded learning run of it and a point, the session's generation and line count when it was enqueued (the session_learning migration).

// SessionDue is an imported session ready to be learned from, through its current point.
type SessionDue struct {
	SessionID  string
	Generation int
	Lines      int
}

// SessionLearningDue returns at most limit imported sessions to learn from, newest first: lines past the last learning run of their generation that was tried (succeeded, failed, queued or running), quiet for idle (no line newer), and active within horizon. It returns none while a learning run of any session is queued or running, so imported history is learned one session at a time and a first import of many sessions never queues a run for each.
func (s *Store) SessionLearningDue(ctx context.Context, idle, horizon time.Duration, limit int) ([]SessionDue, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT e.id::text, e.generation, e.lines FROM external_sessions e
		WHERE e.lines > 0 AND e.last_at < now() - $1::interval AND e.last_at > now() - $2::interval
		  AND NOT EXISTS (
		      SELECT 1 FROM runs l WHERE l.trigger = 'learn' AND l.learns_session = e.id AND l.learn_generation = e.generation
		        AND (l.status IN ('queued', 'running') OR l.learn_lines >= e.lines))
		  AND NOT EXISTS (
		      SELECT 1 FROM runs b WHERE b.trigger = 'learn' AND b.learns_session IS NOT NULL AND b.status IN ('queued', 'running'))
		ORDER BY e.last_at DESC, e.id LIMIT $3`, idle, horizon, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SessionDue])
}

// EnqueueSessionLearning queues a learning run of an imported session through its point, by agent, in that agent's learning conversation (created on first use, archived). It returns "" when one for that point exists already (runs_one_session_learning).
func (s *Store) EnqueueSessionLearning(ctx context.Context, agent, specSHA string, d SessionDue) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO conversations (agent, title, archived, purpose)
			VALUES ($1, 'Learning', true, 'learning') ON CONFLICT (agent) WHERE purpose = 'learning' DO NOTHING`, agent); err != nil {
			if isForeignKeyViolation(err) {
				return ErrNotFound
			}
			return err
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO runs (agent, spec_sha, trigger, prompt, conversation_id, learns_session, learn_generation, learn_lines)
			SELECT $1, $2, 'learn', $6, l.id, $3::uuid, $4, $5
			FROM conversations l WHERE l.agent = $1 AND l.purpose = 'learning'
			ON CONFLICT (learns_session, learn_generation, learn_lines) WHERE trigger = 'learn' AND learns_session IS NOT NULL DO NOTHING
			RETURNING id::text`, agent, specSHA, d.SessionID, d.Generation, d.Lines,
			fmt.Sprintf("Learn from imported session %s, generation %d, through line %d.", d.SessionID, d.Generation, d.Lines)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			id = ""
			return nil
		}
		if isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return err
	})
	return id, err
}

// LearnSessionNow queues a learning run of an imported session through its current point, whatever its age or quiet: the API's way to learn from one at once. ErrConflict when that point has a learning run already.
func (s *Store) LearnSessionNow(ctx context.Context, agent, specSHA, sessionID string) (string, error) {
	e, err := s.GetExternalSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if e.Lines == 0 {
		return "", fmt.Errorf("the session has no lines to learn from: %w", ErrConflict)
	}
	id, err := s.EnqueueSessionLearning(ctx, agent, specSHA, SessionDue{SessionID: e.ID, Generation: e.Generation, Lines: e.Lines})
	if err == nil && id == "" {
		err = fmt.Errorf("a learning run for this point exists already: %w", ErrConflict)
	}
	return id, err
}

// SessionWindow is what a session learning run reads: lines After < n <= Through of one generation.
type SessionWindow struct {
	SessionID  string
	Generation int
	After      int
	Through    int
}

// SessionLearningWindowOf returns a session learning run's window, or ErrNotFound for any other run (a conversation's learning run among them).
func (s *Store) SessionLearningWindowOf(ctx context.Context, runID string) (*SessionWindow, error) {
	var w SessionWindow
	var after *int
	err := s.Pool.QueryRow(ctx, `
		SELECT r.learns_session::text, r.learn_generation, r.learn_lines,
		       (SELECT max(p.learn_lines) FROM runs p
		        WHERE p.trigger = 'learn' AND p.learns_session = r.learns_session AND p.learn_generation = r.learn_generation
		          AND p.status = 'succeeded' AND p.learn_lines < r.learn_lines)
		FROM runs r WHERE r.id = $1 AND r.trigger = 'learn' AND r.learns_session IS NOT NULL`, runID).
		Scan(&w.SessionID, &w.Generation, &w.Through, &after)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if after != nil {
		w.After = *after
	}
	return &w, err
}

// SessionWindowLines returns a window's lines newest first, up to limit lines and about maxBytes of content (the newest line always, whatever its size): a learning run keeps the newest of a long session.
func (s *Store) SessionWindowLines(ctx context.Context, w SessionWindow, limit, maxBytes int) ([]StoredLine, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT n, digest, content FROM (
		    SELECT l.n, l.digest, c.content,
		           sum(length(c.content)) OVER (ORDER BY l.n DESC) AS upto, row_number() OVER (ORDER BY l.n DESC) AS k
		    FROM external_session_lines l JOIN external_line_contents c ON c.digest = l.digest
		    WHERE l.external_session_id = $1 AND l.generation = $2 AND l.n > $3 AND l.n <= $4
		) t
		WHERE k = 1 OR upto <= $6
		ORDER BY n DESC LIMIT $5`, w.SessionID, w.Generation, w.After, w.Through, limit, maxBytes)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[StoredLine])
}

// SessionLearned is what an earlier learning run made of another imported session: its report, which lists the procedures it saw.
type SessionLearned struct {
	SessionID string
	Title     string
	Report    string
	At        time.Time
}

// SessionLearnings returns the newest succeeded session learning runs' reports, one per session (its newest), other than except's: what a learning run compares its own session with.
func (s *Store) SessionLearnings(ctx context.Context, except string, limit int) ([]SessionLearned, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT session, title, report, at FROM (
		    SELECT DISTINCT ON (r.learns_session) r.learns_session::text AS session, COALESCE(e.title, e.first_prompt, '') AS title,
		           COALESCE(r.report, '') AS report, r.finished_at AS at
		    FROM runs r JOIN external_sessions e ON e.id = r.learns_session
		    WHERE r.trigger = 'learn' AND r.status = 'succeeded' AND r.learns_session <> $1::uuid
		    ORDER BY r.learns_session, r.finished_at DESC
		) t ORDER BY at DESC, session LIMIT $2`, except, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SessionLearned])
}

// WriteProposedSkill applies an approved skill:write: the skill's next version (or its first, expect 0), with source agent and the proposing run, its other files kept. It is not attached to any agent: the skill is the person's, for the library and their laptop.
func (s *Store) WriteProposedSkill(ctx context.Context, runID, name, content string, expect int, note string) (*Skill, bool, error) {
	var out *Skill
	var changed bool
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var files []SkillFile
		if expect > 0 {
			cur, err := skillAt(ctx, tx, name, 0, true)
			if err != nil {
				return err
			}
			files = cur.Files
		}
		var err error
		out, changed, err = putSkill(ctx, tx, SkillWrite{Name: name, Content: content, Files: files, Expect: expect, Create: expect == 0},
			SkillProvenance{Source: SkillByAgent, RunID: runID, Note: note})
		return err
	})
	return out, changed, err
}
