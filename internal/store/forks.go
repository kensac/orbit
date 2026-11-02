package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Forks: a new conversation started from another at one of its messages
// (the conversation_forks migration).

// maxForkDepth bounds a chain of forks read for a thread (store.Conversation):
// a fork of a fork of ... past this reads no further back.
const maxForkDepth = 20

// ForkRequest forks a conversation at one of its messages.
type ForkRequest struct {
	ConversationID string
	MessageID      string
	// ClientID makes a retry return the first fork (receipts).
	ClientID string
}

// ForkConversation starts a new conversation of the same agent holding the
// thread as it stood once the message was answered: up to the finish of the
// run that read it (a person's message) or asked it (a question), or up to
// the message itself when no run has read it. The fork keeps the parent's
// title with " (fork)", its model and effort, and its chat workspace as of
// that point. When the answering run succeeded on Claude Code and left a
// session, the fork's first run may continue that session at the run's last
// assistant message (engine planSession); otherwise it starts fresh and is
// told the thread as text. A message whose reply is still being written is
// ErrConflict; one not in the conversation ErrNotFound.
func (s *Store) ForkConversation(ctx context.Context, f ForkRequest) (*Conversation, bool, error) {
	if !IsUUID(f.ConversationID) || !IsUUID(f.MessageID) {
		return nil, false, fmt.Errorf("%w: conversation and message ids are UUIDs", ErrInvalid)
	}
	var agent string
	if err := s.Pool.QueryRow(ctx, `SELECT agent FROM conversations WHERE id = $1`, f.ConversationID).Scan(&agent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrNotFound
		}
		return nil, false, err
	}
	req := map[string]any{"op": "fork", "conversation_id": f.ConversationID, "message_id": f.MessageID}
	raw, replayed, err := s.withReceipt(ctx, agent, f.ClientID, req, func(tx pgx.Tx) (json.RawMessage, error) {
		id, err := forkConversation(ctx, tx, f)
		if err != nil {
			return nil, err
		}
		return json.Marshal(CreateResult{ConversationID: id})
	})
	if err != nil {
		return nil, false, err
	}
	var res CreateResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, false, err
	}
	c, err := s.GetConversation(ctx, res.ConversationID)
	return c, replayed, err
}

func forkConversation(ctx context.Context, tx pgx.Tx, f ForkRequest) (string, error) {
	var (
		title, agent  string
		model, effort *string
		created       time.Time
		runID         *string
		runStatus     *string
		finished      *time.Time
		sessionOK     bool
	)
	err := tx.QueryRow(ctx, `
		SELECT c.title, c.agent, c.model, c.effort, m.created_at, r.id::text, r.status, r.finished_at,
		       r.status = 'succeeded' AND r.session_id IS NOT NULL AND r.runner_kind = 'claude-code'
		FROM conversations c
		JOIN messages m ON m.id = $2 AND m.conversation_id = c.id
		LEFT JOIN runs r ON r.id = m.run_id
		WHERE c.id = $1
		FOR SHARE OF c`, f.ConversationID, f.MessageID).
		Scan(&title, &agent, &model, &effort, &created, &runID, &runStatus, &finished, &sessionOK)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("message %s is not in conversation %s: %w", f.MessageID, f.ConversationID, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	cutoff := created
	var forkRun, resumeAt *string
	if runID != nil {
		if finished == nil {
			return "", fmt.Errorf("the reply to that message is still being written (run %s is %s): %w", *runID, *runStatus, ErrConflict)
		}
		cutoff = *finished
		if sessionOK {
			// The run's last assistant message: where its session stood
			// when the run ended, whatever a live process added after.
			var at *string
			if err := tx.QueryRow(ctx, `SELECT e.payload->>'uuid' FROM run_events e JOIN runs r ON r.id = e.run_id
				WHERE e.run_id = $1 AND e.attempt = r.attempt AND e.kind = 'assistant' AND e.payload->>'uuid' <> ''
				ORDER BY e.seq DESC LIMIT 1`, *runID).Scan(&at); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return "", err
			}
			if at != nil {
				forkRun, resumeAt = runID, at
			}
		}
	}
	if title != "" {
		title = forkTitle(title)
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO conversations
		    (agent, title, model, effort, forked_from, forked_at_message, fork_cutoff, fork_run, fork_resume_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id::text`,
		agent, title, model, effort, f.ConversationID, f.MessageID, cutoff, forkRun, resumeAt).Scan(&id); err != nil {
		return "", err
	}
	// The workspace as of the fork: the parent's newest snapshot taken by
	// then (a snapshot is stored as its run ends, before the run's finish).
	if _, err := tx.Exec(ctx, `INSERT INTO workspaces (conversation_id, run_id, attempt, sha256, bytes, files, archive)
		SELECT $1, w.run_id, w.attempt, w.sha256, w.bytes, w.files, w.archive FROM workspaces w
		WHERE w.conversation_id = $2 AND w.created_at <= $3
		ORDER BY w.id DESC LIMIT 1`, id, f.ConversationID, cutoff); err != nil {
		return "", err
	}
	return id, nil
}

// forkTitle is a fork's title: the parent's with " (fork)", the parent's cut
// at a word to fit.
func forkTitle(parent string) string {
	const mark = " (fork)"
	room := titleMax*4 - utf8.RuneCountInString(mark)
	if utf8.RuneCountInString(parent) > room {
		r := []rune(parent)[:room]
		parent = string(r)
		if i := strings.LastIndexByte(parent, ' '); i > 0 {
			parent = parent[:i]
		}
	}
	return parent + mark
}

// ForkSeed is the session a fork's first run may continue: the parent's
// answering run's, at ResumeAt.
type ForkSeed struct {
	PriorSession
	// Parent is the conversation forked from, whose run left the session.
	Parent string
	// ResumeAt is the run's last assistant message (--resume-session-at).
	ResumeAt string
}

// ForkSession returns the seed of a fork that has none of its own runs yet,
// or nil: not a fork, a fork whose point left no session, or a fork that has
// run (its own runs' sessions follow, LatestSession).
func (s *Store) ForkSession(ctx context.Context, conversationID string) (*ForkSeed, error) {
	var f ForkSeed
	var kind, sid *string
	var depth *int
	err := s.Pool.QueryRow(ctx, `
		SELECT c.forked_from::text, c.fork_resume_at, r.id::text, r.status, r.spec_sha, r.runner_kind, r.session_id,
		       r.finished_at, r.session_depth, r.session_cost_usd::float8
		FROM conversations c JOIN runs r ON r.id = c.fork_run
		WHERE c.id = $1
		  AND NOT EXISTS (SELECT 1 FROM runs o WHERE o.conversation_id = c.id AND o.status IN ('succeeded', 'failed'))`,
		conversationID).Scan(&f.Parent, &f.ResumeAt, &f.RunID, &f.Status, &f.SpecSHA, &kind, &sid, &f.FinishedAt, &depth, &f.CostUSD)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if kind != nil {
		f.RunnerKind = *kind
	}
	if sid != nil {
		f.SessionID = *sid
	}
	if depth != nil {
		f.Depth = *depth
	}
	return &f, nil
}
