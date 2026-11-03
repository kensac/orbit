package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// TelegramIn is one update received from the configured bot.
type TelegramIn struct {
	UpdateID       int64
	ChatID         string
	MessageID      int64
	FromUserID     int64
	Raw            []byte
	Agent          string
	SpecSHA        string
	ConversationID string
	Title          string
	Body           string
	Author         string
	Wake           bool
}

type TelegramResult struct {
	Outcome        string
	Reason         string
	ConversationID string
	MessageID      string
	RunID          string
}

// IngestTelegram writes an allowed Telegram update and its Orbit message in
// one transaction, keyed by update_id. A retry returns the first result.
func (s *Store) IngestTelegram(ctx context.Context, in TelegramIn) (out TelegramResult, replayed bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if got, ok, err := telegramSeen(ctx, tx, in.UpdateID); err != nil || ok {
			out, replayed = got, ok
			return err
		}
		conv := in.ConversationID
		if conv == "" {
			if err := tx.QueryRow(ctx, `INSERT INTO conversations (agent, title) VALUES ($1, $2) RETURNING id::text`,
				in.Agent, in.Title).Scan(&conv); err != nil {
				if isForeignKeyViolation(err) {
					return ErrNotFound
				}
				return err
			}
		}
		p, err := postMessage(ctx, tx, PostMessage{Agent: in.Agent, SpecSHA: in.SpecSHA, Body: in.Body,
			Author: in.Author, Wake: in.Wake, ConversationID: conv})
		if err != nil {
			return err
		}
		out = TelegramResult{Outcome: "posted", ConversationID: conv, MessageID: p.MessageID, RunID: p.RunID}
		_, err = tx.Exec(ctx, `INSERT INTO telegram_in (update_id, chat_id, message_id, from_user_id,
			outcome, agent, conversation_id, orbit_message_id, run_id, raw)
			VALUES ($1, $2, $3, $4, 'posted', $5, $6, $7, NULLIF($8, '')::uuid, $9)`,
			in.UpdateID, in.ChatID, in.MessageID, in.FromUserID, in.Agent, conv, p.MessageID, p.RunID, in.Raw)
		return err
	})
	return out, replayed, err
}

func (s *Store) IgnoreTelegram(ctx context.Context, in TelegramIn, reason string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO telegram_in (update_id, chat_id, message_id, from_user_id,
		outcome, reason, raw) VALUES ($1, $2, $3, $4, 'ignored', $5, $6) ON CONFLICT (update_id) DO NOTHING`,
		in.UpdateID, in.ChatID, in.MessageID, in.FromUserID, reason, in.Raw)
	return err
}

func (s *Store) TelegramSeen(ctx context.Context, updateID int64) (TelegramResult, bool, error) {
	return telegramSeen(ctx, s.Pool, updateID)
}

func telegramSeen(ctx context.Context, db rowQuerier, updateID int64) (TelegramResult, bool, error) {
	var out TelegramResult
	var reason, conversationID, messageID, runID *string
	err := db.QueryRow(ctx, `SELECT outcome, reason, conversation_id::text,
		orbit_message_id::text, run_id::text FROM telegram_in WHERE update_id = $1`, updateID).
		Scan(&out.Outcome, &reason, &conversationID, &messageID, &runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	out.Reason, out.ConversationID, out.MessageID, out.RunID = deref(reason), deref(conversationID), deref(messageID), deref(runID)
	return out, true, nil
}

// TelegramConversation resolves a reply to one of Orbit's sent Telegram messages.
func (s *Store) TelegramConversation(ctx context.Context, chatID string, messageID int64) (string, error) {
	var conversationID string
	err := s.Pool.QueryRow(ctx, `SELECT conversation_id::text FROM telegram_out WHERE chat_id = $1 AND message_id = $2`, chatID, messageID).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return conversationID, err
}

// RecordTelegramOutbound keeps the provider message id so a person's reply can continue its conversation.
func (s *Store) RecordTelegramOutbound(ctx context.Context, chatID string, messageID int64, conversationID string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO telegram_out (chat_id, message_id, conversation_id)
		VALUES ($1, $2, $3) ON CONFLICT (chat_id, message_id) DO NOTHING`, chatID, messageID, conversationID)
	if isForeignKeyViolation(err) {
		return ErrNotFound
	}
	return err
}

type RunTelegram struct {
	ChatID    string
	MessageID int64
}

// TelegramForRun returns the newest Telegram message a run read.
func (s *Store) TelegramForRun(ctx context.Context, runID string) (*RunTelegram, error) {
	var msg RunTelegram
	err := s.Pool.QueryRow(ctx, `SELECT ti.chat_id, ti.message_id FROM telegram_in ti
		JOIN messages m ON m.id = ti.orbit_message_id
		WHERE m.run_id = $1 AND ti.outcome = 'posted'
		ORDER BY ti.received_at DESC LIMIT 1`, runID).Scan(&msg.ChatID, &msg.MessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &msg, nil
}
