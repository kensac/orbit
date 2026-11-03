package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// Synthetic transcripts only: never a real session.
const (
	cliSessA = "66666666-6666-4666-8666-666666666666"
	cliSessB = "77777777-7777-4777-8777-777777777777"
)

func cliLine(session, typ, uuid, text string) string {
	content := any(text)
	if typ == "assistant" {
		content = []any{map[string]any{"type": "text", "text": text}}
	}
	b, _ := json.Marshal(map[string]any{"type": typ, "sessionId": session, "uuid": uuid, "cwd": "/nowhere/widget",
		"timestamp": "2026-10-01T10:00:00Z", "message": map[string]any{"role": typ, "content": content}})
	return string(b) + "\n"
}

func runImport(t *testing.T, url string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := importSessions(context.Background(), append(args, "--api", url, "--token", "op-token"), &out); err != nil {
		t.Fatalf("import %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func TestImportClaudeSessions(t *testing.T) {
	s := testdb.New(t)
	srv := httptest.NewServer((&api.Server{Store: s, Token: "op-token", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).InternalHandler())
	defer srv.Close()

	dir := t.TempDir()
	proj := filepath.Join(dir, "-nowhere-widget")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	a := cliLine(cliSessA, "user", "u1", "Why is the sprocket slow?") + cliLine(cliSessA, "assistant", "a1", "The sprocket polls.")
	// B resumed A: it starts with A's lines under its own id.
	b := cliLine(cliSessB, "user", "u1", "Why is the sprocket slow?") + cliLine(cliSessB, "assistant", "a1", "The sprocket polls.") +
		cliLine(cliSessB, "user", "u2", "Make it push instead.")
	write := func(id, content string) {
		if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(cliSessA, a)
	write(cliSessB, b)

	if out := runImport(t, srv.URL, dir, "--print"); !strings.Contains(out, cliSessA) || !strings.Contains(out, "3 lines") {
		t.Fatalf("print = %s", out)
	}
	if out := runImport(t, srv.URL, dir); !strings.Contains(out, "2 to create") || !strings.Contains(out, "nothing was sent") {
		t.Fatalf("dry run = %s", out)
	}
	if p, _ := s.ListExternalSessions(context.Background(), store.SessionFilter{}); len(p.Sessions) != 0 {
		t.Fatalf("a dry run stored %d sessions", len(p.Sessions))
	}
	if out := runImport(t, srv.URL, dir, "--apply"); !strings.Contains(out, "2 created") || !strings.Contains(out, "5 lines added, 2 of them already stored") {
		t.Fatalf("apply = %s", out)
	}

	// A grows by a line and a half: only the complete line is sent.
	write(cliSessA, a+cliLine(cliSessA, "user", "u3", "Check the gear too.")+`{"type":"user","partial`)
	if out := runImport(t, srv.URL, dir, "--apply"); !strings.Contains(out, "0 created, 1 appended to") || !strings.Contains(out, "1 unchanged; 1 lines added") {
		t.Fatalf("append = %s", out)
	}
	// B is rewritten on disk: a new generation.
	write(cliSessB, cliLine(cliSessB, "user", "u9", "Start over."))
	if out := runImport(t, srv.URL, dir, "--apply"); !strings.Contains(out, "1 rewritten") {
		t.Fatalf("rewrite = %s", out)
	}
	if out := runImport(t, srv.URL, dir); !strings.Contains(out, "0 to create, 0 to append to, 0 to rewrite, 2 unchanged") {
		t.Fatalf("rerun = %s", out)
	}
	p, err := s.ListExternalSessions(context.Background(), store.SessionFilter{})
	if err != nil || len(p.Sessions) != 2 {
		t.Fatalf("sessions = %v %v", p, err)
	}
	for _, e := range p.Sessions {
		want := map[string]int{cliSessA: 3, cliSessB: 1}[e.SessionID]
		if e.Lines != want {
			t.Fatalf("%s has %d lines, want %d", e.SessionID, e.Lines, want)
		}
	}
}
