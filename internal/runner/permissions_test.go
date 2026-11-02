package runner

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/spec"
)

func TestPermissionModes(t *testing.T) {
	for level, mode := range map[string]string{
		spec.LevelLocked: "dontAsk", spec.LevelAsk: "default", spec.LevelEdits: "acceptEdits",
		spec.LevelAuto: "auto", spec.LevelFull: "bypassPermissions", "sudo": "dontAsk", "": "dontAsk",
	} {
		if got := PermissionMode(level); got != mode {
			t.Errorf("PermissionMode(%q) = %q, want %q", level, got, mode)
		}
	}
	if startMode(spec.LevelAuto) != "default" {
		t.Error("auto must start as ask: a model without it would refuse the flag")
	}
}

func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestChatArgsCarryTheLevel(t *testing.T) {
	c := &claudeCode{bin: "claude"}
	local := c.chatArgs(Request{Chat: &Chat{Level: spec.LevelEdits, Ceiling: spec.LevelEdits}}, t.TempDir(), 1)
	if flagValue(local, "--permission-mode") != "acceptEdits" || slices.Contains(local, "--allow-dangerously-skip-permissions") {
		t.Errorf("local edits: %v", local)
	}
	if !strings.Contains(flagValue(local, "--settings"), `Read(//proc/**)`) {
		t.Errorf("a local chat keeps orbit's deny rules at every level: %s", flagValue(local, "--settings"))
	}
	sb := &Sandbox{Profile: "workstation"}
	full := c.chatArgs(Request{Chat: &Chat{Level: spec.LevelAsk, Ceiling: spec.LevelFull, Sandbox: sb}}, t.TempDir(), 1)
	if flagValue(full, "--permission-mode") != "default" || !slices.Contains(full, "--allow-dangerously-skip-permissions") {
		t.Errorf("a sandboxed chat whose ceiling is full can be raised to it: %v", full)
	}
	edits := c.chatArgs(Request{Chat: &Chat{Level: spec.LevelAsk, Ceiling: spec.LevelEdits, Sandbox: sb}}, t.TempDir(), 1)
	if slices.Contains(edits, "--allow-dangerously-skip-permissions") {
		t.Errorf("below a full ceiling the process cannot bypass: %v", edits)
	}
}

type events struct {
	mu   sync.Mutex
	list []Event
}

func (e *events) emit(ev Event) {
	e.mu.Lock()
	e.list = append(e.list, ev)
	e.mu.Unlock()
}

func (e *events) of(kind string) []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []map[string]any
	for _, ev := range e.list {
		if ev.Kind == kind {
			var m map[string]any
			_ = json.Unmarshal(ev.Payload, &m)
			out = append(out, m)
		}
	}
	return out
}

func chatRun(t *testing.T, bin string, chat *Chat) *events {
	t.Helper()
	r, err := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	if err != nil {
		t.Fatal(err)
	}
	evs := &events{}
	if _, err := r.Run(context.Background(), Request{Workdir: t.TempDir(), System: "s", Prompt: "hi", Chat: chat}, evs.emit); err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestChatPermissionRequestsGoToThePrompt(t *testing.T) {
	bin, dir := fakeClaudeBin(t, fakeScenario{Ask: []json.RawMessage{
		json.RawMessage(`{"tool_name":"Write","input":{"file_path":"a.txt"},"tool_use_id":"t1","decision_reason":"writes a file","blocked_path":"/x"}`),
	}})
	var seen claudeproto.PermissionRequest
	evs := chatRun(t, bin, &Chat{Level: spec.LevelAsk, Ceiling: spec.LevelAsk,
		Prompt: func(_ context.Context, req claudeproto.PermissionRequest) claudeproto.Permission {
			seen = req
			return claudeproto.Permission{Allow: true}
		}})
	if seen.Tool != "Write" || seen.DecisionReason != "writes a file" || seen.BlockedPath != "/x" {
		t.Errorf("the prompt saw %+v", seen)
	}
	if got := evs.of("orbit.permission"); len(got) != 1 || got[0]["allowed"] != true || got[0]["request_id"] != "ask-1-0" {
		t.Errorf("orbit.permission = %v", got)
	}
	log := read(t, dir+"/log")
	if !strings.Contains(log, `answer ask-1-0 {"behavior":"allow"`) {
		t.Errorf("the CLI was not answered allow: %s", log)
	}
	if !strings.Contains(read(t, dir+"/starts"), "--permission-prompt-tool stdio") {
		t.Error("a chat process has the stdio permission prompt")
	}
}

func TestChatWithoutAPromptDeniesAndRecords(t *testing.T) {
	bin, dir := fakeClaudeBin(t, fakeScenario{Ask: []json.RawMessage{json.RawMessage(`{"tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"t1"}`)}})
	evs := chatRun(t, bin, &Chat{Level: spec.LevelAsk, Ceiling: spec.LevelAsk})
	if got := evs.of("orbit.permission"); len(got) != 1 || got[0]["allowed"] != false {
		t.Errorf("orbit.permission = %v", got)
	}
	if log := read(t, dir+"/log"); !strings.Contains(log, `"behavior":"deny"`) {
		t.Errorf("the CLI was not answered deny: %s", log)
	}
}

func TestAutoFallsBackToAskWhereTheCLIRefusesIt(t *testing.T) {
	bin, dir := fakeClaudeBin(t, fakeScenario{Refuse: []string{"auto"}})
	evs := chatRun(t, bin, &Chat{Level: spec.LevelAuto, Ceiling: spec.LevelAuto})
	log := read(t, dir+"/log")
	if !strings.Contains(log, "refused auto\nmode default\nturn 1 default") {
		t.Errorf("log = %q", log)
	}
	got := evs.of("orbit.permissions")
	if len(got) != 1 || got[0]["level"] != "auto" || got[0]["applied"] != "ask" || !strings.Contains(got[0]["reason"].(string), "Cannot set") {
		t.Errorf("orbit.permissions = %v", got)
	}

	bin, dir = fakeClaudeBin(t, fakeScenario{})
	evs = chatRun(t, bin, &Chat{Level: spec.LevelAuto, Ceiling: spec.LevelAuto})
	if log := read(t, dir+"/log"); !strings.Contains(log, "mode auto\nturn 1 auto") {
		t.Errorf("log = %q", log)
	}
	if got := evs.of("orbit.permissions"); len(got) != 1 || got[0]["applied"] != "auto" {
		t.Errorf("orbit.permissions = %v", got)
	}
}
