package store_test

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestBoxAccounts(t *testing.T) {
	s := testdb.New(t)
	b, err := s.EnsureBox(ctx, "orbit")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.EnsureBox(ctx, "finance")
	a, err := s.CreateAccount(ctx, b.ID, "personal", "kanishk")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(a.Slot) || a.Agent != "orbit" || a.LoggedIn || a.CheckedAt != nil {
		t.Fatalf("account %+v", a)
	}
	if _, err := s.CreateAccount(ctx, b.ID, "personal", "kanishk"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a duplicate label: %v", err)
	}
	// Labels are per box.
	if _, err := s.CreateAccount(ctx, other.ID, "personal", "kanishk"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < store.MaxBoxAccounts; i++ {
		if _, err := s.CreateAccount(ctx, b.ID, "a"+string(rune('a'+i)), "kanishk"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateAccount(ctx, b.ID, "one more", "kanishk"); !errors.Is(err, store.ErrLimit) {
		t.Fatalf("over the limit: %v", err)
	}
	slots, err := s.AccountSlots(ctx, b.ID)
	if err != nil || len(slots) != store.MaxBoxAccounts || slots[0] != a.Slot {
		t.Fatalf("slots %v %v", slots, err)
	}

	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := s.RecordAccountLogin(ctx, a.ID, store.AccountLogin{LoggedIn: true, Email: "k@example.com", SubscriptionType: "max", RefreshExpiresAt: &exp})
	if err != nil || !got.LoggedIn || *got.Email != "k@example.com" || !got.RefreshExpiresAt.Equal(exp) || got.CheckedAt == nil {
		t.Fatalf("login %+v %v", got, err)
	}
	got, err = s.RecordAccountLogin(ctx, a.ID, store.AccountLogin{})
	if err != nil || got.LoggedIn || got.Email == nil || *got.Email != "k@example.com" || got.RefreshExpiresAt != nil {
		t.Fatalf("logged out %+v %v", got, err)
	}
	if _, err := s.RenameAccount(ctx, a.ID, "ab"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("rename onto a label: %v", err)
	}
	if got, err := s.RenameAccount(ctx, a.ID, "home"); err != nil || got.Label != "home" {
		t.Fatalf("rename %+v %v", got, err)
	}

	// Terminals name their account; one of another box's is refused.
	tm, err := s.CreateTerminal(ctx, store.NewBoxTerminal{WorkspaceID: b.ID, Preset: "login", Title: "x", Cols: 80, Rows: 24, CreatedBy: "k", AccountID: a.ID})
	if err != nil || tm.AccountID == nil || *tm.AccountID != a.ID || tm.AccountSlot != a.Slot {
		t.Fatalf("terminal %+v %v", tm, err)
	}
	fa, _ := s.ListAccounts(ctx, other.ID)
	if _, err := s.CreateTerminal(ctx, store.NewBoxTerminal{WorkspaceID: b.ID, Preset: "shell", Title: "x", Cols: 80, Rows: 24, CreatedBy: "k", AccountID: fa[0].ID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another box's account: %v", err)
	}
	if _, err := s.CreateTerminal(ctx, store.NewBoxTerminal{WorkspaceID: b.ID, Preset: "login", Title: "x", Cols: 80, Rows: 24, CreatedBy: "k"}); err == nil {
		t.Fatal("a login terminal without an account")
	}
	plain, _ := s.CreateTerminal(ctx, store.NewBoxTerminal{WorkspaceID: b.ID, Preset: "shell", Title: "x", Cols: 80, Rows: 24, CreatedBy: "k"})
	if err := s.TerminalAttached(ctx, tm.ID, 80, 24); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetAccount(ctx, a.ID); got.LastUsedAt == nil {
		t.Fatal("an attach under the account is not its last use")
	}
	ended, err := s.EndRunningTerminals(ctx, b.ID, store.EndBoxRestarted)
	if err != nil || len(ended) != 2 {
		t.Fatalf("ended %v %v", ended, err)
	}
	if n, _ := s.RunningTerminals(ctx, b.ID); n != 0 {
		t.Fatalf("%d still running", n)
	}
	if got, _ := s.GetTerminal(ctx, plain.ID); got.EndReason == nil || *got.EndReason != store.EndBoxRestarted {
		t.Fatalf("plain terminal %+v", got)
	}
}
