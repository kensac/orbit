package store_test

import (
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// One instance holds the scheduler lock at a time, and Release passes it.
func TestSchedulerLockIsHeldOnce(t *testing.T) {
	s := testdb.New(t)
	other, err := store.Open(ctx, s.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	lead, err := s.TrySchedulerLock(ctx)
	if err != nil || lead == nil {
		t.Fatalf("first lock = %v, %v", lead, err)
	}
	if l, err := other.TrySchedulerLock(ctx); err != nil || l != nil {
		t.Fatalf("second instance locked beside the first: %v, %v", l, err)
	}
	lead.Release(ctx)
	l, err := other.TrySchedulerLock(ctx)
	if err != nil || l == nil {
		t.Fatalf("after release = %v, %v", l, err)
	}
	l.Release(ctx)
}

// The leader's connection dropping releases the lock, and its Ping says so.
func TestSchedulerLockGoesWithItsConnection(t *testing.T) {
	s := testdb.New(t)
	other, err := store.Open(ctx, s.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	lead, err := s.TrySchedulerLock(ctx)
	if err != nil || lead == nil {
		t.Fatalf("lock = %v, %v", lead, err)
	}
	defer lead.Release(ctx)
	var n int
	if err := other.Pool.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_locks
		WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("terminated %d holders: %v", n, err)
	}
	if err := lead.Ping(ctx); err == nil {
		t.Fatal("Ping succeeded on a dropped connection")
	}
	l, err := other.TrySchedulerLock(ctx)
	if err != nil || l == nil {
		t.Fatalf("after the drop = %v, %v", l, err)
	}
	l.Release(ctx)
}

func TestTelegramLockIsHeldOnce(t *testing.T) {
	s := testdb.New(t)
	other, err := store.Open(ctx, s.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	lead, err := s.TryTelegramLock(ctx)
	if err != nil || lead == nil {
		t.Fatalf("first lock = %v, %v", lead, err)
	}
	if l, err := other.TryTelegramLock(ctx); err != nil || l != nil {
		t.Fatalf("second instance locked beside the first: %v, %v", l, err)
	}
	lead.Release(ctx)
	l, err := other.TryTelegramLock(ctx)
	if err != nil || l == nil {
		t.Fatalf("after release = %v, %v", l, err)
	}
	l.Release(ctx)
}
