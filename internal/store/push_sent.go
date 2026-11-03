package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// SummaryTag is the notification tag of a push that folds several rows into one (internal/push).
const SummaryTag = "orbit-inbox"

// RecordPushSent records that author's devices were sent ids under tag.
func (s *Store) RecordPushSent(ctx context.Context, author, tag string, ids []string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO push_sent (attention_id, author, tag)
		SELECT unnest($3::uuid[]), $1, $2`, author, tag, ids)
	return err
}

// PushClose is one notification to take off a person's devices.
type PushClose struct {
	Author string
	Tag    string
}

// ClaimClosedPush returns the notifications whose rows have closed since they were pushed, less than maxAge ago, and marks them so each is closed once: a row pushed as itself once it closes, a summary once every row summarised under it for that person has. It holds ClaimPush's lock, so two instances never both close one.
func (s *Store) ClaimClosedPush(ctx context.Context, maxAge time.Duration) ([]PushClose, error) {
	var out []PushClose
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(pushLock)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT p.id::text, p.author, p.tag, (`+attentionOpen+`)
			FROM push_sent p JOIN attention a ON a.id = p.attention_id
			WHERE p.closed_at IS NULL AND p.sent_at > now() - $1::interval`, maxAge.String())
		if err != nil {
			return err
		}
		type sent struct {
			id, author, tag string
			open            bool
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (sent, error) {
			var x sent
			err := r.Scan(&x.id, &x.author, &x.tag, &x.open)
			return x, err
		})
		if err != nil {
			return err
		}
		groups := map[PushClose][]sent{}
		var order []PushClose
		for _, x := range list {
			k := PushClose{Author: x.author, Tag: x.tag}
			if _, ok := groups[k]; !ok {
				order = append(order, k)
			}
			groups[k] = append(groups[k], x)
		}
		var ids []string
		for _, k := range order {
			closed, all := []string{}, true
			for _, x := range groups[k] {
				if x.open {
					all = false
				} else {
					closed = append(closed, x.id)
				}
			}
			if len(closed) == 0 || (k.Tag == SummaryTag && !all) {
				continue
			}
			ids = append(ids, closed...)
			out = append(out, k)
		}
		if len(ids) == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE push_sent SET closed_at = now() WHERE id = ANY($1::uuid[])`, ids)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PushActionUse is one use of a push action link.
type PushActionUse struct {
	Nonce, Person, Kind, TargetID, Decision string
}

// UsePushAction records a push action link's use; ErrConflict when its nonce was used before (another button of the same notification, or the same one twice).
func (s *Store) UsePushAction(ctx context.Context, u PushActionUse) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO push_action_uses (nonce, person, kind, target_id, decision) VALUES ($1, $2, $3, $4::uuid, $5)`,
		u.Nonce, u.Person, u.Kind, u.TargetID, u.Decision)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}
