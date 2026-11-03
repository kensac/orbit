package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestConversationPermissionsAreVersions(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	c, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a"})
	if err != nil {
		t.Fatal(err)
	}
	if cp, err := s.ConversationPermission(ctx, c.ConversationID); err != nil || cp != nil {
		t.Fatalf("a new conversation has %v, %v", cp, err)
	}
	soon := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	first, err := s.SetConversationPermission(ctx, c.ConversationID, "edits", "kanishk", &soon)
	if err != nil || first.Version != 1 || first.Level != "edits" || first.SetBy != "kanishk" || !first.ExpiresAt.Equal(soon) {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := s.SetConversationPermission(ctx, c.ConversationID, "", "kanishk", nil)
	if err != nil || second.Version != 2 {
		t.Fatalf("second = %+v, %v", second, err)
	}
	cp, err := s.ConversationPermission(ctx, c.ConversationID)
	if err != nil || cp.Version != 2 || cp.InForce(time.Now()) {
		t.Fatalf("latest = %+v, %v: level '' is the agent's own, never in force", cp, err)
	}
	hist, err := s.ConversationPermissions(ctx, c.ConversationID, 10)
	if err != nil || len(hist) != 2 || hist[0].Version != 2 || hist[1].Level != "edits" {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	if !first.InForce(time.Now()) || first.InForce(soon) {
		t.Error("a version is in force until it expires")
	}
	if _, err := s.SetConversationPermission(ctx, "00000000-0000-0000-0000-000000000000", "ask", "k", nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unknown conversation: %v", err)
	}
}
