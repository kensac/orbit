package engine_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
	"github.com/kensac/orbit/internal/workspace"
)

// fakeSandbox is sandboxd on this host: a sandbox is a directory whose
// work/ stands for /work, and an attach runs the argv here with /work
// rewritten to it and "claude" to a fake CLI.
type fakeSandbox struct {
	t       *testing.T
	cli     string
	mu      sync.Mutex
	roots   map[string]string
	created int
	profile string
	gone    int
}

func (f *fakeSandbox) CreateProfile(_ context.Context, _, profile string, archive io.Reader) (sandbox.CreateResponse, error) {
	f.profile = profile
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	id := strings.Repeat(string(rune('a'+f.created)), 64)
	root := f.t.TempDir()
	if _, err := workspace.Unpack(archive, workspace.Into(filepath.Join(root, "work"))); err != nil {
		return sandbox.CreateResponse{}, err
	}
	f.roots[id] = root
	return sandbox.CreateResponse{ID: id, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (f *fakeSandbox) root(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.roots[id]
}

func (f *fakeSandbox) Attach(ctx context.Context, id string, ar sandbox.AttachRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	root := f.root(id)
	argv := make([]string, len(ar.Argv))
	for i, a := range ar.Argv {
		argv[i] = strings.ReplaceAll(a, sandbox.WorkDir, filepath.Join(root, "work"))
		if a == "claude" {
			argv[i] = f.cli
		}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = filepath.Join(root, "work")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TOKEN_SEEN=" + ar.Env["CLAUDE_CODE_OAUTH_TOKEN"]}
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

func (f *fakeSandbox) Archive(_ context.Context, id string) (io.ReadCloser, error) {
	tree, _, err := workspace.Pack(workspace.Tree{Dir: filepath.Join(f.root(id), "work")})
	return io.NopCloser(bytes.NewReader(tree)), err
}

func (f *fakeSandbox) Destroy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.roots[id]; ok {
		f.gone++
		delete(f.roots, id)
	}
	return nil
}

// sandboxCLI is a fake live CLI: each turn writes a file into its cwd and a
// transcript line under its config directory, and replies with what is in
// uploads/.
const sandboxCLI = `#!/bin/sh
n=0
while IFS= read -r line; do
  n=$((n+1))
  mkdir -p "$CLAUDE_CONFIG_DIR/projects/-work"
  echo '{"type":"user"}' >> "$CLAUDE_CONFIG_DIR/projects/-work/sb-1.jsonl"
  echo "turn $n" > "made-$n.txt"
  echo "$n" >> "$HOME/.kept"
  up=$(cat uploads/* 2>/dev/null | tr '\n' ' ')
  echo '{"type":"system","subtype":"init","session_id":"sb-1"}'
  echo '{"type":"result","subtype":"success","is_error":false,"result":"reply '$n' saw '"$up"'token '"$TOKEN_SEEN"'","total_cost_usd":0.01,"session_id":"sb-1"}'
done
`

func TestASandboxedChatKeepsItsWorkspace(t *testing.T) {
	for _, profile := range spec.SandboxProfiles {
		t.Run(profile, func(t *testing.T) { sandboxedChat(t, profile) })
	}
}

func sandboxedChat(t *testing.T, profile string) {
	s := testdb.New(t)
	dir := t.TempDir()
	cli := filepath.Join(dir, "claude")
	if err := os.WriteFile(cli, []byte(sandboxCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(`
defaults: {runner: cc, channels: [log]}
runners: {cc: {kind: claude-code, binary: /nonexistent}}
channels: {log: {kind: log}}
sandbox: {sandboxd: "unix:/tmp/none.sock"}
`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "helper.yaml"), []byte(`
name: helper
charter: Help.
chat: true
sandbox: `+profile+`
wake_on_message: true
tools: [Bash, Read, Write]
session: {resume: true, idle: 1h, live: 5m}
`), 0o644)
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeSandbox{t: t, cli: cli, roots: map[string]string{}}
	env := func(k string) (string, bool) {
		v, ok := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-1", "ORBIT_SANDBOX_TOKEN": "sb"}[k]
		return v, ok
	}
	e := &engine.Engine{Store: s, DataDir: t.TempDir(), ConfigDir: dir, Env: env, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewRunner: runner.New, Live: runner.NewLive(2),
		NewSandbox: func(string, string) (runner.SandboxClient, error) { return fs, nil }}
	defer e.Live.Close()
	if err := e.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	_ = cfg
	a := e.Current().Config.Agents["helper"]
	var conv string
	if err := s.Pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE agent = 'helper' AND is_default`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	send := func(body, file string) *store.ThreadRun {
		t.Helper()
		u, err := s.CreateUpload(ctx, store.NewUpload{ConversationID: conv, Name: "note.txt", MediaType: "text/plain", Content: []byte(file)})
		if err != nil {
			t.Fatal(err)
		}
		r, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "helper", SpecSHA: a.SHA, Body: body, Wake: true, Attachments: []string{u.ID}})
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
	r1 := send("hi", "alpha\n")
	if r1.Status != "succeeded" || r1.Report == nil || *r1.Report != "reply 1 saw alpha token tok-1" {
		t.Fatalf("run 1 = %s %v %v", r1.Status, deref(r1.Report), deref(r1.Error))
	}
	r2 := send("again", "beta\n")
	if r2.Status != "succeeded" || deref(r2.Report) != "reply 2 saw beta alpha token tok-1" {
		t.Fatalf("run 2 = %s %q %q: the live sandbox gets the new upload", r2.Status, deref(r2.Report), deref(r2.Error))
	}
	if fs.created != 1 {
		t.Fatalf("sandboxes created = %d, want one live sandbox for both messages", fs.created)
	}
	ws, err := s.LatestWorkspace(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := workspace.List(ws.Archive, nil)
	var names []string
	for _, f := range files {
		names = append(names, f.Path)
	}
	if got := strings.Join(names, ","); got != "made-1.txt,made-2.txt,uploads/note (2).txt,uploads/note.txt" {
		t.Fatalf("workspace = %s", got)
	}
	// The session's transcript came back to HOME, where resume looks.
	home := filepath.Join(e.DataDir, "conversations", conv, "home")
	work := filepath.Join(e.DataDir, "conversations", conv, "work")
	if p, _ := runner.SessionFile(home, work, "sb-1"); !exists(p) {
		t.Fatalf("transcript not at %s", p)
	}
	if fs.profile != profile {
		t.Fatalf("profile = %q", fs.profile)
	}
	if kept, err := os.ReadFile(filepath.Join(e.DataDir, "conversations", conv, "box-home", ".kept")); profile == spec.SandboxWorkstation && (err != nil || string(kept) != "1\n2\n") {
		t.Fatalf("a workstation's home is kept: %q %v", kept, err)
	}

	// The person changes the workspace between turns: the next turn is
	// told by code, and runs in a new sandbox made from the person's version
	// (the live one's copy is stale), resuming the session.
	tree, err := s.ConversationWorkspace(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := s.LatestWorkspace(ctx, conv)
	snapNow, err := workspace.Open(base.Archive, nil)
	if err != nil {
		t.Fatal(err)
	}
	ed := snapNow.Edit(time.Now())
	_ = ed.PutFile("uploads/person.txt", []byte("gamma\n"))
	_ = ed.Remove("made-1.txt")
	archive, n, err := ed.Archive()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPersonWrite(ctx, store.PersonWrite{WorkspaceID: tree.ID, ConversationID: conv, BaseID: base.ID,
		Archive: archive, Files: n, Person: "kanishk"}); err != nil {
		t.Fatal(err)
	}
	r3 := send("third", "delta\n")
	if got := deref(r3.Report); r3.Status != "succeeded" || got != "reply 1 saw beta delta alpha gamma token tok-1" {
		t.Fatalf("run 3 = %s %q %q: a new sandbox from the person's version", r3.Status, got, deref(r3.Error))
	}
	if fs.created != 2 {
		t.Fatalf("sandboxes created = %d, want a new one after the person's change", fs.created)
	}
	var prompt string
	if err := s.Pool.QueryRow(ctx, `SELECT convert_from(content, 'UTF8') FROM artifacts WHERE run_id = $1 AND name = 'prompt.md'`, r3.ID).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "- added uploads/person.txt\n") || !strings.Contains(prompt, "- removed made-1.txt (it is in the workspace's trash)\n") {
		t.Fatalf("prompt = %q", prompt)
	}
	if latest, _, _ := s.PersonChanges(ctx, conv); latest != nil {
		t.Fatal("after the turn, the agent has seen the person's version")
	}
	e.Live.Close()
	if fs.gone != 2 {
		t.Fatalf("sandboxes destroyed = %d", fs.gone)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestStoppingARunningRun(t *testing.T) {
	fr := &fakeRunner{output: goodOutput, block: make(chan struct{}), started: make(chan struct{})}
	e, s, h := harness(t, fr)
	c := claim(t, e, s)
	done := make(chan error, 1)
	go func() { done <- e.Execute(ctx, c) }()
	<-fr.started
	if st, err := s.CancelRun(ctx, c.ID, "kanishk"); err != nil || st != "cancelling" {
		t.Fatalf("cancel = %q %v", st, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not stop")
	}
	run, err := s.GetRun(ctx, c.ID)
	if err != nil || run.Status != "cancelled" {
		t.Fatalf("run = %+v %v", run, err)
	}
	if len(h.messages) != 0 {
		t.Fatalf("a stopped run notified: %v", h.messages)
	}
	if _, err := s.CancelRun(ctx, c.ID, "kanishk"); err == nil {
		t.Fatal("a finished run cancelled again")
	}
}

// actRunner is a chat backend that calls orbit's act tool once and replies
// with what the tool said.
type actRunner struct{ args string }

func (actRunner) Kind() string  { return "claude-code" }
func (actRunner) Model() string { return "fake" }
func (f actRunner) Run(ctx context.Context, req runner.Request, _ func(runner.Event)) (*runner.Result, error) {
	if req.Chat == nil {
		return nil, errors.New("not a chat run")
	}
	text, isErr := req.Chat.Call(ctx, "act", []byte(f.args))
	if isErr {
		text = "error: " + text
	}
	return &runner.Result{Reply: text, SessionID: "s1"}, nil
}

func TestThePersonsMessageIsTheInstruction(t *testing.T) {
	s := testdb.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte("defaults: {runner: cc, channels: [log]}\nrunners: {cc: {kind: claude-code}}\nchannels: {log: {kind: log}}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "helper.yaml"), []byte("name: helper\ncharter: Help.\nchat: true\nwake_on_message: true\nactions: {agent:define: approve}\n"), 0o644)
	args := `{"type": "agent:define", "reason": "asked", "params": {"spec": {"name": "news", "charter": "News."}}}`
	e := &engine.Engine{Store: s, DataDir: t.TempDir(), ConfigDir: dir, Outbox: true, Env: func(string) (string, bool) { return "", false },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) {
			return actRunner{args}, nil
		}}
	if err := e.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	a := e.Current().Config.Agents["helper"]
	run := func(wake func() string) *store.ThreadRun {
		t.Helper()
		id := wake()
		c, err := s.ClaimRun(ctx, "test")
		if err != nil || c == nil {
			t.Fatalf("claim: %v %v", c, err)
		}
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		r, err := s.ThreadRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Unattended: the approve rule holds.
	manual := run(func() string {
		id, err := s.EnqueueRun(ctx, store.NewRun{Agent: "helper", SpecSHA: a.SHA, Trigger: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	})
	if !strings.Contains(deref(manual.Report), "waits for the person's approval") || len(manual.Actions) != 1 || manual.Actions[0].Status != "pending" {
		t.Fatalf("manual = %q %+v", deref(manual.Report), manual.Actions)
	}
	// The person asked: it executes in the turn, approved by them.
	asked := run(func() string {
		r, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "helper", SpecSHA: a.SHA, Body: "make a news agent", Author: "kanishk", Wake: true})
		if err != nil {
			t.Fatal(err)
		}
		return r.RunID
	})
	if !strings.Contains(deref(asked.Report), "agent:define executed") || len(asked.Actions) != 1 || asked.Actions[0].Status != "executed" {
		t.Fatalf("asked = %q %+v", deref(asked.Report), asked.Actions)
	}
	act, err := s.GetAction(ctx, asked.Actions[0].ID)
	if err != nil || act.DecidedBy == nil || *act.DecidedBy != "kanishk" || act.InstructedBy == nil || *act.InstructedBy != "kanishk" {
		t.Fatalf("action = %+v %v", act, err)
	}
	if e.Current().Config.Agents["news"] == nil {
		t.Fatal("the defined agent is not in force")
	}
}

func TestPagePublishReturnsTheConversationPageReferenceOnlyForPersonMessage(t *testing.T) {
	s := testdb.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte("defaults: {runner: cc, channels: [log]}\nrunners: {cc: {kind: claude-code}}\nchannels: {log: {kind: log}}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "chat.yaml"), []byte("name: chat\ncharter: Help.\nchat: true\nwake_on_message: true\nactions: {page:publish: approve}\n"), 0o644)
	args := `{"type":"page:publish","reason":"requested","params":{"title":"Example","html":"<main>hello</main>"}}`
	e := &engine.Engine{Store: s, DataDir: t.TempDir(), ConfigDir: dir, Env: func(string) (string, bool) { return "", false },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) {
			return actRunner{args}, nil
		}}
	if err := e.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	a := e.Current().Config.Agents["chat"]
	wake := func(trigger string) *store.ThreadRun {
		t.Helper()
		var id string
		if trigger == "message" {
			r, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "chat", SpecSHA: a.SHA, Body: "make a page", Author: "kanishk", Wake: true})
			if err != nil {
				t.Fatal(err)
			}
			id = r.RunID
		} else {
			var err error
			id, err = s.EnqueueRun(ctx, store.NewRun{Agent: "chat", SpecSHA: a.SHA, Trigger: trigger})
			if err != nil {
				t.Fatal(err)
			}
		}
		c, err := s.ClaimRun(ctx, "test")
		if err != nil || c == nil {
			t.Fatalf("claim: %v %v", c, err)
		}
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		r, err := s.ThreadRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	asked := wake("message")
	if asked.Report == nil || len(asked.Actions) != 1 || *asked.Report != `{"page_id":"`+asked.Actions[0].ID+`","title":"Example"}` || asked.Actions[0].Status != "executed" {
		t.Fatalf("person message result = %q actions=%+v", deref(asked.Report), asked.Actions)
	}
	manual := wake("manual")
	if manual.Report == nil || !strings.Contains(*manual.Report, "only available while answering") || len(manual.Actions) != 0 {
		t.Fatalf("manual page publish = %q actions=%+v", deref(manual.Report), manual.Actions)
	}
}
