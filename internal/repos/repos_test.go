package repos

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var ctx = context.Background()

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Ann", "GIT_AUTHOR_EMAIL=ann@example.com", "GIT_COMMITTER_NAME=Ann", "GIT_COMMITTER_EMAIL=ann@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// remote is a bare repository with two commits on main, a tag, a symlink,
// a file under .github, and a pull request head, as GitHub keeps one.
func remote(t *testing.T) (bare, work string) {
	t.Helper()
	root := t.TempDir()
	bare, work = filepath.Join(root, "orbit.git"), filepath.Join(root, "work")
	gitIn(t, root, "init", "--quiet", "--bare", bare)
	gitIn(t, root, "init", "--quiet", "-b", "main", work)
	write := func(name, body string) {
		p := filepath.Join(work, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "# orbit\n")
	write("internal/x.go", "package x\n")
	write(".github/workflows/ci.yml", "on: push\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(work, "escape")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "add", ".")
	gitIn(t, work, "commit", "--quiet", "-m", "first")
	gitIn(t, work, "tag", "v1")
	write("README.md", "# orbit\n\nStanding agents.\n")
	gitIn(t, work, "commit", "--quiet", "-am", "second: describe it")
	gitIn(t, work, "push", "--quiet", bare, "main", "v1")
	gitIn(t, work, "checkout", "--quiet", "-b", "feature")
	write("internal/y.go", "package x\n\nvar Y = 1\n")
	gitIn(t, work, "add", ".")
	gitIn(t, work, "commit", "--quiet", "-m", "a pull request")
	gitIn(t, work, "push", "--quiet", bare, "feature:refs/pull/5/head")
	gitIn(t, work, "checkout", "--quiet", "main")
	gitIn(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return bare, work
}

func TestAMirrorGivesATreeWithoutGitAndExactHistory(t *testing.T) {
	bare, work := remote(t)
	m := &Mirrors{Root: t.TempDir(), URL: func(string) string { return bare }}
	if _, ok := m.Has("kensac/orbit"); ok {
		t.Fatal("a mirror before any fetch")
	}
	if err := m.Fetch(ctx, "kensac/orbit", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Fetch(ctx, "kensac/orbit", nil); err != nil {
		t.Fatalf("a second fetch: %v", err)
	}
	if _, ok := m.Has("kensac/orbit"); !ok {
		t.Fatal("no mirror after a fetch")
	}
	main := gitIn(t, work, "rev-parse", "main")
	for ref, want := range map[string]string{"": main, "main": main, "v1": gitIn(t, work, "rev-parse", "v1^{commit}"), main: main} {
		if got, err := m.Resolve(ctx, "kensac/orbit", ref); err != nil || got != want {
			t.Fatalf("resolve %q = %q %v, want %s", ref, got, err, want)
		}
	}
	for _, bad := range []string{"--output=/tmp/x", "nope"} {
		if _, err := m.Resolve(ctx, "kensac/orbit", bad); err == nil {
			t.Fatalf("resolved %q", bad)
		}
	}
	// The mirror's config names no remote and no credential.
	dir, _ := m.Dir("kensac/orbit")
	cfg, _ := os.ReadFile(filepath.Join(dir, "config"))
	if strings.Contains(string(cfg), "remote") || strings.Contains(string(cfg), bare) {
		t.Fatalf("the mirror's config: %s", cfg)
	}

	dest := filepath.Join(t.TempDir(), "repos", "orbit")
	files, skipped, err := m.Extract(ctx, "kensac/orbit", main, dest)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	if want := []string{".github/workflows/ci.yml", "README.md", "internal/x.go"}; !slices.Equal(files, want) || skipped != 1 {
		t.Fatalf("files %v skipped %d, want %v and the symlink skipped", files, skipped, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "README.md")); !strings.Contains(string(b), "Standing agents.") {
		t.Fatalf("README = %q", b)
	}
	for _, p := range []string{".git", "escape"} {
		if _, err := os.Lstat(filepath.Join(dest, p)); !os.IsNotExist(err) {
			t.Fatalf("%s in the tree: %v", p, err)
		}
	}

	log, err := m.Log(ctx, "kensac/orbit", "", "", 0, "")
	if err != nil || len(log) != 2 || log[0].SHA != main || log[0].Subject != "second: describe it" || log[1].Author != "Ann" {
		t.Fatalf("log = %+v %v", log, err)
	}
	if log, _ := m.Log(ctx, "kensac/orbit", "main", "internal", 5, ""); len(log) != 1 || log[0].Subject != "first" {
		t.Fatalf("log of a path = %+v", log)
	}
	if pr, _ := m.Log(ctx, "kensac/orbit", "refs/pull/5/head", "", 1, ""); len(pr) != 1 || pr[0].Subject != "a pull request" {
		t.Fatalf("a PR's head = %+v", pr)
	}
	show, err := m.Show(ctx, "kensac/orbit", main, "")
	if err != nil || !strings.Contains(show, "second: describe it") || !strings.Contains(show, "+Standing agents.") {
		t.Fatalf("show = %q %v", show, err)
	}
	if f, _ := m.Show(ctx, "kensac/orbit", "v1", "README.md"); f != "# orbit\n" {
		t.Fatalf("a file at v1 = %q", f)
	}
	diff, err := m.Diff(ctx, "kensac/orbit", "main", "refs/pull/5/head", "", false)
	if err != nil || !strings.Contains(diff, "+var Y = 1") {
		t.Fatalf("diff = %q %v", diff, err)
	}
	if stat, _ := m.Diff(ctx, "kensac/orbit", "main", "refs/pull/5/head", "", true); !strings.Contains(stat, "internal/y.go") || strings.Contains(stat, "+var") {
		t.Fatalf("stat = %q", stat)
	}
	blame, err := m.Blame(ctx, "kensac/orbit", "", "README.md", 3, 3)
	if err != nil || !strings.Contains(blame, main) || !strings.Contains(blame, "Standing agents.") || strings.Contains(blame, "# orbit") {
		t.Fatalf("blame = %q %v", blame, err)
	}
	// Nothing the model passes is read as an option.
	for _, call := range []func() error{
		func() error { _, err := m.Show(ctx, "kensac/orbit", "--output=/tmp/orbit-x", ""); return err },
		func() error { _, err := m.Diff(ctx, "kensac/orbit", "main", "--ext-diff", "", false); return err },
		func() error { _, err := m.Log(ctx, "kensac/orbit", "main", "--output=x", 1, ""); return err },
		func() error { _, err := m.Blame(ctx, "kensac/orbit", "-L1,2", "README.md", 0, 0); return err },
	} {
		if call() == nil {
			t.Fatal("an option passed as a revision or path was run")
		}
	}
	if _, err := m.Dir("../x"); err == nil {
		t.Fatal("a repository outside owner/name")
	}
}

func TestClipSaysWhatItCut(t *testing.T) {
	s := strings.Repeat("line of text\n", 100)
	got := Clip(s, 100)
	if len(got) > 200 || !strings.Contains(got, "cut this answer") || !strings.HasPrefix(s, strings.SplitN(got, "\n[orbit", 2)[0]) {
		t.Fatalf("clip = %q", got)
	}
	if Clip("short", 100) != "short" {
		t.Fatal("clipped a short answer")
	}
}
