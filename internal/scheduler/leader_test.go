package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
)

// Two instances during a deploy: one leads, the other waits, and the lock
// passes when the leader stops. A dropped connection passing it is
// TestSchedulerLockGoesWithItsConnection in the store.
func TestOneSchedulerLeadsAcrossInstances(t *testing.T) {
	first, s, _ := setup(t, "0 9 * * *")
	other, err := store.Open(ctx, s.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	second := *first
	second.Store = other

	lead, err := first.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if conn, err := second.acquire(wait); !errors.Is(err, context.DeadlineExceeded) {
		if conn != nil {
			conn.Release(ctx)
		}
		t.Fatalf("a second instance led beside the first: %v", err)
	}
	lead.Release(ctx)
	took, cancel2 := context.WithTimeout(ctx, 40*time.Second)
	defer cancel2()
	conn, err := second.acquire(took)
	if err != nil {
		t.Fatalf("the waiting instance never took over: %v", err)
	}
	conn.Release(ctx)
}
