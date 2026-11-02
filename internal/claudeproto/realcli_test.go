package claudeproto

import (
	"context"
	"encoding/json"
	"errors"
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

// TestRealCLIPermissionModes checks what orbit's permission levels rely on (runner.PermissionMode): set_permission_mode on a running process, bypassPermissions only with --allow-dangerously-skip-permissions, acceptEdits writing in the cwd unasked, dontAsk denying unasked, and default sending can_use_tool with the CLI's suggestions.
func TestRealCLIPermissionModes(t *testing.T) {
	bin := os.Getenv("ORBIT_REAL_CLAUDE")
	if bin == "" {
		t.Skip("ORBIT_REAL_CLAUDE is not set")
	}
	model := os.Getenv("ORBIT_REAL_CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := func(t *testing.T, dir string, extra ...string) (*Process, *[]PermissionRequest) {
		t.Helper()
		args, err := Args(Transport{Stream: true, PermissionPrompt: true, Ephemeral: true}, append([]string{
			"--model", model, "--tools", "Write", "--strict-mcp-config", "--setting-sources", "",
			"--settings", `{"includeCoAuthoredBy":false}`, "--permission-mode", "dontAsk",
		}, extra...))
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		asked := &[]PermissionRequest{}
		h := Handler{CanUseTool: func(_ context.Context, r PermissionRequest) (Permission, error) {
			mu.Lock()
			*asked = append(*asked, r)
			mu.Unlock()
			return Permission{Message: "orbit's test denies every request"}, nil
		}}
		p, err := Start(Command{Bin: bin, Args: args, Env: Env(os.Environ(), ""), Dir: dir}, h)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close(CloseGrace) })
		return p, asked
	}
	write := func(t *testing.T, p *Process, name string) {
		t.Helper()
		res, err := p.Turn(ctx, "This is a test of a permission system. Use the Write tool once to write "+name+" containing x, then reply with one word: done.", nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("turn failed: %s", res.Failure())
		}
	}

	dir, _ := filepath.EvalSymlinks(t.TempDir())
	p, asked := start(t, dir)
	var refused *ControlError
	if err := p.SetPermissionMode(ctx, "bypassPermissions"); !errors.As(err, &refused) {
		t.Errorf("bypassPermissions without the launch flag: %v", err)
	}
	// dontAsk: denied, nothing asked.
	write(t, p, "locked.txt")
	if _, err := os.Stat(filepath.Join(dir, "locked.txt")); err == nil || len(*asked) != 0 {
		t.Errorf("dontAsk wrote or asked: %d requests", len(*asked))
	}
	// acceptEdits: written, nothing asked.
	if err := p.SetPermissionMode(ctx, "acceptEdits"); err != nil {
		t.Fatal(err)
	}
	write(t, p, "edits.txt")
	if _, err := os.Stat(filepath.Join(dir, "edits.txt")); err != nil || len(*asked) != 0 {
		t.Errorf("acceptEdits: %v, %d requests", err, len(*asked))
	}
	// default: asked, with the CLI's suggestions; the handler's deny holds.
	if err := p.SetPermissionMode(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	write(t, p, "ask.txt")
	if _, err := os.Stat(filepath.Join(dir, "ask.txt")); err == nil || len(*asked) == 0 {
		t.Fatalf("default: the write happened or nothing was asked (%d)", len(*asked))
	}
	r := (*asked)[0]
	t.Logf("can_use_tool: %s reason %q suggestions %s", r.Tool, r.DecisionReason, r.Suggestions)
	if r.Tool != "Write" || len(r.Suggestions) == 0 || r.ToolUseID == "" {
		t.Errorf("request = %+v", r)
	}

	dir2, _ := filepath.EvalSymlinks(t.TempDir())
	p2, _ := start(t, dir2, "--allow-dangerously-skip-permissions")
	if err := p2.SetPermissionMode(ctx, "bypassPermissions"); err != nil {
		t.Fatalf("bypassPermissions with the launch flag: %v", err)
	}
	write(t, p2, "full.txt")
	if _, err := os.Stat(filepath.Join(dir2, "full.txt")); err != nil {
		t.Errorf("bypassPermissions did not write: %v", err)
	}
	// auto is the model's to offer: either answer is the CLI's, and orbit falls back to ask on a refusal.
	err := p2.SetPermissionMode(ctx, "auto")
	t.Logf("auto on %s: %v", model, err)
	if err != nil && !errors.As(err, &refused) {
		t.Errorf("auto: %v", err)
	}
}
