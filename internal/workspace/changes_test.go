package workspace

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func describe(cs []PathChange) string {
	var out []string
	for _, c := range cs {
		s := c.Op + " " + c.Kind + " " + c.Path
		if c.From != "" {
			s += " from " + c.From
		}
		if (c.Version != "") != (c.Kind == KindFile && c.Op != OpTrash) {
			s += " (version wrong)"
		}
		out = append(out, s)
	}
	return strings.Join(out, "\n")
}

func TestChangesListEveryPathThatDiffers(t *testing.T) {
	s := snapshotOf(t, map[string]string{"src/a.go": "a", "src/b.go": "b", "notes.md": "n", "old/x.txt": "x"})
	e := s.Edit(time.Now())
	for _, step := range []error{
		e.PutFile("src/a.go", []byte("A")),
		e.Move("src/b.go", "src/c.go"),
		e.Remove("old"),
		e.Mkdir("made"),
		e.Remove("notes.md"),
		e.Mkdir("notes.md"),
	} {
		if step != nil {
			t.Fatal(step)
		}
	}
	after := reopen(t, e)
	want := strings.Join([]string{
		"create dir made",
		"trash file notes.md",
		"create dir notes.md",
		"trash dir old",
		"trash file old/x.txt",
		"modify file src/a.go",
		"rename file src/c.go from src/b.go",
	}, "\n")
	if got := describe(Changes(s, after)); got != want {
		t.Fatalf("changes:\n%s\nwant\n%s", got, want)
	}
	if got := Changes(after, after); len(got) != 0 {
		t.Fatalf("no change = %v", got)
	}
	// From nothing, every path is created, parents first.
	first := describe(Changes(nil, s))
	if !strings.HasPrefix(first, "create dir empty\ncreate symlink link\ncreate file notes.md\ncreate dir old\ncreate file old/x.txt\ncreate dir src\n") {
		t.Fatalf("from nothing:\n%s", first)
	}
	if c := Changes(s, after); c[5].Version != after.VersionOf(mustLookup(t, after, "src/a.go")) {
		t.Fatal(fmt.Sprint("the modify carries the new version: ", c[5]))
	}
}

func mustLookup(t *testing.T, s *Snapshot, p string) *Entry {
	t.Helper()
	e, err := s.Lookup(p)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
