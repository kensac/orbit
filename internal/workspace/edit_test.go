package workspace

import (
	"errors"
	"io/fs"
	"testing"
	"time"
)

func reopen(t *testing.T, e *Edit) *Snapshot {
	t.Helper()
	archive, _, err := e.Archive()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(archive, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func content(t *testing.T, s *Snapshot, p string) string {
	t.Helper()
	e, err := s.Lookup(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return string(e.Data())
}

func TestEditLeavesTheSnapshotAndPacksTheChange(t *testing.T) {
	s := snapshotOf(t, map[string]string{"src/a.go": "a", "notes.md": "n"})
	e := s.Edit(time.Now())
	if err := e.PutFile("src/a.go", []byte("A")); err != nil {
		t.Fatal(err)
	}
	if err := e.PutFile("new/deep/b.txt", []byte("b")); err != nil {
		t.Fatal(err)
	}
	if err := e.Mkdir("made"); err != nil {
		t.Fatal(err)
	}
	if err := e.Mkdir("made"); !errors.Is(err, ErrExists) {
		t.Fatalf("mkdir twice = %v", err)
	}
	if err := e.PutFile("notes.md/x", []byte("x")); !errors.Is(err, ErrPath) {
		t.Fatalf("under a file = %v", err)
	}
	if err := e.PutFile("link/x", []byte("x")); !errors.Is(err, ErrPath) {
		t.Fatalf("under a symlink = %v", err)
	}
	if err := e.PutFile("src", []byte("x")); !errors.Is(err, ErrPath) {
		t.Fatalf("over a directory = %v", err)
	}
	after := reopen(t, e)
	if content(t, s, "src/a.go") != "a" {
		t.Fatal("the snapshot an edit started from is not changed")
	}
	if content(t, after, "src/a.go") != "A" || content(t, after, "new/deep/b.txt") != "b" || content(t, after, "notes.md") != "n" {
		t.Fatal("edited content")
	}
	if d, err := after.Lookup("made"); err != nil || d.Kind != KindDir {
		t.Fatalf("made = %+v %v", d, err)
	}
	if l, err := after.Lookup("link"); err != nil || l.Target != "../outside" {
		t.Fatalf("a symlink is kept as written: %+v %v", l, err)
	}
}

func TestMoveRemoveAndInsert(t *testing.T) {
	s := snapshotOf(t, map[string]string{"src/a.go": "a", "src/x/y.go": "y", "b.txt": "b"})
	e := s.Edit(time.Now())
	if err := e.Move("src", "lib/src"); err != nil {
		t.Fatal(err)
	}
	if err := e.Move("lib", "lib/inner"); !errors.Is(err, ErrPath) {
		t.Fatalf("into itself = %v", err)
	}
	if err := e.Move("b.txt", "lib"); !errors.Is(err, ErrExists) {
		t.Fatalf("over an existing path = %v", err)
	}
	if err := e.Move("nope", "x"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing = %v", err)
	}
	if err := e.Remove("b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(""); !errors.Is(err, ErrPath) {
		t.Fatalf("the root = %v", err)
	}
	after := reopen(t, e)
	if content(t, after, "lib/src/x/y.go") != "y" || content(t, after, "lib/src/a.go") != "a" {
		t.Fatal("moved subtree")
	}
	if _, err := after.Lookup("src"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the old path is gone")
	}
	if _, err := after.Lookup("b.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("removed")
	}

	// A restore is an insert from the snapshot that still has it.
	e2 := after.Edit(time.Now())
	if err := e2.Insert(s, "b.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := e2.Insert(s, "src", "lib/src"); !errors.Is(err, ErrExists) {
		t.Fatalf("insert over = %v", err)
	}
	if err := e2.Insert(s, "src", "again/src"); err != nil {
		t.Fatal(err)
	}
	restored := reopen(t, e2)
	if content(t, restored, "b.txt") != "b" || content(t, restored, "again/src/x/y.go") != "y" {
		t.Fatal("restored content")
	}
}

func TestArchiveRefusesTooLarge(t *testing.T) {
	e := emptyEdit()
	if err := e.PutFile("big", make([]byte, MaxBytes)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Archive(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large = %v", err)
	}
}

func emptyEdit() *Edit {
	s, _ := Open(nil, nil)
	return s.Edit(time.Now())
}

func TestDiff(t *testing.T) {
	before := snapshotOf(t, map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c", "same.txt": "s"})
	e := before.Edit(time.Now())
	_ = e.PutFile("a.txt", []byte("A"))
	_ = e.Move("b.txt", "moved/b.txt")
	_ = e.Remove("c.txt")
	_ = e.PutFile("new.txt", []byte("n"))
	after := reopen(t, e)
	got := Diff(before, after)
	want := []Change{{Op: "modified", Path: "a.txt"}, {Op: "removed", Path: "c.txt"}, {Op: "renamed", Path: "moved/b.txt", From: "b.txt"},
		{Op: "added", Path: "new.txt"}}
	if len(got) != len(want) {
		t.Fatalf("diff = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("diff[%d] = %+v, want %+v (all %+v)", i, got[i], want[i], got)
		}
	}
	if d := Diff(nil, after); len(d) != 4 || d[0].Op != "added" {
		t.Fatalf("from nothing = %+v", d)
	}
}
