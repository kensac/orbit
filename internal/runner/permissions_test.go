package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	if chat.RecordPermission == nil {
		chat.RecordPermission = func(req claudeproto.PermissionRequest, p claudeproto.Permission) error {
			b, _ := json.Marshal(map[string]any{"request_id": req.RequestID, "tool": req.Tool, "allowed": p.Allow, "message": p.Message})
			evs.emit(Event{Kind: "orbit.permission", Payload: b})
			return nil
		}
	}
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

func TestChatDeniesAllowWhenRecordingFails(t *testing.T) {
	bin, dir := fakeClaudeBin(t, fakeScenario{Ask: []json.RawMessage{json.RawMessage(`{"tool_name":"Write","tool_use_id":"t1"}`)}})
	chatRun(t, bin, &Chat{Level: spec.LevelAsk,
		Prompt: func(context.Context, claudeproto.PermissionRequest) claudeproto.Permission {
			return claudeproto.Permission{Allow: true}
		},
		RecordPermission: func(_ claudeproto.PermissionRequest, p claudeproto.Permission) error {
			if !p.Allow {
				t.Error("the recorder did not receive the prompt's allow decision")
			}
			return errors.New("write failed")
		},
	})
	if log := read(t, dir+"/log"); !strings.Contains(log, `"behavior":"deny"`) || !strings.Contains(log, "could not record") {
		t.Errorf("the CLI did not receive a recording failure denial: %s", log)
	}
}

func TestALiveChatProcessMovesBetweenLevels(t *testing.T) {
	bin, dir := fakeClaudeBin(t, fakeScenario{})
	r, err := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(2)
	defer live.Close()
	work, home := t.TempDir(), t.TempDir()
	run := func(level, resume string) (*events, string) {
		t.Helper()
		evs := &events{}
		var mode string
		sess := &Session{Home: home, Resume: resume, KeepDays: 1, Live: live, Key: "k", Open: true, LiveIdle: time.Minute,
			Started: func(m, _ string) error { mode = m; return nil }}
		if _, err := r.Run(context.Background(), Request{Workdir: work, System: "s", Prompt: "hi", Session: sess,
			Chat: &Chat{Level: level, Ceiling: spec.LevelEdits, Choice: LevelChoice{Source: "conversation", Version: 2}}}, evs.emit); err != nil {
			t.Fatal(err)
		}
		return evs, mode
	}
	evs, _ := run(spec.LevelAsk, "")
	if got := evs.of("orbit.permissions"); len(got) != 1 || got[0]["when"] != "start" || got[0]["applied"] != "ask" || got[0]["version"] != float64(2) {
		t.Fatalf("first run = %v", got)
	}
	evs, mode := run(spec.LevelEdits, "fake-1")
	if mode != "live" {
		t.Fatalf("the second run took mode %q, want the live process", mode)
	}
	if got := evs.of("orbit.permissions"); len(got) != 1 || got[0]["when"] != "turn" || got[0]["mode"] != "acceptEdits" {
		t.Fatalf("second run = %v", got)
	}
	run(spec.LevelEdits, "fake-1")
	if starts := lines(t, dir+"/starts"); len(starts) != 1 {
		t.Fatalf("%d processes, want one moved between levels", len(starts))
	}
	if log := read(t, dir+"/log"); strings.Count(log, "mode ") != 1 || !strings.Contains(log, "turn 1 default\nmode acceptEdits\nturn 2 acceptEdits\nturn 3 acceptEdits") {
		t.Fatalf("log = %q: one set_permission_mode, for the change", log)
	}
}

func TestALevelChangeDuringTheTurnReachesTheProcess(t *testing.T) {
	bin, dir := fakeClaudeBin(t, fakeScenario{Ask: []json.RawMessage{json.RawMessage(`{"tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"t1"}`)}})
	changes := make(chan LevelChange, 1)
	evs := chatRun(t, bin, &Chat{Level: spec.LevelEdits, Ceiling: spec.LevelEdits, Changes: changes,
		Prompt: func(_ context.Context, _ claudeproto.PermissionRequest) claudeproto.Permission {
			changes <- LevelChange{Level: spec.LevelLocked, Choice: LevelChoice{Source: "conversation", Version: 4}}
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) && !strings.Contains(read(t, dir+"/log"), "mode dontAsk") {
				time.Sleep(10 * time.Millisecond)
			}
			return claudeproto.Permission{Message: "lowered"}
		}})
	got := evs.of("orbit.permissions")
	if len(got) != 2 || got[1]["when"] != "live" || got[1]["applied"] != "locked" || got[1]["version"] != float64(4) {
		t.Fatalf("orbit.permissions = %v", got)
	}
	if log := read(t, dir+"/log"); !strings.Contains(log, "turn 1 acceptEdits\nmode dontAsk\nanswer") {
		t.Fatalf("log = %q: the mode changed before the request was answered", log)
	}
}

