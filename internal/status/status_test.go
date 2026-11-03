package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// at is t0 plus n seconds.
func at(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }

func ev(n int, name string, f ...func(*Event)) Event {
	e := Event{Name: name, At: at(n), PromptID: "p1"}
	if name == "SessionStart" {
		e.PromptID = ""
	}
	for _, g := range f {
		g(&e)
	}
	return e
}

func tool(name string) func(*Event)  { return func(e *Event) { e.Tool = name } }
func agent(id string) func(*Event)   { return func(e *Event) { e.AgentID = id } }
func prompt(id string) func(*Event)  { return func(e *Event) { e.PromptID = id } }
func errCode(c string) func(*Event)  { return func(e *Event) { e.Error = c } }
func note(kind string) func(*Event)  { return func(e *Event) { e.Notification = kind; e.Message = "m" } }
func reason(r string) func(*Event)   { return func(e *Event) { e.Reason = r } }
func message(m string) func(*Event)  { return func(e *Event) { e.Message = m } }
func noPrompt() func(*Event)         { return func(e *Event) { e.PromptID = "" } }
func toolUse(id string) func(*Event) { return func(e *Event) { e.ToolUseID = id } }

type step struct {
	ev      Event
	state   State
	kind    Kind
	detail  string
	changed bool
}

func run(t *testing.T, steps []step) *Tracker {
	t.Helper()
	tr := NewTracker(1, t0)
	for i, s := range steps {
		changed := tr.Apply(s.ev)
		got := tr.Snapshot()
		if got == nil {
			if s.state != "" {
				t.Fatalf("step %d (%s): no snapshot, want %s", i, s.ev.Name, s.state)
			}
			continue
		}
		if got.State != s.state || got.Kind != s.kind || got.Detail != s.detail || changed != s.changed {
			t.Fatalf("step %d (%s): got %s/%s/%q changed=%v, want %s/%s/%q changed=%v",
				i, s.ev.Name, got.State, got.Kind, got.Detail, changed, s.state, s.kind, s.detail, s.changed)
		}
		if got.Attempt != 1 {
			t.Fatalf("attempt = %d", got.Attempt)
		}
	}
	return tr
}

