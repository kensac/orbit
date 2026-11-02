package store_test

import (
	"errors"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestAConversationsWorkspaceIsMadeOnceAndWithItsFirstSnapshot(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Body: "hi", Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.GetConversation(ctx, r.ConversationID)
	if err != nil || c.WorkspaceID != nil {
		t.Fatalf("before = %+v %v", c, err)
	}
	if _, err := s.PutWorkspace(ctx, r.ConversationID, r.RunID, 1, 0, []byte("tar")); err != nil {
		t.Fatal(err)
	}
	c, _ = s.GetConversation(ctx, r.ConversationID)
	if c.WorkspaceID == nil {
		t.Fatal("the first snapshot makes the workspace")
	}
	got, err := s.EnsureConversationWorkspaces(ctx, []string{r.ConversationID})
	if err != nil || got[r.ConversationID] != *c.WorkspaceID {
		t.Fatalf("ensure = %v %v, want the same id", got, err)
	}
	tree, err := s.ConversationWorkspace(ctx, r.ConversationID)
	if err != nil || tree.ID != *c.WorkspaceID || tree.Scope != "conversation" || tree.Agent != "a" || *tree.ConversationID != r.ConversationID {
		t.Fatalf("tree = %+v %v", tree, err)
	}
	if again, err := s.GetWorkspaceTree(ctx, tree.ID); err != nil || again.ID != tree.ID {
		t.Fatalf("by id = %+v %v", again, err)
	}
	if _, err := s.GetWorkspaceTree(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("not an id = %v", err)
	}
	meta, err := s.LatestSnapshot(ctx, r.ConversationID)
	if err != nil || meta.Archive != nil || meta.Bytes != 3 || meta.RunID != r.RunID {
		t.Fatalf("latest = %+v %v", meta, err)
	}
	if b, err := s.SnapshotArchive(ctx, meta.ID); err != nil || string(b) != "tar" {
		t.Fatalf("archive = %q %v", b, err)
	}
	page, err := s.ListWorkspaceEvents(ctx, tree.ID, 0, 0)
	if err != nil || len(page.Events) != 0 || page.HasMore {
		t.Fatalf("events = %+v %v", page, err)
	}

	r2, _, _ := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Title: "x"})
	got, err = s.EnsureConversationWorkspaces(ctx, []string{r.ConversationID, r2.ConversationID, "00000000-0000-0000-0000-000000000000"})
	if err != nil || len(got) != 2 || got[r2.ConversationID] == "" {
		t.Fatalf("ensure two = %v %v", got, err)
	}
}