func TestTheLevelIsPutBackWhenAPlanIsApproved(t *testing.T) {
	bin, dir := fakeClaudeBin(t, fakeScenario{Plan: true, Ask: []json.RawMessage{
		json.RawMessage(`{"tool_name":"ExitPlanMode","input":{"plan":"write a.txt"},"tool_use_id":"t1","requires_user_interaction":true}`),
		json.RawMessage(`{"tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"t2"}`),
	}})
	changes := make(chan LevelChange, 1)
	evs := chatRun(t, bin, &Chat{Level: spec.LevelEdits, Ceiling: spec.LevelEdits, Changes: changes,
		Prompt: func(_ context.Context, req claudeproto.PermissionRequest) claudeproto.Permission {
			if req.Tool == "ExitPlanMode" {
				// A change while planning waits for the plan.
				changes <- LevelChange{Level: spec.LevelAsk}
				time.Sleep(100 * time.Millisecond)
				return claudeproto.Permission{Allow: true}
			}
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) && !strings.Contains(read(t, dir+"/log"), "left plan\nmode default") {
				time.Sleep(10 * time.Millisecond)
			}
			return claudeproto.Permission{Allow: true}
		}})
	log := read(t, dir+"/log")
	if strings.Contains(log, "entered plan\nmode") || !strings.Contains(log, "left plan\nmode default\nanswer ask-1-1") {
		t.Fatalf("log = %q: the level waits for the plan, then is applied", log)
	}
	got := evs.of("orbit.permissions")
	if len(got) != 3 || got[1]["when"] != "live" || got[1]["applied"] != "edits" || got[2]["when"] != "plan" || got[2]["applied"] != "ask" {
		t.Fatalf("orbit.permissions = %v", got)
	}
}

func TestPendingPermissionDeniedDuringSuccessfulLiveLowering(t *testing.T) {
	bin, dir := fakeClaudeBin(t, fakeScenario{
		HoldModes: []string{"dontAsk"},
		Ask:       []json.RawMessage{json.RawMessage(`{"tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"t1"}`)},
	})
	changes := make(chan LevelChange, 1)
	var allowed bool
	ev := chatRun(t, bin, &Chat{Level: spec.LevelEdits, Ceiling: spec.LevelEdits, Changes: changes,
		Prompt: func(_ context.Context, _ claudeproto.PermissionRequest) claudeproto.Permission {
			changes <- LevelChange{Level: spec.LevelLocked, Choice: LevelChoice{Source: "conversation", Version: 7}}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) && !strings.Contains(read(t, dir+"/log"), "request dontAsk") {
				time.Sleep(2 * time.Millisecond)
			}
			if !strings.Contains(read(t, dir+"/log"), "request dontAsk") {
				t.Error("the CLI did not receive the lowering request")
			}
			return claudeproto.Permission{Allow: true}
		},
		RecordPermission: func(_ claudeproto.PermissionRequest, p claudeproto.Permission) error {
			allowed = p.Allow
			return os.WriteFile(dir+"/release-dontAsk", nil, 0o600)
		},
	})
	if allowed {
		t.Fatal("a pending permission was allowed while the CLI was lowering its mode")
	}
	if log := read(t, dir+"/log"); !strings.Contains(log, "request dontAsk\nmode dontAsk") {
		t.Fatalf("the delayed permission-mode change did not succeed: %q", log)
	}
	if got := ev.of("orbit.permissions"); len(got) != 2 || got[1]["applied"] != spec.LevelLocked {
		t.Fatalf("permission events = %v; want the locked level applied", got)
	}
}

