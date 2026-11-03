package claudesync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kensac/orbit/internal/claudeimport"
	"github.com/kensac/orbit/internal/store"
)

// Synthetic entries and skills only.

func TestRenderMemoryReadsBackTheSameFields(t *testing.T) {
	e := store.Entry{Key: "squash-merges", Version: 2, Status: "active", EntryFields: store.EntryFields{Type: "feedback",
		Title: "Squash merges", Description: "Merge PRs: squash, never rebase", Body: "Squash-merge every PR.",
		Why: "One commit per change keeps the history readable.", HowToApply: "Use the squash button."}}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "squash-merges.md"), RenderMemory(e), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, claudeimport.MemoryIndexFile), []byte(indexLine(e, "squash-merges.md")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, warnings, err := claudeimport.ReadMemoryDir(dir)
	if err != nil || len(warnings) != 0 || len(got) != 1 {
		t.Fatalf("read back = %+v %v %v", got, warnings, err)
	}
	if got[0].Key != e.Key || FieldsSHA256(got[0].EntryFields) != FieldsSHA256(e.EntryFields) {
		t.Fatalf("read back %+v, want %+v", got[0], e.EntryFields)
	}
}

func TestPlanIndexKeepsOtherLines(t *testing.T) {
	dir := t.TempDir()
	old := "# Memory index\n\n- [Mine](mine.md) - a line orbit does not own\n- [Old title](a.md) - old\n- [Gone](b.md)\n"
	if err := os.WriteFile(filepath.Join(dir, claudeimport.MemoryIndexFile), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	op, ok, err := planIndex(dir, map[string]string{"a.md": "- [New title](a.md) - new", "b.md": "", "c.md": "- [C](c.md)"})
	if err != nil || !ok {
		t.Fatalf("plan = %+v %v %v", op, ok, err)
	}
	want := "# Memory index\n\n- [Mine](mine.md) - a line orbit does not own\n- [New title](a.md) - new\n- [C](c.md)\n"
	if got := string(op.files[""]); got != want {
		t.Fatalf("index =\n%s\nwant\n%s", got, want)
	}
}
