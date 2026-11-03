package store_test

import (
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// The change index is read only up to the first snapshot not indexed, so
// a reader never skips one; indexing twice writes once.
func TestTheChangeIndexIsReadUpToItsFirstGap(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Body: "hi", Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	conv := r.ConversationID
	for _, tar := range []string{"one", "two", "three"} {
		// The same run storing three different trees, as three attempts would.
		if _, err := s.PutWorkspace(ctx, conv, r.RunID, 1, 0, []byte(tar)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.UnindexedSnapshots(ctx, conv, 10)
	if err != nil || len(list) != 3 {
		t.Fatalf("unindexed = %v %v", list, err)
	}
	if head, err := s.IndexedHead(ctx, conv); err != nil || head != 0 {
		t.Fatalf("head before = %d %v", head, err)
	}
	put := func(w store.Workspace, path string) {
		t.Helper()
		if err := s.IndexSnapshot(ctx, &w, []store.WorkspaceChangeIn{{Op: "create", Path: path, Kind: "file", Version: "sha256:" + path}}); err != nil {
			t.Fatal(err)
		}
	}
	put(list[0], "a")
	put(list[2], "c")
	head, err := s.IndexedHead(ctx, conv)
	if err != nil || head != list[0].ID {
		t.Fatalf("head with a gap = %d %v, want %d", head, err, list[0].ID)
	}
	got, err := s.WorkspaceChanges(ctx, conv, 0, 0, head, 10)
	if err != nil || len(got) != 1 || got[0].Path != "a" || got[0].Source != "run" || *got[0].RunID != r.RunID {
		t.Fatalf("changes = %+v %v", got, err)
	}
	put(list[1], "b")
	put(list[1], "again")
	head, _ = s.IndexedHead(ctx, conv)
	got, _ = s.WorkspaceChanges(ctx, conv, list[0].ID, 1<<30, head, 10)
	if head != list[2].ID || len(got) != 2 || got[0].Path != "b" || got[1].Path != "c" {
		t.Fatalf("after the gap = %d %+v", head, got)
	}

	// The run's changes lie between the snapshot before its first and its last.
	rs, err := s.GetRunSnapshots(ctx, r.RunID)
	if err != nil || rs.WorkspaceID == nil || rs.Before != nil || rs.After.ID != list[2].ID || rs.Final {
		t.Fatalf("run snapshots = %+v %v", rs, err)
	}
	states, err := s.RunChangeStates(ctx, []string{r.RunID})
	if err != nil || len(states) != 0 {
		t.Fatalf("a running run has no summary to compute = %v %v", states, err)
	}
}
