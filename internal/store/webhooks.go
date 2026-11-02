package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// TriggerWebhook is a run a signed webhook delivery started.
const TriggerWebhook = "webhook"

// WebhookDelivery is one inbound delivery that became a run.
type WebhookDelivery struct {
	ID          string     `json:"id"`
	Agent       string     `json:"agent"`
	TriggerName string     `json:"trigger"`
	DeliveryID  string     `json:"delivery_id"`
	Event       string     `json:"event,omitempty"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	ReceivedAt  time.Time  `json:"received_at"`
	MediaType   string     `json:"media_type"`
	Body        []byte     `json:"-"`
	RunID       string     `json:"run_id"`
}

// NewWebhook is what recording a delivery needs.
type NewWebhook struct {
	Agent        string
	SpecSHA      string
	TriggerName  string
	TriggerIndex int
	DeliveryID   string
	Event        string
	SentAt       *time.Time
	MediaType    string
	Body         []byte
	Prompt       string
}

// RecordWebhook enqueues the run a delivery starts and records the delivery with it, in one transaction. A delivery already recorded (webhook_deliveries_once: a sender's retry, or two instances receiving it) enqueues nothing and returns the first run's id with duplicate true.
func (s *Store) RecordWebhook(ctx context.Context, d NewWebhook) (runID string, duplicate bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO runs (agent, spec_sha, trigger, trigger_index, prompt)
			VALUES ($1, $2, 'webhook', $3, $4) RETURNING id::text`,
			d.Agent, d.SpecSHA, d.TriggerIndex, d.Prompt).Scan(&runID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO webhook_deliveries (agent, trigger_name, delivery_id, event, sent_at, media_type, body, run_id)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8)`,
			d.Agent, d.TriggerName, d.DeliveryID, d.Event, d.SentAt, d.MediaType, d.Body, runID)
		return err
	})
	if isForeignKeyViolation(err) {
		return "", false, ErrNotFound
	}
	if isUniqueViolation(err) {
		err = s.Pool.QueryRow(ctx, `SELECT run_id::text FROM webhook_deliveries
			WHERE agent = $1 AND trigger_name = $2 AND delivery_id = $3`, d.Agent, d.TriggerName, d.DeliveryID).Scan(&runID)
		return runID, err == nil, err
	}
	if err != nil {
		return "", false, err
	}
	return runID, false, nil
}

// WebhookForRun is the delivery that started a run, or nil for a run no delivery started.
func (s *Store) WebhookForRun(ctx context.Context, runID string) (*WebhookDelivery, error) {
	var d WebhookDelivery
	var event *string
	err := s.Pool.QueryRow(ctx, `
		SELECT id::text, agent, trigger_name, delivery_id, event, sent_at, received_at, media_type, body, run_id::text
		FROM webhook_deliveries WHERE run_id = $1`, runID).
		Scan(&d.ID, &d.Agent, &d.TriggerName, &d.DeliveryID, &event, &d.SentAt, &d.ReceivedAt, &d.MediaType, &d.Body, &d.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if event != nil {
		d.Event = *event
	}
	return &d, nil
}
