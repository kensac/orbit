package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/spec"
)

// TestRealCLIChat runs a chat turn against the real CLI:
// ORBIT_REAL_CLAUDE=<wrapper> go test -run RealCLIChat ./internal/runner/.
// The wrapper restores the person's HOME for a keychain login
// (docs/agents/testing.md). It checks what the fakes cannot: orbit's SDK
// MCP tools are listed and called over the control channel, the reply is the
// final message, a second turn lands in the same live process, and an
// interrupt ends a turn with a result.
func TestRealCLIChat(t *testing.T) {
	bin := os.Getenv("ORBIT_REAL_CLAUDE")
	if bin == "" {
		t.Skip("ORBIT_REAL_CLAUDE is not set")
	}
	model := os.Getenv("ORBIT_REAL_CLAUDE_MODEL")
	if model == "" {
		model = "haiku"
	}
	r, err := newClaudeCode(spec.RunnerSpec{Kind: "claude-code", Binary: bin, Model: model},
		func(n string) (string, bool) {
			return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "placeholder"}[n], n == "CLAUDE_CODE_OAUTH_TOKEN"
		})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	work := filepath.Join(dir, "work")
	_ = os.MkdirAll(work, 0o700)
	_ = os.WriteFile(filepath.Join(work, "notes.txt"), []byte("the secret word is heliotrope\n"), 0o600)

	var mu sync.Mutex
	var remembered []string
	chat := func() *Chat {
		return &Chat{
			Tools: []ChatTool{{Name: "remember", Description: "Remember a fact.",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{"content": map[string]any{"type": "string"}}, "required": []string{"content"}}}},
			Call: func(_ context.Context, name string, args json.RawMessage) (string, bool) {
				var v struct{ Content string }
				_ = json.Unmarshal(args, &v)
				mu.Lock()
				remembered = append(remembered, v.Content)
				mu.Unlock()
				return "remembered", false
			},
		}
	}
	live := NewLive(2)
	defer live.Close()
	var modes []string
	sess := func(resume string) *Session {
		return &Session{Home: filepath.Join(dir, "home"), KeepDays: 1, Live: live, Key: "c1", Open: true, LiveIdle: time.Minute,
			Resume: resume, Started: func(mode, _ string) error { modes = append(modes, mode); return nil }}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var events []string
	emit := func(e Event) { events = append(events, e.Kind) }

	res, err := r.Run(ctx, Request{Workdir: work, System: "Answer briefly.", Tools: nil, Session: sess(""), Chat: chat(),
		Prompt: "Read notes.txt, call the remember tool with the secret word as content, then reply with just the word."}, emit)
	if err != nil {
		t.Fatalf("turn 1: %v (events %v)", err, events)
	}
	if !strings.Contains(strings.ToLower(res.Reply), "heliotrope") || len(remembered) != 1 || !strings.Contains(remembered[0], "heliotrope") {
		t.Fatalf("turn 1: reply %q, remembered %q", res.Reply, remembered)
	}
	res2, err := r.Run(ctx, Request{Workdir: work, System: "Answer briefly.", Session: sess(res.SessionID), Chat: chat(),
		Prompt: "What word did you just remember? Reply with just the word, without reading any file."}, emit)
	if err != nil || !strings.Contains(strings.ToLower(res2.Reply), "heliotrope") {
		t.Fatalf("turn 2: %v %+v", err, res2)
	}
	if len(modes) != 2 || modes[1] != "live" {
		t.Fatalf("modes = %v, want the second turn live", modes)
	}

	stop := make(chan struct{})
	c3 := chat()
	c3.Interrupt = stop
	time.AfterFunc(4*time.Second, func() { close(stop) })
	res3, err := r.Run(ctx, Request{Workdir: work, System: "Answer briefly.", Session: sess(res2.SessionID), Chat: c3,
		Prompt: "Write a 3000-word essay about the history of clocks."}, emit)
	if err != nil || !res3.Interrupted {
		t.Fatalf("turn 3: %v %+v, want interrupted", err, res3)
	}
	t.Logf("modes %v; interrupted reply %d bytes", modes, len(res3.Reply))
}

// TestRealCLIChatSteer steers a chat turn against the real CLI (as TestRealCLIChat): a message sent while the turn's tool call runs is written into the turn, the CLI starts it there (Steered), and the one reply answers both.
func TestRealCLIChatSteer(t *testing.T) {
	bin := os.Getenv("ORBIT_REAL_CLAUDE")
	if bin == "" {
		t.Skip("ORBIT_REAL_CLAUDE is not set")
	}
	model := os.Getenv("ORBIT_REAL_CLAUDE_MODEL")
	if model == "" {
		model = "haiku"
	}
	r, err := newClaudeCode(spec.RunnerSpec{Kind: "claude-code", Binary: bin, Model: model},
		func(n string) (string, bool) {
			return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "placeholder"}[n], n == "CLAUDE_CODE_OAUTH_TOKEN"
		})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	work := filepath.Join(dir, "work")
	_ = os.MkdirAll(work, 0o700)
	steer := make(chan SteerMessage, 1)
	var mu sync.Mutex
	var steered []string
	id := claudeproto.NewUUID()
	sent := false
	emit := func(e Event) {
		if !sent && e.Kind == "assistant" && strings.Contains(string(e.Payload), `"tool_use"`) {
			sent = true
			steer <- SteerMessage{ID: id, Text: "Also say the word banana in your reply."}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	res, err := r.Run(ctx, Request{Workdir: work, System: "Answer briefly.", Tools: []string{"Bash"},
		Session: &Session{Home: filepath.Join(dir, "home"), KeepDays: 1},
		Chat: &Chat{Steer: steer, Steered: func(s string) { mu.Lock(); steered = append(steered, s); mu.Unlock() }},
		Prompt: "Run the shell command `sleep 5; echo one` with the Bash tool, then reply with exactly what it printed."}, emit)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reply %q, steered %v", res.Reply, steered)
	if len(steered) != 1 || steered[0] != id || !strings.Contains(strings.ToLower(res.Reply), "banana") {
		t.Fatalf("reply %q, steered %v: want the steer taken and answered", res.Reply, steered)
	}
}