// The turn as the real CLI 2.1.316 reported it for a Read and a Bash call.
func TestTrackerTurn(t *testing.T) {
	tr := run(t, []step{
		{ev(0, "SessionStart"), "", "", "", false},
		{ev(1, "UserPromptSubmit"), Working, "", "", true},
		{ev(2, "PreToolUse", tool("Read")), Working, "", "", false},
		{ev(3, "PostToolUse", tool("Read")), Working, "", "", false},
		{ev(4, "PreToolUse", tool("Bash")), Working, "", "", false},
		{ev(5, "PostToolUse", tool("Bash")), Working, "", "", false},
		{ev(6, "Stop"), Done, "", "", true},
		{ev(7, "SessionEnd", reason("other")), Done, "", "", false},
	})
	if s := tr.Snapshot(); s.Event != "Stop" || !s.At.Equal(at(6)) || s.PromptID != "p1" {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestTrackerWaits(t *testing.T) {
	run(t, []step{
		{ev(1, "UserPromptSubmit"), Working, "", "", true},
		{ev(2, "PreToolUse", tool("AskUserQuestion")), Waiting, Question, "", true},
		{ev(3, "PostToolUse", tool("AskUserQuestion")), Working, "", "", true},
		{ev(4, "PermissionRequest", tool("Write")), Waiting, Permission, "Write", true},
		{ev(5, "PostToolUse", tool("Write")), Working, "", "", true},
		{ev(6, "Elicitation", message("sign in to x")), Waiting, Elicitation, "sign in to x", true},
		{ev(7, "ElicitationResult"), Working, "", "", true},
		{ev(8, "Notification", note("permission_prompt")), Waiting, Permission, "m", true},
		{ev(9, "PostToolUseFailure", tool("Edit")), Working, "", "", true},
		{ev(10, "Notification", note("elicitation_dialog")), Waiting, Elicitation, "m", true},
		{ev(11, "Stop"), Done, "", "", true},
	})
}

// A notification that blocks nothing, compaction and subagent stops say
// nothing about the turn.
func TestTrackerQuietEvents(t *testing.T) {
	run(t, []step{
		{ev(1, "UserPromptSubmit"), Working, "", "", true},
		{ev(2, "Notification", note("idle_prompt")), Working, "", "", false},
		{ev(3, "PreCompact"), Working, "", "", false},
		{ev(4, "PostCompact"), Working, "", "", false},
		{ev(5, "SubagentStop", agent("a1")), Working, "", "", false},
		{ev(6, "Stop"), Done, "", "", true},
		{ev(7, "PostCompact"), Done, "", "", false},
	})
}

func TestTrackerFailures(t *testing.T) {
	run(t, []step{
		{ev(1, "UserPromptSubmit"), Working, "", "", true},
		{ev(2, "StopFailure", errCode("model_not_found")), Errored, "", "model_not_found", true},
		{ev(3, "SessionEnd", reason("other")), Errored, "", "model_not_found", false},
	})
	run(t, []step{
		{ev(1, "UserPromptSubmit"), Working, "", "", true},
		{ev(2, "StopFailure"), Errored, "", "the turn failed", true},
	})
	run(t, []step{
		{ev(1, "UserPromptSubmit"), Working, "", "", true},
		{ev(2, "SessionEnd", reason("other")), Errored, "", "the session ended before the turn finished (other)", true},
	})
	// SessionEnd with no turn seen is the process, not this run's turn.
	run(t, []step{{ev(1, "SessionEnd", noPrompt()), "", "", "", false}})
}

// A subagent's own work never moves the main agent's state, except that
// what it waits on blocks the person too, and only it clears its wait.
func TestTrackerSubagents(t *testing.T) {
	run(t, []step{
		{ev(1, "UserPromptSubmit"), Working, "", "", true},
		{ev(2, "SubagentStart", agent("a1")), Working, "", "", false},
		{ev(3, "PermissionRequest", agent("a1"), tool("Bash")), Waiting, Permission, "Bash", true},
		{ev(4, "PostToolUse", tool("Read")), Waiting, Permission, "Bash", false},
		{ev(5, "PostToolUse", agent("a2"), tool("Read")), Waiting, Permission, "Bash", false},
		{ev(6, "PostToolUse", agent("a1"), tool("Bash")), Working, "", "", true},
		{ev(7, "StopFailure", agent("a1"), errCode("x")), Working, "", "", false},
		{ev(8, "Stop", agent("a1")), Working, "", "", false},
		{ev(9, "Stop"), Done, "", "", true},
	})
	// The main agent's question is not cleared by a subagent's tool.
	run(t, []step{
		{ev(1, "PreToolUse", tool("AskUserQuestion")), Waiting, Question, "", true},
		{ev(2, "PostToolUse", agent("a1"), tool("Read")), Waiting, Question, "", false},
		{ev(3, "PostToolUse", tool("AskUserQuestion")), Working, "", "", true},
	})
}

// Events of another run's turn, from before the attempt, or older than
// the snapshot (spooled, read late) change nothing.
func TestTrackerOwnership(t *testing.T) {
	run(t, []step{
		{ev(-5, "Stop", prompt("p0")), "", "", "", false},
		{ev(-1, "UserPromptSubmit"), "", "", "", false},
		{ev(1, "UserPromptSubmit"), Working, "", "", true},
		{ev(2, "Stop", prompt("p0")), Working, "", "", false},
		{ev(3, "UserPromptSubmit", prompt("p2")), Working, "", "", false},
		{ev(5, "Stop"), Done, "", "", true},
		{ev(4, "PreToolUse", tool("AskUserQuestion")), Done, "", "", false},
		{ev(6, "PostToolUse", tool("Read")), Done, "", "", false},
	})
	tr := NewTracker(2, t0)
	if !tr.Own(ev(1, "SessionStart")) || !tr.Own(ev(2, "PreToolUse", prompt("pX"))) || tr.Own(ev(3, "Stop")) {
		t.Fatal("the first event naming a turn adopts it")
	}
}

func TestFromHook(t *testing.T) {
	in := `{"hook_event_name":"StopFailure","session_id":"s","prompt_id":"p","error":"rate_limit",
		"cwd":"/w","transcript_path":"/t","last_assistant_message":"` + strings.Repeat("x", 5000) + `"}`
	e, err := FromHook([]byte(in), t0.In(time.FixedZone("x", 3600)))
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "StopFailure" || e.SessionID != "s" || e.PromptID != "p" || e.Error != "rate_limit" || !e.At.Equal(t0) || e.At.Location() != time.UTC {
		t.Fatalf("event = %+v", e)
	}
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "xxx") {
		t.Fatal("the last message stays out of the posted event")
	}
	e, err = FromHook([]byte(`{"hook_event_name":"StopFailure","error":{"type":"overloaded"},"message":"`+strings.Repeat("é", 400)+`"}`), t0)
	if err != nil || e.Error != `{"type":"overloaded"}` || len(e.Message) > maxField || !strings.HasPrefix(e.Message, "é") {
		t.Fatalf("event = %+v, %v", e, err)
	}
	if strings.ContainsRune(e.Message, '\uFFFD') || len(e.Message)%2 != 0 {
		t.Fatalf("message clipped mid-rune: %d bytes", len(e.Message))
	}
	if _, err := FromHook([]byte("{"), t0); err == nil {
		t.Fatal("bad JSON is an error")
	}
	if len(HookEvents) != 16 || !ToolEvents["PermissionRequest"] || ToolEvents["Stop"] {
		t.Fatal("hook events changed: check the real CLI and the runner test")
	}
}

func TestDerive(t *testing.T) {
	started, finished := at(1), at(9)
	errMsg := "claude-code: " + strings.Repeat("e", 400)
	hook := func(s State, k Kind, attempt int) *Snapshot {
		return &Snapshot{State: s, Kind: k, Detail: "d", Event: "x", At: at(5), Attempt: attempt}
	}
	cases := []struct {
		name string
		r    Run
		want Status
	}{
		{"queued", Run{Status: "queued", CreatedAt: t0},
			Status{State: Queued, Since: &t0, Source: SourceRun}},
		{"running, no hooks", Run{Status: "running", Attempt: 1, StartedAt: &started},
			Status{State: Working, Since: &started, Source: SourceRun}},
		{"running, hook waiting", Run{Status: "running", Attempt: 1, StartedAt: &started, Hook: hook(Waiting, Permission, 1)},
			Status{State: Waiting, Kind: Permission, Detail: "d", Since: ptr(at(5)), Source: SourceHooks}},
		{"running, hook done: orbit still records", Run{Status: "running", Attempt: 1, Hook: hook(Done, "", 1)},
			Status{State: Working, Detail: "d", Since: ptr(at(5)), Source: SourceHooks}},
		{"running, hook errored", Run{Status: "running", Attempt: 1, Hook: hook(Errored, "", 1)},
			Status{State: Errored, Detail: "d", Since: ptr(at(5)), Source: SourceHooks}},
		{"running, an earlier attempt's hook", Run{Status: "running", Attempt: 2, StartedAt: &started, Hook: hook(Waiting, Permission, 1)},
			Status{State: Working, Since: &started, Source: SourceRun}},
		{"succeeded over a hook", Run{Status: "succeeded", Attempt: 1, FinishedAt: &finished, Hook: hook(Errored, "", 1)},
			Status{State: Done, Since: &finished, Source: SourceRun}},
		{"skipped", Run{Status: "skipped", FinishedAt: &finished},
			Status{State: Done, Since: &finished, Source: SourceRun}},
		{"question open", Run{Status: "succeeded", FinishedAt: &finished, OpenQuestion: true},
			Status{State: Waiting, Kind: Question, Since: &finished, Source: SourceRun}},
		{"action pending wins", Run{Status: "succeeded", FinishedAt: &finished, OpenQuestion: true, PendingAction: true},
			Status{State: Waiting, Kind: Approval, Since: &finished, Source: SourceRun}},
		{"failed", Run{Status: "failed", FinishedAt: &finished, Error: &errMsg, Hook: hook(Done, "", 1), Attempt: 1},
			Status{State: Errored, Detail: errMsg[:maxDetail], Since: &finished, Source: SourceRun}},
		{"cancelled", Run{Status: "cancelled", FinishedAt: &finished},
			Status{State: Idle, Since: &finished, Source: SourceRun}},
	}
	for _, c := range cases {
		c.r.ID, c.want.RunID = "r", "r"
		got := Derive(c.r)
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(c.want)
		if string(gb) != string(wb) {
			t.Errorf("%s:\n got %s\nwant %s", c.name, gb, wb)
		}
	}
	if n := None(); n.State != Idle || n.RunID != "" {
		t.Fatalf("none = %+v", n)
	}
}

// collect is a listener handler that records events.
type collect struct {
	mu  sync.Mutex
	evs []Event
}

func (c *collect) handle(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, e)
}

