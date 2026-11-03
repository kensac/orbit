package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const telegramPollerLock = int64(0x74656c656772616d)

type TelegramLock struct{ conn *pgxpool.Conn }

func (s *Store) TryTelegramLock(ctx context.Context) (*TelegramLock, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, telegramPollerLock).Scan(&ok); err != nil {
		conn.Release()
		return nil, err
	}
	if !ok {
		conn.Release()
		return nil, nil
	}
	return &TelegramLock{conn: conn}, nil
}

func (l *TelegramLock) Ping(ctx context.Context) error { return l.conn.Ping(ctx) }
func (l *TelegramLock) Release(ctx context.Context) {
	_, _ = l.conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, telegramPollerLock)
	l.conn.Release()
}

type TelegramIn struct {
	UpdateID, ChatID, UserID    int64
	Agent, SpecSHA, Text, Title string
	NewConversation             bool
}

type TelegramResult struct{ Outcome, ConversationID, MessageID, RunID string }

// IngestTelegram posts one authorized Telegram update and advances its chat conversation atomically.
func (s *Store) IngestTelegram(ctx context.Context, in TelegramIn) (res TelegramResult, replayed bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var ignored bool
		err := tx.QueryRow(ctx, `SELECT true FROM telegram_in WHERE update_id=$1`, in.UpdateID).Scan(&ignored)
		if err == nil {
			replayed = true
			res.Outcome = "seen"
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var conv string
		if !in.NewConversation {
			err = tx.QueryRow(ctx, `SELECT conversation_id::text FROM telegram_chats WHERE chat_id=$1 FOR UPDATE`, in.ChatID).Scan(&conv)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if conv == "" {
			if in.Agent == "" {
				return ErrNotFound
			}
			if err := tx.QueryRow(ctx, `INSERT INTO conversations (agent,title) VALUES ($1,$2) RETURNING id::text`, in.Agent, in.Title).Scan(&conv); err != nil {
				if isForeignKeyViolation(err) {
					return ErrNotFound
				}
				return err
			}
		}
		var agent string
		if err := tx.QueryRow(ctx, `SELECT agent FROM conversations WHERE id=$1`, conv).Scan(&agent); err != nil {
			return err
		}
		p, err := postMessage(ctx, tx, PostMessage{Agent: agent, SpecSHA: in.SpecSHA, Body: in.Text, Author: "telegram", Wake: true, ConversationID: conv})
		if err != nil {
			return err
		}
		res = TelegramResult{Outcome: "posted", ConversationID: conv, MessageID: p.MessageID, RunID: p.RunID}
		if _, err := tx.Exec(ctx, `INSERT INTO telegram_in (update_id,chat_id,user_id,outcome,conversation_id,orbit_message_id,run_id,raw) VALUES ($1,$2,$3,'posted',$4,$5,NULLIF($6,'')::uuid,$7)`, in.UpdateID, in.ChatID, in.UserID, conv, p.MessageID, p.RunID, in.Text); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO telegram_chats (chat_id,conversation_id,agent) VALUES ($1,$2,$3) ON CONFLICT(chat_id) DO UPDATE SET conversation_id=EXCLUDED.conversation_id, agent=EXCLUDED.agent, updated_at=now()`, in.ChatID, conv, agent)
		return err
	})
	return res, replayed, err
}

func (s *Store) IgnoreTelegram(ctx context.Context, updateID, chatID, userID int64, reason, text string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO telegram_in (update_id,chat_id,user_id,outcome,reason,raw) VALUES ($1,$2,$3,'ignored',$4,$5) ON CONFLICT(update_id) DO NOTHING`, updateID, chatID, userID, reason, text)
	return err
}

type RunTelegram struct{ ChatID int64 }

func (s *Store) TelegramOffset(ctx context.Context) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(max(update_id),0) FROM telegram_in`).Scan(&id)
	return id + 1, err
}

func (s *Store) TelegramForRun(ctx context.Context, runID string) (*RunTelegram, error) {
	var t RunTelegram
	err := s.Pool.QueryRow(ctx, `SELECT ti.chat_id FROM telegram_in ti JOIN messages m ON m.id=ti.orbit_message_id WHERE m.run_id=$1 ORDER BY ti.received_at DESC LIMIT 1`, runID).Scan(&t.ChatID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
