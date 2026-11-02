package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runHook(t *testing.T, d AgentHookDef, ev string) (int, map[string]any, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := AgentHook(strings.NewReader(ev), &out, &errOut, d, t.TempDir())
	var m map[string]any
	_ = json.Unmarshal(out.Bytes(), &m)
	return code, m, out.String() + errOut.String()
}

func TestAgentHookSayAndDeny(t *testing.T) {
	_, m, _ := runHook(t, AgentHookDef{Event: "UserPromptSubmit", Say: "Lead with the total."}, `{"hook_event_name":"UserPromptSubmit"}`)
	if m["hookSpecificOutput"].(map[string]any)["additionalContext"] != "Lead with the total." {
		t.Fatalf("say = %v", m)
	}
	_, m, _ = runHook(t, AgentHookDef{Event: "PreToolUse", Deny: "Not this tool."}, `{"hook_event_name":"PreToolUse","tool_name":"Grep"}`)
	if m["hookSpecificOutput"].(map[string]any)["permissionDecision"] != "deny" {
		t.Fatalf("deny = %v", m)
	}
	_, m, _ = runHook(t, AgentHookDef{Event: "Stop", Say: "Check the figures."}, `{"hook_event_name":"Stop","stop_hook_active":false}`)
	if m["decision"] != "block" || m["reason"] != "Check the figures." {
		t.Fatalf("stop = %v", m)
	}
	// Once only: the CLI marks a stop it already sent back.
	if _, m, _ = runHook(t, AgentHookDef{Event: "Stop", Say: "Check the figures."}, `{"hook_event_name":"Stop","stop_hook_active":true}`); len(m) != 0 {
		t.Fatalf("second stop = %v", m)
	}
	d, err := DecodeAgentHook(EncodeAgentHook(AgentHookDef{Event: "Stop", Say: "it's \"quoted\"; $(x)"}))
	if err != nil || d.Say != "it's \"quoted\"; $(x)" {
		t.Fatalf("round trip = %+v %v", d, err)
	}
}

func TestAgentHookRun(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "h.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\ncat > event.json\necho \"cwd=$(pwd) key=${SECRET_KEY:-none}\"\necho blocked >&2\nexit 2\n"), 0o700)
	t.Setenv("SECRET_KEY", "s3cret")
	var out, errOut bytes.Buffer
	root := t.TempDir()
	code := AgentHook(strings.NewReader(`{"hook_event_name":"PostToolUse"}`), &out, &errOut, AgentHookDef{Event: "PostToolUse", Run: []string{script}}, root)
	if code != 2 || !strings.Contains(errOut.String(), "blocked") {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	real, _ := filepath.EvalSymlinks(root)
	if !strings.Contains(out.String(), "key=none") || !strings.Contains(out.String(), "cwd="+real) {
		t.Fatalf("stdout = %q (the command must run in the workdir without orbit's environment)", out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(root, "event.json")); !strings.Contains(string(b), "PostToolUse") {
		t.Fatalf("event on stdin = %q", b)
	}
	slow := filepath.Join(dir, "slow.sh")
	_ = os.WriteFile(slow, []byte("#!/bin/sh\nsleep 5\n"), 0o700)
	errOut.Reset()
	if code := AgentHook(strings.NewReader(`{}`), &out, &errOut, AgentHookDef{Event: "Stop", Run: []string{slow}, Timeout: 100 * time.Millisecond}, root); code != 1 || !strings.Contains(errOut.String(), "timed out") {
		t.Fatalf("timeout: code %d %q", code, errOut.String())
	}
}
