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
	if err != nil || meta.Archive != nil || meta.Bytes != 3 || *meta.RunID != r.RunID {
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

func TestAPersonsWriteIsASnapshotOfItsOwn(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Body: "hi", Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	tree, _ := s.ConversationWorkspace(ctx, r.ConversationID)
	write := func(base int64, archive string, ev ...store.WorkspaceEventIn) (*store.WriteResult, error) {
		return s.PutPersonWrite(ctx, store.PersonWrite{WorkspaceID: tree.ID, ConversationID: r.ConversationID, BaseID: base,
			Archive: []byte(archive), Person: "kanishk", RequestID: "req-1", Events: ev})
	}
	first, err := write(0, "v1", store.WorkspaceEventIn{Op: "write", Path: "a.txt", VersionAfter: "sha256:1"})
	if err != nil || first.Snapshot.Source != "person" || *first.Snapshot.Author != "kanishk" || first.Snapshot.RunID != nil {
		t.Fatalf("first = %+v %v", first, err)
	}
	if _, err := write(0, "v2"); !errors.Is(err, store.ErrStale) {
		t.Fatalf("a write based on an older snapshot = %v", err)
	}
	// While a run of the conversation is running, the write is refused.
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil {
		t.Fatalf("claim = %+v %v", c, err)
	}
	if _, err := write(first.Snapshot.ID, "v2"); !errors.Is(err, store.ErrWorkspaceBusy) {
		t.Fatalf("busy = %v", err)
	}
	// The run sees the person's version: what changed is the person's
	// snapshot against nothing a run stored.
	latest, seen, err := s.PersonChanges(ctx, r.ConversationID)
	if err != nil || latest == nil || latest.ID != first.Snapshot.ID || string(latest.Archive) != "v1" || seen != nil {
		t.Fatalf("person changes = %+v %+v %v", latest, seen, err)
	}
	if w, err := s.LatestWorkspace(ctx, r.ConversationID); err != nil || string(w.Archive) != "v1" {
		t.Fatalf("restore reads = %+v %v", w, err)
	}
	// The run's snapshot is stored even unchanged: the agent has seen it.
	if stored, err := s.PutWorkspace(ctx, r.ConversationID, c.ID, c.Attempt, 0, []byte("v1")); err != nil || !stored {
		t.Fatalf("run snapshot after a person's = %v %v", stored, err)
	}
	if stored, _ := s.PutWorkspace(ctx, r.ConversationID, c.ID, c.Attempt, 0, []byte("v1")); stored {
		t.Fatal("an unchanged run snapshot after a run's is not stored")
	}
	if latest, _, _ := s.PersonChanges(ctx, r.ConversationID); latest != nil {
		t.Fatal("nothing new from the person once a run has seen it")
	}
	page, err := s.ListWorkspaceEvents(ctx, tree.ID, 0, 0)
	if err != nil || len(page.Events) != 1 || page.Events[0].Op != "write" || *page.Events[0].Person != "kanishk" ||
		*page.Events[0].RequestID != "req-1" || page.Events[0].VersionBefore != nil {
		t.Fatalf("events = %+v %v", page, err)
	}
}

func TestTrashKeepsItsContentAndRestores(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r, _, _ := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Title: "x"})
	tree, _ := s.ConversationWorkspace(ctx, r.ConversationID)
	pw := store.PersonWrite{WorkspaceID: tree.ID, ConversationID: r.ConversationID, Archive: []byte("with")}
	base, err := s.PutPersonWrite(ctx, pw)
	if err != nil {
		t.Fatal(err)
	}
	pw.BaseID, pw.Archive = base.Snapshot.ID, []byte("without")
	pw.Trash = []store.TrashIn{{Path: "old.md", Kind: "file", Bytes: 3}}
	res, err := s.PutPersonWrite(ctx, pw)
	if err != nil || len(res.Trash) != 1 {
		t.Fatalf("trash = %+v %v", res, err)
	}
	item := res.Trash[0]
	if item.SnapshotID != base.Snapshot.ID || item.Path != "old.md" || item.RestoredAt != nil || item.By != nil {
		t.Fatalf("item = %+v: its content is the snapshot before", item)
	}
	if got, err := s.GetTrashItem(ctx, tree.ID, item.ID); err != nil || got.ID != item.ID {
		t.Fatalf("get = %+v %v", got, err)
	}
	other, _, _ := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Title: "y"})
	otherTree, _ := s.ConversationWorkspace(ctx, other.ConversationID)
	if _, err := s.GetTrashItem(ctx, otherTree.ID, item.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another workspace's item = %v", err)
	}
	pw.BaseID, pw.Archive, pw.Trash, pw.Restored = res.Snapshot.ID, []byte("with again"), nil, item.ID
	if _, err := s.PutPersonWrite(ctx, pw); err != nil {
		t.Fatal(err)
	}
	items, next, err := s.ListTrash(ctx, tree.ID, nil, 0)
	if err != nil || len(items) != 1 || items[0].RestoredAt == nil || next != nil {
		t.Fatalf("list = %+v %v %v", items, next, err)
	}
	// A signed upload's nonce uploads once.
	last, _ := s.LatestSnapshot(ctx, r.ConversationID)
	pw = store.PersonWrite{WorkspaceID: tree.ID, ConversationID: r.ConversationID, BaseID: last.ID, Archive: []byte("u1"),
		Events: []store.WorkspaceEventIn{{Op: "upload", Path: "f", Nonce: "n1"}}}
	up, err := s.PutPersonWrite(ctx, pw)
	if err != nil {
		t.Fatal(err)
	}
	pw.BaseID, pw.Archive = up.Snapshot.ID, []byte("u2")
	if _, err := s.PutPersonWrite(ctx, pw); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a used nonce = %v", err)
	}
	if now, _ := s.LatestSnapshot(ctx, r.ConversationID); now.ID != up.Snapshot.ID {
		t.Fatal("a refused write stores nothing")
	}
}
