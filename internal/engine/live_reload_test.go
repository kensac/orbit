package engine_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestLiveChatProcessSurvivesConfigReloadWhenInvocationIsUnchanged(t *testing.T) {
	s := testdb.New(t)
	dir := t.TempDir()
	cli := filepath.Join(dir, "claude")
	if err := os.WriteFile(cli, []byte(sandboxCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig := func(timezone string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(`
timezone: `+timezone+`
defaults: {runner: cc, channels: [log]}
runners: {cc: {kind: claude-code, binary: /unused}}
channels: {log: {kind: log}}
sandbox: {sandboxd: "unix:/tmp/unused.sock"}
`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("UTC")
	if err := os.WriteFile(filepath.Join(dir, "agents/helper.yaml"), []byte(`
name: helper
charter: Help.
chat: true
sandbox: workstation
wake_on_message: true
tools: [Bash, Read, Write]
session: {resume: true, idle: 24h, live: 24h}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	client := &fakeSandbox{t: t, cli: cli, roots: map[string]string{}}
	env := func(k string) (string, bool) {
		v, ok := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-1", "ORBIT_SANDBOX_TOKEN": "sb"}[k]
		return v, ok
	}
	e := &engine.Engine{
		Store: s, DataDir: t.TempDir(), ConfigDir: dir, Env: env,
		Log:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		NewRunner: runner.New, Live: runner.NewLiveWithPressure(2, nil, 0),
		NewSandbox: func(string, string) (runner.SandboxClient, error) { return client, nil },
	}
	defer e.Live.Close()
	if err := e.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	var conv string
	if err := s.Pool.QueryRow(ctx, "SELECT id::text FROM conversations WHERE agent = 'helper' AND is_default").Scan(&conv); err != nil {
		t.Fatal(err)
	}
	send := func(body, file string) *store.ThreadRun {
		t.Helper()
		specSHA := e.Current().Config.Agents["helper"].SHA
		u, err := s.CreateUpload(ctx, store.NewUpload{ConversationID: conv, Name: "note.txt", MediaType: "text/plain", Content: []byte(file)})
		if err != nil {
			t.Fatal(err)
		}
		r, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "helper", SpecSHA: specSHA, Body: body, Wake: true, Attachments: []string{u.ID}})
		if err != nil {
			t.Fatal(err)
		}
		c, err := s.ClaimRun(ctx, "test")
		if err != nil || c == nil {
			t.Fatalf("claim: %v %v", c, err)
		}
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		run, err := s.ThreadRunByID(ctx, r.RunID)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	first := send("first", "alpha\n")
	if first.Status != "succeeded" || deref(first.Report) != "reply 1 saw alpha token tok-1" || e.Live.Len() != 1 {
		t.Fatalf("first = %s %q, live=%d", first.Status, deref(first.Report), e.Live.Len())
	}

	writeConfig("America/Los_Angeles")
	if err := e.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if e.Live.Len() != 1 {
		t.Fatalf("config reload closed an unchanged invocation; live=%d", e.Live.Len())
	}
	second := send("second", "beta\n")
	if second.Status != "succeeded" || deref(second.Report) != "reply 2 saw beta alpha token tok-1" {
		t.Fatalf("second = %s %q %q", second.Status, deref(second.Report), deref(second.Error))
	}
	if client.created != 1 || client.gone != 0 {
		t.Fatalf("box created=%d destroyed=%d; unchanged process should keep its box", client.created, client.gone)
	}
}
