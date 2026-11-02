package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// MailIn is a mail read from orbit's mailbox (internal/mail).
type MailIn struct {
	MessageID   string
	From        string
	Subject     string
	Raw         []byte
	Attachments json.RawMessage
	// Agent and SpecSHA are who the mail goes to. ConversationID is the conversation a reply continues; empty opens a new one titled Title.
	Agent          string
	SpecSHA        string
	ConversationID string
	Title          string
	Body           string
	Wake           bool
}

// MailResult is what a mail became.
type MailResult struct {
	Outcome        string `json:"outcome"`
	Reason         string `json:"reason,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
}

// IngestMail turns a mail into a message, in its conversation or a new one, and records it, in one transaction keyed by Message-ID: a mail seen before returns what it became (replayed) and writes nothing. A concurrent duplicate waits on the first one's uncommitted row (mail_in's primary key), then reads it.
func (s *Store) IngestMail(ctx context.Context, m MailIn) (res MailResult, replayed bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if r, ok, err := mailSeen(ctx, tx, m.MessageID); err != nil || ok {
			res, replayed = r, ok
			return err
		}
		conv := m.ConversationID
		if conv == "" {
			if err := tx.QueryRow(ctx, `INSERT INTO conversations (agent, title) VALUES ($1, $2) RETURNING id::text`,
				m.Agent, m.Title).Scan(&conv); err != nil {
				if isForeignKeyViolation(err) {
					return ErrNotFound
				}
				return err
			}
		}
		p, err := postMessage(ctx, tx, PostMessage{Agent: m.Agent, SpecSHA: m.SpecSHA, Body: m.Body, Author: m.From,
			Wake: m.Wake, ConversationID: conv})
		if err != nil {
			return err
		}
		res = MailResult{Outcome: "posted", ConversationID: conv, MessageID: p.MessageID, RunID: p.RunID}
		_, err = tx.Exec(ctx, `INSERT INTO mail_in (message_id, from_addr, subject, outcome, agent, conversation_id,
			orbit_message_id, run_id, attachments, raw) VALUES ($1, $2, $3, 'posted', $4, $5, $6, NULLIF($7, '')::uuid, $8, $9)`,
			m.MessageID, m.From, m.Subject, m.Agent, conv, p.MessageID, p.RunID, attachments(m.Attachments), m.Raw)
		return err
	})
	return res, replayed, err
}

// IgnoreMail records a mail orbit will not act on, and why. A mail already recorded keeps its first outcome.
func (s *Store) IgnoreMail(ctx context.Context, m MailIn, reason string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO mail_in (message_id, from_addr, subject, outcome, reason, attachments, raw)
		VALUES ($1, $2, $3, 'ignored', $4, $5, $6) ON CONFLICT (message_id) DO NOTHING`,
		m.MessageID, m.From, m.Subject, reason, attachments(m.Attachments), m.Raw)
	return err
}

// MailSeen reports what a Message-ID became, if orbit has read it.
func (s *Store) MailSeen(ctx context.Context, messageID string) (MailResult, bool, error) {
	return mailSeen(ctx, s.Pool, messageID)
}

func mailSeen(ctx context.Context, db rowQuerier, messageID string) (MailResult, bool, error) {
	var r MailResult
	var reason, conv, msg, run *string
	err := db.QueryRow(ctx, `SELECT outcome, reason, conversation_id::text, orbit_message_id::text, run_id::text
		FROM mail_in WHERE message_id = $1`, messageID).Scan(&r.Outcome, &reason, &conv, &msg, &run)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.Reason, r.ConversationID, r.MessageID, r.RunID = deref(reason), deref(conv), deref(msg), deref(run)
	return r, true, nil
}

// MailForRun is the newest mail among the messages run read: its Message-ID and subject, or "" when the run read none. A run that answers a mail is delivered as a reply to it.
func (s *Store) MailForRun(ctx context.Context, runID string) (messageID, subject string, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT mi.message_id, mi.subject FROM mail_in mi
		JOIN messages m ON m.id = mi.orbit_message_id
		WHERE m.run_id = $1 ORDER BY mi.received_at DESC LIMIT 1`, runID).Scan(&messageID, &subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return messageID, subject, err
}

// RunConversation is the conversation a run belongs to.
func (s *Store) RunConversation(ctx context.Context, runID string) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `SELECT conversation_id::text FROM runs WHERE id = $1`, runID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// rowQuerier is a pool or a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func attachments(b json.RawMessage) []byte {
	if len(b) == 0 {
		return []byte("[]")
	}
	return b
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
