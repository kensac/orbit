package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapshotOf(t *testing.T, files map[string]string) *Snapshot {
	t.Helper()
	dir := t.TempDir()
	for rel, c := range files {
		write(t, dir, rel, c)
	}
	_ = os.Symlink("../outside", filepath.Join(dir, "link"))
	_ = os.MkdirAll(filepath.Join(dir, "empty"), 0o700)
	archive, _, err := Pack(Tree{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(archive, func(name string) bool { return name == ".orbit" || strings.HasPrefix(name, ".orbit/") })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSnapshotIndexesTheArchive(t *testing.T) {
	s := snapshotOf(t, map[string]string{
		"src/main.go": "package main\n", "src/b.go": "package b\n", "README.md": "# hi\n", "a.txt": "a",
		".orbit/claude/x.jsonl": "{}",
	})
	var names []string
	for _, e := range s.Children("") {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != "empty,src,README.md,a.txt,link" {
		t.Fatalf("root = %s (directories first, then by name; .orbit hidden)", got)
	}
	e, err := s.Lookup("src/main.go")
	if err != nil || e.Kind != KindFile || string(e.Data()) != "package main\n" || e.Bytes != 13 {
		t.Fatalf("main.go = %+v %v", e, err)
	}
	if v := s.VersionOf(e); v != Version([]byte("package main\n")) || !strings.HasPrefix(v, "sha256:") {
		t.Fatalf("version = %s", v)
	}
	if l, _ := s.Lookup("link"); l.Kind != KindSymlink || l.Target != "../outside" {
		t.Fatalf("link = %+v", l)
	}
	if _, err := s.Lookup("link/x"); !errors.Is(err, ErrPath) {
		t.Fatalf("through a symlink = %v", err)
	}
	if _, err := s.Lookup("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing = %v", err)
	}
	if _, err := s.Lookup(".orbit/claude/x.jsonl"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("hidden = %v", err)
	}
	if !s.HasChildren("src") || s.HasChildren("empty") {
		t.Fatal("has_children")
	}
}

func TestOpenAddsImpliedDirectories(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "a/b/c.txt", Typeflag: tar.TypeReg, Size: 2, Mode: 0o600})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	s, err := Open(buf.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := s.Lookup("a/b"); err != nil || d.Kind != KindDir {
		t.Fatalf("a/b = %+v %v", d, err)
	}
	if e, _ := s.Lookup("a/b/c.txt"); string(e.Data()) != "hi" {
		t.Fatalf("c.txt = %q", e.Data())
	}
}

func TestCleanPath(t *testing.T) {
	for _, ok := range []string{"", "a", "a/b.txt", ".gitignore", "a b/c"} {
		if _, err := CleanPath(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"/a", "a/", "a//b", "./a", "..", "../a", "a/../b", ".", "a\x00b"} {
		if _, err := CleanPath(bad); !errors.Is(err, ErrPath) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestIsText(t *testing.T) {
	if !IsText([]byte("héllo\r\n")) || !IsText(nil) {
		t.Fatal("text")
	}
	if IsText([]byte("a\x00b")) || IsText([]byte{0xff, 0xfe}) {
		t.Fatal("binary")
	}
	late := append(bytes.Repeat([]byte("a"), binarySniff), 0)
	if !IsText(late) {
		t.Fatal("a NUL after the first 8000 bytes is text, as git decides")
	}
}

func TestIgnored(t *testing.T) {
	s := snapshotOf(t, map[string]string{
		".gitignore":          "# comment\n*.log\n/build/\nnode_modules\n!keep.log\ndocs/**/*.tmp\n",
		"app.log":             "",
		"keep.log":            "",
		"build/out":           "",
		"sub/build/out":       "",
		"sub/node_modules/x":  "",
		"docs/a/b/c.tmp":      "",
		"docs/c.tmp":          "",
		"sub/.gitignore":      "local.txt\n",
		"sub/local.txt":       "",
		"local.txt":           "",
		"src/main.go":         "",
		"src/deep/trace.log":  "",
		"node_modules_x/file": "",
	})
	want := map[string]bool{
		"app.log": true, "keep.log": false, "build": true, "build/out": true, "sub/build/out": false,
		"sub/node_modules": true, "sub/node_modules/x": true, "docs/a/b/c.tmp": true, "docs/c.tmp": true,
		"sub/local.txt": true, "local.txt": false, "src/main.go": false, "src/deep/trace.log": true, "node_modules_x/file": false,
	}
	for p, ig := range want {
		e, err := s.Lookup(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if got := s.Ignored(e); got != ig {
			t.Errorf("%s ignored = %v, want %v", p, got, ig)
		}
	}
}

func TestWriteTarGz(t *testing.T) {
	s := snapshotOf(t, map[string]string{"src/main.go": "package main\n", "src/x/y.txt": "y", "other": "o"})
	dir, _ := s.Lookup("src")
	var buf bytes.Buffer
	if err := s.WriteTarGz(&buf, dir); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	if got["src/main.go"] != "package main\n" || got["src/x/y.txt"] != "y" || len(got) != 4 {
		t.Fatalf("tar = %v", got)
	}
	if _, ok := got["other"]; ok {
		t.Fatal("a directory's download is its subtree")
	}
}
