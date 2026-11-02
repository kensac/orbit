package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

func TestAttachmentsAreInTheWorkdir(t *testing.T) {
	var got []byte
	fr := &fakeRunner{output: goodOutput}
	fr.seen = func(req runner.Request) { got, _ = os.ReadFile(filepath.Join(req.Workdir, "uploads", "receipt.txt")) }
	e, s, _ := harness(t, fr)
	var conv string
	if err := s.Pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE agent = 'finance' AND is_default`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUpload(ctx, store.NewUpload{ConversationID: conv, Name: "receipt.txt", MediaType: "text/plain", Content: []byte("total 12.50")})
	if err != nil {
		t.Fatal(err)
	}
	a := e.Current().Config.Agents["finance"]
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: a.SHA, Body: "what is this?", Wake: true,
		Attachments: []string{u.ID}}); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if string(got) != "total 12.50" {
		t.Fatalf("uploads/receipt.txt = %q", got)
	}
	if !strings.Contains(fr.got.Prompt, "attached: uploads/receipt.txt (text/plain, 11 bytes)") {
		t.Fatalf("prompt does not list the attachment:\n%s", fr.got.Prompt)
	}
}
