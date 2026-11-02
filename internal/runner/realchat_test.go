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

// TestRealCLIChatPermissionLevels moves one live chat process between levels with set_permission_mode (ORBIT_REAL_CLAUDE=<wrapper> go test -run RealCLIChatPermissionLevels ./internal/runner/): at locked a Read outside the workspace is denied without asking; the next run, at ask, is taken by the same process, and the same Read comes to orbit's prompt.
func TestRealCLIChatPermissionLevels(t *testing.T) {
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
	work, outside := filepath.Join(dir, "work"), filepath.Join(dir, "outside")
	_ = os.MkdirAll(work, 0o700)
	_ = os.MkdirAll(outside, 0o700)
	secret := filepath.Join(outside, "word.txt")
	_ = os.WriteFile(secret, []byte("the word is marigold\n"), 0o600)

	live := NewLive(2)
	defer live.Close()
	var modes []string
	sess := func(resume string) *Session {
		return &Session{Home: filepath.Join(dir, "home"), KeepDays: 1, Live: live, Key: "c1", Open: true, LiveIdle: time.Minute,
			Resume: resume, Started: func(mode, _ string) error { modes = append(modes, mode); return nil }}
	}
	var mu sync.Mutex
	var asked []string
	chat := func(level string) *Chat {
		return &Chat{Level: level, Ceiling: spec.LevelEdits, Choice: LevelChoice{Source: "conversation"},
			Prompt: func(_ context.Context, req claudeproto.PermissionRequest) claudeproto.Permission {
				mu.Lock()
				asked = append(asked, req.Tool)
				mu.Unlock()
				return claudeproto.Permission{Allow: true}
			}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	evs := &events{}
	prompt := "This is a test of a permission system. Use the Read tool once on " + secret + " and reply with the word it names, or with the single word denied if you could not read it."
	res, err := r.Run(ctx, Request{Workdir: work, System: "Answer briefly.", Session: sess(""), Chat: chat(spec.LevelLocked), Prompt: prompt}, evs.emit)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if strings.Contains(strings.ToLower(res.Reply), "marigold") || len(asked) != 0 {
		t.Fatalf("locked: reply %q, asked %v", res.Reply, asked)
	}
	res2, err := r.Run(ctx, Request{Workdir: work, System: "Answer briefly.", Session: sess(res.SessionID), Chat: chat(spec.LevelAsk), Prompt: prompt}, evs.emit)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if len(modes) != 2 || modes[1] != "live" {
		t.Fatalf("modes = %v, want the second run in the same process", modes)
	}
	if !strings.Contains(strings.ToLower(res2.Reply), "marigold") || len(asked) == 0 || asked[0] != "Read" {
		t.Fatalf("ask: reply %q, asked %v", res2.Reply, asked)
	}
	t.Logf("levels %v; asked %v", evs.of("orbit.permissions"), asked)
}
