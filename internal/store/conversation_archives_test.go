package store_test

import (
	"encoding/json"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestConversationArchiveImportsAppendReadOnlyVersions(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	base := store.NewConversationArchive{
		TargetAgent: "a", SourceAgent: "source", SourceConversationID: "10000000-0000-4000-8000-000000000001",
		SourceBundleSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Snapshot:           json.RawMessage(`{"conversation":{"id":"10000000-0000-4000-8000-000000000001"},"pages":[{"items":[],"has_more":false,"next_before":null}]}`),
		ImportedBy:         "kanishk",
	}
	results, err := s.ImportConversationArchives(ctx, []store.NewConversationArchive{base}, true)
	if err != nil || len(results) != 1 || results[0].Outcome != "created" || results[0].ID != 0 {
		t.Fatalf("dry run = %+v, %v", results, err)
	}
	archives, _, err := s.ListConversationArchives(ctx, "a", 50, 0)
	if err != nil || len(archives) != 0 {
		t.Fatalf("dry run wrote archives: %+v, %v", archives, err)
	}
	results, err = s.ImportConversationArchives(ctx, []store.NewConversationArchive{base}, false)
	if err != nil || len(results) != 1 || results[0].Outcome != "created" || results[0].Version != 1 || results[0].ID == 0 {
		t.Fatalf("first import = %+v, %v", results, err)
	}
	firstID := results[0].ID
	equivalent := base
	equivalent.Snapshot = json.RawMessage(`{ "pages" : [ {"next_before":null,"has_more":false,"items":[]} ], "conversation":{"id":"10000000-0000-4000-8000-000000000001"} }`)
	results, err = s.ImportConversationArchives(ctx, []store.NewConversationArchive{equivalent}, false)
	if err != nil || results[0].Outcome != "unchanged" || results[0].ID != firstID {
		t.Fatalf("repeat import = %+v, %v", results, err)
	}
	changed := base
	changed.Snapshot = json.RawMessage(`{"conversation":{"id":"10000000-0000-4000-8000-000000000001"},"pages":[{"items":[{"kind":"message"}],"has_more":false,"next_before":null}]}`)
	results, err = s.ImportConversationArchives(ctx, []store.NewConversationArchive{changed}, false)
	if err != nil || results[0].Outcome != "versioned" || results[0].Version != 2 {
		t.Fatalf("changed import = %+v, %v", results, err)
	}
	var versions, messages, runs int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM conversation_archives WHERE target_agent='a'`).Scan(&versions); err != nil || versions != 2 {
		t.Fatalf("archive versions = %d, %v", versions, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE agent='a'`).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("operational messages = %d, %v", messages, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE agent='a'`).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("operational runs = %d, %v", runs, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE conversation_archives SET imported_by='changed' WHERE id=$1`, results[0].ID); err == nil {
		t.Fatal("archive update succeeded")
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM conversation_archives WHERE target_agent='a'`); err == nil {
		t.Fatal("archive delete succeeded")
	}
	archiveVersions, more, err := s.ListConversationArchives(ctx, "a", 50, 0)
	if err != nil || more || len(archiveVersions) != 2 || archiveVersions[0].Version != 2 || archiveVersions[1].Version != 1 {
		t.Fatalf("archive versions = %+v, more=%v, err=%v", archiveVersions, more, err)
	}
	if _, err := s.GetConversationArchive(ctx, "a", results[0].ID); err != nil {
		t.Fatalf("get latest archive: %v", err)
	}
}

func TestConversationArchiveImportRejectsInvalidIdentityAndSnapshot(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	archive := store.NewConversationArchive{TargetAgent: "a", SourceAgent: "source", SourceConversationID: "not-a-uuid",
		SourceBundleSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Snapshot: json.RawMessage(`{}`)}
	if _, err := s.ImportConversationArchives(ctx, []store.NewConversationArchive{archive}, false); err == nil {
		t.Fatal("invalid identity was accepted")
	}
}
