package store_test

import (
	"testing"
	"time"

	"github.com/kensac/orbit/internal/testdb"
)

// The oldest due queued run's wait counts from when it became due; a continuation not yet due and a run that is not queued do not count.
func TestOldestQueuedAge(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a", "b")
	if age, err := s.OldestQueuedAge(ctx); err != nil || age != 0 {
		t.Fatalf("empty queue: %v %v", age, err)
	}
	old := manual(t, s, "a")
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET created_at = now() - interval '40 minutes' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	// Created long ago but due only a minute ago: a minute of waiting.
	late := manual(t, s, "b")
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET created_at = now() - interval '2 hours', run_after = now() - interval '1 minute' WHERE id = $1`, late); err != nil {
		t.Fatal(err)
	}
	age, err := s.OldestQueuedAge(ctx)
	if err != nil || age < 39*time.Minute || age > 41*time.Minute {
		t.Fatalf("age = %v %v, want about 40m", age, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET status = 'cancelled', finished_at = now() WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if age, err := s.OldestQueuedAge(ctx); err != nil || age < 50*time.Second || age > 3*time.Minute {
		t.Fatalf("age = %v %v, want about 1m", age, err)
	}
}