func TestPermissionPromptUsesTheLiveLevelAtDecisionTime(t *testing.T) {
	level := spec.LevelLocked
	promptCalls := 0
	lowerOnPrompt := false
	chat := &Chat{
		Level:        spec.LevelLocked,
		CurrentLevel: func() string { return level },
		Prompt: func(context.Context, claudeproto.PermissionRequest) claudeproto.Permission {
			promptCalls++
			if lowerOnPrompt {
				level = spec.LevelLocked
			}
			return claudeproto.Permission{Allow: true}
		},
		RecordPermission: func(claudeproto.PermissionRequest, claudeproto.Permission) error { return nil },
	}
	var tools chatTools
	tools.set(chat, nil)
	h := tools.handler()

	level = spec.LevelAsk
	p, err := h.CanUseTool(context.Background(), claudeproto.PermissionRequest{Tool: "Write"})
	if err != nil || !p.Allow || promptCalls != 1 {
		t.Fatalf("locked to ask: got %+v, %v, prompts=%d; want an ask prompt", p, err, promptCalls)
	}

	level = spec.LevelAsk
	lowerOnPrompt = true
	p, err = h.CanUseTool(context.Background(), claudeproto.PermissionRequest{Tool: "Write"})
	if err != nil || p.Allow || promptCalls != 2 {
		t.Fatalf("ask to locked while pending: got %+v, %v, prompts=%d; want a denial", p, err, promptCalls)
	}
}

func TestLockedConnectorRunRecordsItsActualCLIMode(t *testing.T) {
	bin, _ := fakeClaudeBin(t, fakeScenario{})
	r, err := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	if err != nil {
		t.Fatal(err)
	}
	evs := &events{}
	conns := &Connectors{Servers: []ConnectorServer{{Name: "docs", Tools: []ConnectorTool{{Name: "write", Ask: true}}}}}
	_, err = r.Run(context.Background(), Request{Workdir: t.TempDir(), Prompt: "hi", Connectors: conns,
		Chat: &Chat{Level: spec.LevelLocked, Ceiling: spec.LevelLocked}}, evs.emit)
	if err != nil {
		t.Fatal(err)
	}
	got := evs.of("orbit.permissions")
	if len(got) != 1 || got[0]["applied"] != spec.LevelLocked || got[0]["mode"] != "default" {
		t.Fatalf("permissions event = %v; want locked applied with CLI mode default for connector prompts", got)
	}
}

func TestFailedLoweringAndInterruptKillTheProcess(t *testing.T) {
	bin, dir := fakeClaudeBin(t, fakeScenario{
		Refuse:          []string{"dontAsk"},
		RefuseInterrupt: true,
		Ask:             []json.RawMessage{json.RawMessage(`{"tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"t1"}`)},
	})
	r, err := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(2)
	defer live.Close()
	changes := make(chan LevelChange, 1)
	decisions := make(chan bool, 1)
	sess := &Session{Home: t.TempDir(), KeepDays: 1, Live: live, Key: "c1", Open: true, LiveIdle: time.Minute}
	readLog := func() string {
		b, _ := os.ReadFile(dir + "/log")
		return string(b)
	}
	chat := &Chat{Level: spec.LevelEdits, Ceiling: spec.LevelEdits, Changes: changes,
		Prompt: func(_ context.Context, _ claudeproto.PermissionRequest) claudeproto.Permission {
			changes <- LevelChange{Level: spec.LevelLocked, Choice: LevelChoice{Source: "conversation", Version: 5}}
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) && !strings.Contains(readLog(), "interrupt refused") {
				time.Sleep(10 * time.Millisecond)
			}
			return claudeproto.Permission{Allow: true}
		},
		RecordPermission: func(_ claudeproto.PermissionRequest, p claudeproto.Permission) error {
			decisions <- p.Allow
			return nil
		}}
	_, err = r.Run(context.Background(), Request{Workdir: t.TempDir(), Prompt: "run ls", Session: sess, Chat: chat}, func(Event) {})
	if err == nil {
		t.Fatal("turn succeeded after both lowering and interrupt were refused")
	}
	select {
	case allowed := <-decisions:
		if allowed {
			t.Fatal("a pending prompt was allowed after the level was lowered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the pending prompt was not recorded")
	}
	if live.Len() != 0 {
		t.Fatalf("%d live processes remain after an uninterruptible failed lowering", live.Len())
	}
	if log := read(t, dir+"/log"); !strings.Contains(log, "refused dontAsk") || !strings.Contains(log, "interrupt refused") {
		t.Fatalf("mode/interrupt refusal not observed: %q", log)
	}
}

