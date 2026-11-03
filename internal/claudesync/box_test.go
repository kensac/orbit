package claudesync

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNewBoxBundleIncludesOnlyInstructionsAndSkills(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("CLAUDE.md", "Use the project rules.\n")
	write("settings.json", `{"secret":"do-not-copy"}`)
	write("skills/review/SKILL.md", "---\nname: review\ndescription: Review changes.\n---\nRead carefully.\n")
	write("skills/review/references/checklist.md", "Check the tests.\n")
	write("plugins/installed_plugins.json", `{"plugin":"do-not-copy"}`)

	first, err := NewBoxBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewBoxBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == "" || first.Digest != second.Digest || !bytes.Equal(first.Archive, second.Archive) {
		t.Fatal("the same config did not produce the same bundle")
	}
	want := []string{"CLAUDE.md", "skills/review/SKILL.md", "skills/review/references/checklist.md"}
	var got []string
	tr := tar.NewReader(bytes.NewReader(first.Archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, h.Name)
		if h.ModTime.Unix() != 0 || h.Typeflag != tar.TypeReg {
			t.Fatalf("entry header = %+v", h)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bundle files = %v, want %v", got, want)
	}
	if len(first.Files) != len(want) || len(first.Warnings) != 0 {
		t.Fatalf("files/warnings = %v/%v", first.Files, first.Warnings)
	}
}

func TestNewBoxBundleRejectsSymlinkRootsAndInstructionFile(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := NewBoxBundle(root); err == nil {
		t.Fatal("a symlinked CLAUDE.md was accepted")
	}
	link := filepath.Join(t.TempDir(), "claude-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := NewBoxBundle(link); err == nil {
		t.Fatal("a symlinked config directory was accepted")
	}
}
