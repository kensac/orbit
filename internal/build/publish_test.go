package build

import (
	"bytes"
	"github.com/kensac/orbit/internal/build/buildtest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneAndPublish(t *testing.T) {
	w := newWorld(t)
	gg := Git{WorkRoot: t.TempDir(), CloneURL: func(string) string { return w.bare }}
	tree, err := gg.Clone(ctx, w.app, "kensac/orbit")
	if err != nil {
		t.Fatal(err)
	}
	// The agent's workspace: the clone, a change, and a .git of its own
	// that orbit must not use.
	ws := t.TempDir()
	if err := applyTree(ws, bytes.NewReader(tree)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "README.md")); err != nil {
		t.Fatalf("clone: %v", err)
	}
	_ = os.WriteFile(filepath.Join(ws, "notes.md"), []byte("from the chat\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(ws, ".git"), 0o700)
	_ = os.WriteFile(filepath.Join(ws, ".git", "config"), []byte("[core]\n\tfsmonitor = touch /tmp/pwned\n"), 0o600)
	packed, err := packTree(ws)
	if err != nil {
		t.Fatal(err)
	}
	author, _ := ParseIdentity("orbit-kensac[bot] <bot@example.com>")
	branch, err := ChatBranch("notes")
	if err != nil || branch != "orbit/chat/notes" {
		t.Fatalf("branch %q %v", branch, err)
	}
	res, err := gg.Publish(ctx, PublishRequest{App: w.app, Repo: "kensac/orbit", Branch: branch, Tree: packed,
		Title: "Add notes", Body: "Notes.\n\nCo-Authored-By: Claude <noreply@anthropic.com>", Author: author, RunID: "r1",
		Policy: DiffPolicy{Secrets: []string{testCredential}}, OpenPR: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.HeadSHA != buildtest.RevParse(w.bare, "refs/heads/orbit/chat/notes") || res.Files != 1 || res.PRNumber == 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := runGit(t, w.bare, "show", "--name-only", "--format=%s", "refs/heads/orbit/chat/notes"); !strings.Contains(got, "notes.md") || strings.Contains(got, "config") {
		t.Fatalf("commit = %q", got)
	}
	w.gh.Mu.Lock()
	created := w.gh.Created
	w.gh.Mu.Unlock()
	if len(created) != 1 || strings.Contains(created[0]["body"].(string), "Co-Authored-By") {
		t.Fatalf("created = %v", created)
	}
	// The credential the sandbox held never leaves in a commit.
	_ = os.WriteFile(filepath.Join(ws, "leak.txt"), []byte(testCredential+"\n"), 0o600)
	packed, _ = packTree(ws)
	if _, err := gg.Publish(ctx, PublishRequest{App: w.app, Repo: "kensac/orbit", Branch: branch, Tree: packed, Title: "x",
		Author: author, RunID: "r2", Policy: DiffPolicy{Secrets: []string{testCredential}}}); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("leak = %v", err)
	}
	for _, bad := range []string{"main", "../x", "orbit/3f2a9c1e", ""} {
		if b, err := ChatBranch(bad); err == nil && !strings.HasPrefix(b, ChatBranchPrefix) {
			t.Errorf("%q -> %q", bad, b)
		}
	}
	if _, err := gg.Publish(ctx, PublishRequest{Branch: "main", Title: "x"}); err == nil {
		t.Fatal("pushed to main")
	}
}
