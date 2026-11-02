package claudeproto

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRealCLIProtocol runs the real Claude Code CLI against this package: ORBIT_REAL_CLAUDE=claude go test -run RealCLI ./internal/claudeproto/. It uses the CLI's own login (HOME is the person's, so a keychain login works) and never runs in CI. It checks what the fakes cannot: initialize with a host hook, set_permission_mode, can_use_tool answered both ways, a hook_callback per call, and an interrupt that withdraws a pending permission request and ends the turn.
func TestRealCLIProtocol(t *testing.T) {
	bin := os.Getenv("ORBIT_REAL_CLAUDE")
	if bin == "" {
		t.Skip("ORBIT_REAL_CLAUDE is not set")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := os.Getenv("ORBIT_REAL_CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	args, err := Args(Transport{Stream: true, PermissionPrompt: true, Ephemeral: true}, []string{
		"--model", model, "--tools", "Write", "--strict-mcp-config", "--setting-sources", "",
		"--settings", `{"permissions":{"ask":["Write"]},"includeCoAuthoredBy":false}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var decided, hooked []string
	pending := make(chan struct{}, 1)
	withdrawn := make(chan struct{}, 1)
	decide := func(ctx context.Context, r PermissionRequest) Permission {
		var in struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(r.Input, &in)
		name := filepath.Base(in.FilePath)
		if name == "slow.txt" {
			pending <- struct{}{}
			<-ctx.Done()
			withdrawn <- struct{}{}
			return Permission{Message: "withdrawn"}
		}
		return Permission{Allow: name == "yes.txt", Message: "orbit refuses " + name}
	}
	record := func(r PermissionRequest, p Permission) error {
		mu.Lock()
		defer mu.Unlock()
		var in struct {
			FilePath string `json:"file_path"`
		}
		_ = json.Unmarshal(r.Input, &in)
		decided = append(decided, r.Tool+" "+filepath.Base(in.FilePath)+" "+map[bool]string{true: "allow", false: "deny"}[p.Allow])
		return nil
	}
	h := Handler{
		CanUseTool: Recorded(decide, record),
		Hook: func(_ context.Context, r HookCallback) (json.RawMessage, error) {
			mu.Lock()
			defer mu.Unlock()
			hooked = append(hooked, r.CallbackID)
			return json.RawMessage(`{}`), nil
		},
	}
	p, err := Start(Command{Bin: bin, Args: args, Env: Env(os.Environ(), ""), Dir: dir}, h)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(CloseGrace)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	init, err := p.Initialize(ctx, Init{Hooks: map[string][]HookMatcher{
		"PreToolUse": {{Matcher: "Write", CallbackIDs: []string{"hook_write"}, Timeout: 10}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var desc struct {
		Models       []json.RawMessage `json:"models"`
		HooksApplied *bool             `json:"hooks_applied"`
	}
	if json.Unmarshal(init, &desc) != nil || len(desc.Models) == 0 {
		t.Fatalf("initialize answered %s", init)
	}
	t.Logf("initialize: %d models, hooks_applied %v", len(desc.Models), desc.HooksApplied != nil && *desc.HooksApplied)
	if err := p.SetPermissionMode(ctx, "default"); err != nil {
		t.Fatal(err)
	}

	var kinds []string
	emit := func(l Line) { kinds = append(kinds, l.Kind) }
	res, err := p.Turn(ctx, "This is a test of a permission system. Use the Write tool twice, one call each, and continue after a refusal: "+
		"first write no.txt containing x, then write yes.txt containing hi. Then reply with one word: done.", emit)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("turn 1: %s %s; decisions %v; hooks %v", res.Subtype, res.Result, decided, hooked)
	if res.IsError {
		t.Fatalf("turn 1 failed: %s", res.Failure())
	}
	if b, err := os.ReadFile(filepath.Join(dir, "yes.txt")); err != nil || !strings.Contains(string(b), "hi") {
		t.Errorf("the allowed Write did not happen: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "no.txt")); err == nil {
		t.Error("the denied Write happened")
	}
	mu.Lock()
	joined := strings.Join(decided, ",")
	nHooks := len(hooked)
	mu.Unlock()
	if !strings.Contains(joined, "Write no.txt deny") || !strings.Contains(joined, "Write yes.txt allow") {
		t.Errorf("decisions %s", joined)
	}
	if nHooks < 2 {
		t.Errorf("the PreToolUse hook fired %d times, want one per Write", nHooks)
	}

	// An interrupt while a permission request waits withdraws it and ends the turn.
	if err := p.Send("Use the Write tool to write slow.txt containing x. Nothing else."); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pending:
	case <-ctx.Done():
		t.Fatal("no permission request for slow.txt")
	}
	if err := p.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	res, err = p.Await(ctx, emit)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("turn 2 after interrupt: %s %s", res.Subtype, res.Result)
	select {
	case <-withdrawn:
	case <-time.After(10 * time.Second):
		t.Error("the CLI did not withdraw the pending request")
	}
	if !strings.Contains(strings.Join(kinds, ","), "control_cancel_request") {
		t.Errorf("no control_cancel_request in %v", kinds)
	}
	if _, err := os.Stat(filepath.Join(dir, "slow.txt")); err == nil {
		t.Error("the withdrawn Write happened")
	}
}