func TestLevelChangeWaitsForExitPlanModeResult(t *testing.T) {
	bin, dir := fakeClaudeBin(t, fakeScenario{PlanExit: true, Ask: []json.RawMessage{
		json.RawMessage(`{"tool_name":"ExitPlanMode","input":{"plan":"write a.txt"},"tool_use_id":"t1"}`),
		json.RawMessage(`{"tool_name":"Write","input":{"file_path":"a.txt","content":"ok"},"tool_use_id":"t2"}`),
	}})
	r, err := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	if err != nil {
		t.Fatal(err)
	}
	changes := make(chan LevelChange, 1)
	chat := &Chat{Level: spec.LevelEdits, Ceiling: spec.LevelEdits, Changes: changes,
		Prompt: func(_ context.Context, req claudeproto.PermissionRequest) claudeproto.Permission {
			if req.Tool == "ExitPlanMode" {
				changes <- LevelChange{Level: spec.LevelAsk, Choice: LevelChoice{Source: "conversation", Version: 9}}
				time.Sleep(100 * time.Millisecond)
			}
			return claudeproto.Permission{Allow: true}
		},
		RecordPermission: func(claudeproto.PermissionRequest, claudeproto.Permission) error { return nil }}
	if _, err := r.Run(context.Background(), Request{Workdir: t.TempDir(), Prompt: "write a file", Chat: chat}, func(ev Event) {
		t.Logf("%s %s", ev.Kind, ev.Payload)
	}); err != nil {
		t.Fatal(err)
	}
	log := read(t, dir+"/log")
	plan := strings.Index(log, "entered plan")
	exitAnswer := strings.Index(log, `answer ask-1-0 {"behavior":"allow"`)
	leftPlan := strings.Index(log, "left plan")
	modeDefault := strings.Index(log, "mode default")
	writeAnswer := strings.Index(log, `answer ask-1-1 {"behavior":"allow"`)
	if plan < 0 || exitAnswer < plan || leftPlan < exitAnswer || modeDefault < leftPlan || writeAnswer < modeDefault {
		t.Fatalf("level change crossed ExitPlanMode's permission response: %q", log)
	}
}

func TestLockedChatRoutesConnectorApprovals(t *testing.T) {
	conns := &Connectors{
		Servers: []ConnectorServer{{Name: "docs", Tools: []ConnectorTool{{Name: "write", Ask: true}}}},
		Permit: func(_ context.Context, server, tool, id string, _ json.RawMessage) claudeproto.Permission {
			if server != "docs" || tool != "write" || id != "t1" {
				t.Errorf("connector approval routed with wrong identifiers: %s %s %s", server, tool, id)
			}
			return claudeproto.Permission{Allow: true}
		},
		Record: func(claudeproto.PermissionRequest, claudeproto.Permission) error { return nil },
	}
	c := &claudeCode{bin: "claude"}
	chat := &Chat{Level: spec.LevelLocked,
		Prompt: func(context.Context, claudeproto.PermissionRequest) claudeproto.Permission {
			t.Error("a locked chat must deny ordinary prompts")
			return claudeproto.Permission{Allow: true}
		},
		RecordPermission: conns.Record,
	}
	args := c.chatArgs(Request{Chat: chat, Connectors: conns}, t.TempDir(), 1)
	if flagValue(args, "--permission-mode") != "default" || !strings.Contains(flagValue(args, "--settings"), "mcp__docs__write") {
		t.Errorf("connector ask rules are not active: %v", args)
	}
	var tools chatTools
	tools.set(chat, nil)
	tools.connectors(conns)
	h := tools.handler()
	for tool, allow := range map[string]bool{"mcp__docs__write": true, "Write": false} {
		p, err := h.CanUseTool(context.Background(), claudeproto.PermissionRequest{Tool: tool, ToolUseID: "t1"})
		if err != nil || p.Allow != allow {
			t.Errorf("%s: got %+v (%v), want allow=%v", tool, p, err, allow)
		}
	}
}