func (c *collect) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.evs {
		out = append(out, e.Name)
	}
	return out
}

// sockDir is a short directory for a socket: a test's TempDir can exceed
// the unix socket path limit on macOS.
func sockDir(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "ost")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestChannelPostAndSpool(t *testing.T) {
	state := t.TempDir()
	sock, spool := filepath.Join(sockDir(t), "s.sock"), SpoolPath(state)

	// No listener yet: the post lands in the spool, and Listen reads it.
	if err := Post(sock, spool, ev(1, "UserPromptSubmit")); err != nil {
		t.Fatal(err)
	}
	var c collect
	l, err := Listen(sock, spool, c.handle)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", fi, err)
	}
	if got := strings.Join(c.names(), ","); got != "UserPromptSubmit" {
		t.Fatalf("after listen: %s", got)
	}
	// Through the socket, acknowledged.
	if err := Post(sock, spool, ev(2, "PreToolUse", tool("Read"), toolUse("t1"))); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.names(), ","); got != "UserPromptSubmit,PreToolUse" {
		t.Fatalf("after post: %s", got)
	}
	// A line spooled while the listener runs, a half-written one, and a
	// repeat of a delivered event: Close reads the complete new line once.
	b, _ := json.Marshal(ev(2, "PreToolUse", tool("Read"), toolUse("t1")))
	stop, _ := json.Marshal(ev(3, "Stop"))
	f, _ := os.OpenFile(spool, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.Write(append(append(append(b, '\n'), stop...), "\n{\"event\":\"Sess"...))
	_ = f.Close()
	l.Close()
	l.Close()
	if got := strings.Join(c.names(), ","); got != "UserPromptSubmit,PreToolUse,Stop" {
		t.Fatalf("after close: %s", got)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
	// Closed: posts spool again.
	if err := Post(sock, spool, ev(4, "SessionEnd")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(spool); !strings.Contains(string(b), `"event":"SessionEnd"`) {
		t.Fatalf("spool:\n%s", b)
	}
	var nilL *Listener
	nilL.Close()
}

// The listener's poll reads what a hook spooled while it runs.
func TestChannelSpoolPolledWhileListening(t *testing.T) {
	state := t.TempDir()
	sock, spool := filepath.Join(sockDir(t), "s.sock"), SpoolPath(state)
	var c collect
	l, err := Listen(sock, spool, c.handle)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := Post("", spool, ev(1, "Stop")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * spoolPoll)
	for len(c.names()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := strings.Join(c.names(), ","); got != "Stop" {
		t.Fatalf("polled: %q", got)
	}
}

// A stale socket file from a listener that died is replaced; a post to a
// socket nobody serves spools.
func TestChannelStaleSocket(t *testing.T) {
	state := t.TempDir()
	sock, spool := filepath.Join(sockDir(t), "s.sock"), SpoolPath(state)
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Post(sock, spool, ev(1, "Stop")); err != nil {
		t.Fatal(err)
	}
	var c collect
	l, err := Listen(sock, spool, c.handle)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if got := strings.Join(c.names(), ","); got != "Stop" {
		t.Fatalf("got %q", got)
	}
	if err := Post("", "", ev(1, "Stop")); err == nil {
		t.Fatal("nowhere to post is an error")
	}
}

func TestSocketPath(t *testing.T) {
	a, b := SocketPath("/data/agents/x/home/.orbit-harness"), SocketPath("/data/agents/x/home/.orbit-harness/")
	if a != b || a == SocketPath("/data/agents/y/home/.orbit-harness") {
		t.Fatal("one socket per state directory")
	}
	// The unix socket limit is 104 bytes on macOS: the name adds 33 to the temp dir.
	if len(filepath.Base(a)) != len("orbit-hook-")+16+len(".sock") || filepath.Dir(a) != filepath.Clean(os.TempDir()) {
		t.Fatalf("socket = %s", a)
	}
}

func TestAggregate(t *testing.T) {
	finished := at(9)
	errMsg := "boom"
	done := &Run{ID: "r2", Status: "succeeded", FinishedAt: &finished}
	failed := &Run{ID: "r2", Status: "failed", FinishedAt: &finished, Error: &errMsg}
	running := &Run{ID: "r3", Status: "running", Attempt: 1, StartedAt: &finished}
	cases := []struct {
		name          string
		latest        *Run
		open, pending bool
		state         State
		kind          Kind
	}{
		{"no run", nil, false, false, Idle, ""},
		{"done", done, false, false, Done, ""},
		{"an older question still open", done, true, false, Waiting, Question},
		{"an older action pending", done, true, true, Waiting, Approval},
		{"a failure gives way to a waiting question", failed, true, false, Waiting, Question},
		{"failed", failed, false, false, Errored, ""},
		{"a running run speaks for itself", running, true, true, Working, ""},
	}
	for _, c := range cases {
		got := Aggregate(c.latest, c.open, c.pending)
		if got.State != c.state || got.Kind != c.kind {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, got.State, got.Kind, c.state, c.kind)
		}
		if c.latest != nil && got.RunID != c.latest.ID {
			t.Errorf("%s: run id %q", c.name, got.RunID)
		}
		if c.state == Waiting && got.Detail != "" {
			t.Errorf("%s: detail %q", c.name, got.Detail)
		}
	}
}
