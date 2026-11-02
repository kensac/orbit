package build

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kensac/orbit/internal/build/buildtest"
	"github.com/kensac/orbit/internal/github"
)

func TestTheCheckoutPoolKeepsATicketsCloneAndWarmsSpares(t *testing.T) {
	w := newWorld(t)
	pool := &Pool{Root: t.TempDir(), Warm: 2, CloneURL: func(string) string { return w.bare }}
	w.runner.Pool = pool

	tok, err := w.app.Token(ctx, "kensac/orbit", github.ReadContents)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := pool.Prewarm(ctx, "kensac/orbit", tok.GitAuthEnv()); err != nil || n != 2 {
		t.Fatalf("prewarm = %d %v", n, err)
	}
	if n, _ := pool.Prewarm(ctx, "kensac/orbit", tok.GitAuthEnv()); n != 0 {
		t.Fatalf("a full pool made %d more", n)
	}
	main := buildtest.RevParse(w.bare, "refs/heads/main")
	spares, _ := filepath.Glob(filepath.Join(pool.Root, "warm", "kensac__orbit", "*"))
	if len(spares) != 2 || buildtest.RevParse(spares[0], "HEAD") != main {
		t.Fatalf("spares = %v", spares)
	}

	// The build takes a spare and keeps it as the ticket's clone.
	res, err := w.runner.Run(ctx, w.job("77777777-7777-4777-8777-777777777777", write("/work/hello.txt", "hi\n")), w.deps())
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := pool.Dir(TicketKey(testTicket))
	if n, _ := pool.Spares("kensac/orbit"); n != 1 || buildtest.RevParse(dir, "HEAD") != res.HeadSHA {
		t.Fatalf("spares %d, the ticket's clone at %s, want %s", n, buildtest.RevParse(dir, "HEAD"), res.HeadSHA)
	}
	// Whatever is left in the kept clone does not reach the next build.
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("left behind\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fix := w.job("88888888-8888-4888-8888-888888888888", write("/work/hello.txt", "hello\n"))
	fix.Purpose, fix.Failure = "fix", []byte(`[]`)
	res2, err := w.runner.Run(ctx, fix, w.deps())
	if err != nil {
		t.Fatal(err)
	}
	if got := res2.Diff.Paths(); len(got) != 1 || got[0] != "hello.txt" {
		t.Fatalf("the fix's diff = %v: a stray file from the kept clone reached it", got)
	}
	if err := pool.Release(TicketKey(testTicket)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("released clone: %v", err)
	}
	if _, err := pool.Dir("../escape"); err == nil {
		t.Fatal("a key outside the pool")
	}
}
