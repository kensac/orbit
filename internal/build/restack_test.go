package build

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/build/buildtest"
)

const childTicket = "5b5b5b5b-0000-4000-8000-000000000002"

// onMain commits files to main in the bare repository, as a squash merge
// or another person's change lands there. With from, the commit carries
// that branch's tree (a squash merge of it) plus files.
func onMain(t *testing.T, bare, from string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "clone", "--quiet", bare, "c")
	c := filepath.Join(dir, "c")
	if from != "" {
		runGit(t, c, "merge", "--quiet", "--squash", "origin/"+from)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(c, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, c, "add", ".")
	runGit(t, c, "commit", "--quiet", "-m", "on main")
	runGit(t, c, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	return buildtest.RevParse(bare, "refs/heads/main")
}

func TestAStackedTicketBuildsOnItsParentAndIsRestackedOntoMain(t *testing.T) {
	w := newWorld(t)
	parent, err := w.runner.Run(ctx, w.job("77777777-7777-4777-8777-777777777777", write("/work/hello.txt", "hi\n")), w.deps())
	if err != nil {
		t.Fatal(err)
	}
	cj := w.job("88888888-8888-4888-8888-888888888888", write("/work/child.txt", "child\n"))
	cj.TicketID, cj.StackBranch, cj.StackPR = childTicket, parent.Branch, parent.PRNumber
	child, err := w.runner.Run(ctx, cj, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	if child.BaseSHA != parent.HeadSHA || runGit(t, w.bare, "log", "-1", "--format=%P", "refs/heads/orbit/5b5b5b5b") != parent.HeadSHA {
		t.Fatalf("child = %+v, want it built on %s", child, parent.HeadSHA)
	}
	w.gh.Mu.Lock()
	created := w.gh.Created
	w.gh.Mu.Unlock()
	if len(created) != 2 || created[1]["base"] != parent.Branch || !strings.Contains(created[1]["body"].(string), "Stacked on #7") {
		t.Fatalf("created = %v", created)
	}

	// The parent squash-merges; the child's own commit moves onto main.
	main := onMain(t, w.bare, parent.Branch, nil)
	who := Identity{Name: "orbit-kensac[bot]", Email: "bot@example.com"}
	res, err := w.runner.Restack(ctx, w.app, Restack{Repo: "kensac/orbit", TicketID: childTicket, From: child.BaseSHA, Head: child.HeadSHA, Committer: who})
	if err != nil {
		t.Fatal(err)
	}
	head := buildtest.RevParse(w.bare, "refs/heads/orbit/5b5b5b5b")
	if res.Base != main || res.Branch != "main" || res.Head != head ||
		runGit(t, w.bare, "log", "-1", "--format=%P", head) != main {
		t.Fatalf("restacked = %+v, branch %s, main %s", res, head, main)
	}
	if got := runGit(t, w.bare, "diff", "--name-only", main, head); got != "child.txt" {
		t.Fatalf("the branch over main changes %q, want only the child's file", got)
	}
	msg := runGit(t, w.bare, "log", "-1", "--format=%an|%cn|%B", head)
	if !strings.HasPrefix(msg, "orbit-kensac[bot]|orbit-kensac[bot]|") || !strings.Contains(msg, "Orbit-Run: 88888888-8888-4888-8888-888888888888") {
		t.Fatalf("restacked commit = %q", msg)
	}

	// Again, with nothing moved: the same commit, nothing new.
	again, err := w.runner.Restack(ctx, w.app, Restack{Repo: "kensac/orbit", TicketID: childTicket, From: res.Base, Head: res.Head, Committer: who})
	if err != nil || again.Head != res.Head {
		t.Fatalf("a restack with nothing to move = %+v %v", again, err)
	}

	// Main changes the child's file: the replay conflicts, aborts and
	// pushes nothing.
	onMain(t, w.bare, "", map[string]string{"child.txt": "someone else\n"})
	_, err = w.runner.Restack(ctx, w.app, Restack{Repo: "kensac/orbit", TicketID: childTicket, From: res.Base, Head: res.Head, Committer: who})
	if !errors.Is(err, ErrRestackConflict) || !strings.Contains(err.Error(), "child.txt") {
		t.Fatalf("conflict = %v", err)
	}
	if got := buildtest.RevParse(w.bare, "refs/heads/orbit/5b5b5b5b"); got != res.Head {
		t.Fatalf("a conflicted restack moved the branch to %s", got)
	}
	// A branch that moved since orbit recorded it is not restacked.
	if _, err := w.runner.Restack(ctx, w.app, Restack{Repo: "kensac/orbit", TicketID: childTicket, From: res.Base, Head: main, Committer: who}); err == nil ||
		!strings.Contains(err.Error(), "not the") {
		t.Fatalf("a moved branch = %v", err)
	}
}
