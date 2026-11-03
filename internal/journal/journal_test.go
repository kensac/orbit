package journal

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// memJournal is the store's half of projection, in memory: it numbers
// revisions and answers lookups, so the projector is tested alone.
type memJournal struct {
	items  []Item
	state  AttemptState
	events int
}

func (m *memJournal) look(id string) (*Item, error) {
	for i := len(m.items) - 1; i >= 0; i-- {
		if m.items[i].ID == id {
			return &m.items[i], nil
		}
	}
	return nil, nil
}

func (m *memJournal) apply(t *testing.T, ws []Write) {
	t.Helper()
	for _, w := range ws {
		rev := 1
		if prev, _ := m.look(w.ID); prev != nil {
			rev = prev.Revision + 1
		}
		b, err := json.Marshal(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		turn, parent := w.Turn, w.Parent
		m.items = append(m.items, Item{ID: w.ID, Kind: w.Kind, Revision: rev, Seq: len(m.items) + 1,
			Turn: &turn, Parent: &parent, At: w.At, Body: b})
	}
}

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func (m *memJournal) feed(t *testing.T, attempt int, lines ...string) {
	t.Helper()
	for _, l := range lines {
		var h struct{ Type, Subtype string }
		_ = json.Unmarshal([]byte(l), &h)
		kind := h.Type
		if h.Subtype != "" {
			kind += "." + h.Subtype
		}
		if strings.HasPrefix(l, "orbit.") {
			kind, l, _ = strings.Cut(l, " ")
		}
		m.events++
		seq := m.events
		ws, err := Project(&m.state, Event{Attempt: attempt, Seq: seq, At: t0.Add(time.Duration(seq) * time.Second),
			Kind: kind, Payload: []byte(l)}, m.look)
		if err != nil {
			t.Fatal(err)
		}
		m.apply(t, ws)
	}
}

func (m *memJournal) latest(t *testing.T, id string, into any) Item {
	t.Helper()
	it, _ := m.look(id)
	if it == nil {
		t.Fatalf("no item %s in %s", id, m.dump())
	}
	if into != nil {
		if err := json.Unmarshal(it.Body, into); err != nil {
			t.Fatal(err)
		}
	}
	return *it
}

func (m *memJournal) dump() string {
	var b strings.Builder
	for _, it := range m.items {
		b.WriteString("\n" + it.ID + " r" + string(rune('0'+it.Revision)) + " " + it.Kind + " " + string(it.Body))
	}
	return b.String()
}

func TestAClaudeCodeTurnBecomesTypedItems(t *testing.T) {
	m := &memJournal{}
	big := strings.Repeat("line of output\n", 400)
	bigJSON, _ := json.Marshal(big)
	m.feed(t, 1,
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-x"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"I should read the file first."},`+
			`{"type":"text","text":"Reading."},`+
			`{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/data/runs/r-1/inputs/a.json"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":`+string(bigJSON)+`}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Task","input":{"description":"check totals","prompt":"..."}}]}}`,
		`{"type":"user","parent_tool_use_id":"t2","message":{"content":[{"type":"text","text":"check totals"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"t2","message":{"content":[{"type":"tool_use","id":"t3","name":"Edit",`+
			`"input":{"file_path":"/data/runs/r-1/notes.md","old_string":"a\nb","new_string":"c"}}]}}`,
		`{"type":"user","parent_tool_use_id":"t2","message":{"content":[{"type":"tool_result","tool_use_id":"t3","content":[{"type":"text","text":"ok"}]}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"done","is_error":true}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t4","name":"TodoWrite","input":{"todos":[`+
			`{"content":"read","status":"completed"},{"content":"sum","status":"in_progress"}]}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t5","name":"StructuredOutput","input":{"summary":"s","questions":["Which card?",{"question":"Which month?","fields":[{"name":"m","type":"date"}]}]}}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":4200,"total_cost_usd":0.02,"usage":{"output_tokens":90}}`,
	)

	turn := m.latest(t, "turn:1.1", nil)
	var tb Turn
	_ = json.Unmarshal(turn.Body, &tb)
	if turn.Revision != 2 || tb.State != TurnCompleted || tb.Outcome != "success" || *tb.DurationMS != 4200 ||
		*tb.CostUSD != 0.02 || tb.CompletedAt == nil || !tb.StartedAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("turn = r%d %s", turn.Revision, turn.Body)
	}

	var th Thinking
	m.latest(t, "thinking:1.2.0", &th)
	if th.Text.Head != "I should read the file first." || th.TokensEstimate != 8 || th.Text.Truncated {
		t.Fatalf("thinking = %+v", th)
	}
	var msg Message
	if it := m.latest(t, "message:1.2.1", &msg); msg.Text != "Reading." || msg.Role != "assistant" || *it.Turn != "turn:1.1" {
		t.Fatalf("message = %+v in %v", msg, *it.Turn)
	}

	// A tool call is revised with its result: input whole, result bounded.
	var read ToolCall
	if it := m.latest(t, "tool:t1", &read); it.Revision != 2 || read.State != ToolCompleted ||
		read.Summary != "inputs/a.json" || !strings.Contains(string(read.Input), `"file_path"`) {
		t.Fatalf("read = r%d %+v", it.Revision, read)
	}
	if r := read.Result; r == nil || !r.Truncated || r.Bytes != len(big) || len(r.Head) != HeadMax || !strings.HasPrefix(big, r.Head) || len(r.SHA256) != 64 {
		t.Fatalf("result = %+v", read.Result)
	}

	// A subagent's items name the Task call that spawned it.
	var sub Message
	if it := m.latest(t, "message:1.5.0", &sub); sub.Role != "user" || *it.Parent != "tool:t2" {
		t.Fatalf("subagent prompt = %+v parent %v", sub, *it.Parent)
	}
	edit := m.latest(t, "tool:t3", nil)
	if *edit.Parent != "tool:t2" {
		t.Fatalf("subagent tool parent = %v", *edit.Parent)
	}
	var task ToolCall
	if m.latest(t, "tool:t2", &task); task.State != ToolFailed || task.Result.Head != "done" {
		t.Fatalf("task = %+v", task)
	}

	// The successful Edit is a diff.
	var d Diff
	if it := m.latest(t, "diff:t3", &d); d.Path != "notes.md" || d.Change != "edit" || *it.Parent != "tool:t2" ||
		d.Patch.Head != "--- a/notes.md\n+++ b/notes.md\n@@\n-a\n-b\n+c\n" {
		t.Fatalf("diff = %+v %q", d, d.Patch.Head)
	}

	var pl Plan
	if m.latest(t, "plan", &pl); len(pl.Entries) != 2 || pl.Entries[1] != (PlanEntry{"sum", "in_progress"}) {
		t.Fatalf("plan = %+v", pl)
	}

	// The final answer is no tool call; its questions are items.
	if it, _ := m.look("tool:t5"); it != nil {
		t.Fatal("StructuredOutput became a tool call")
	}
	var q Question
	if m.latest(t, "question:1.10.0", &q); q.Text != "Which card?" {
		t.Fatalf("question = %+v", q)
	}
	// A question with a form is an item by its text; the form is the thread's.
	if m.latest(t, "question:1.10.1", &q); q.Text != "Which month?" {
		t.Fatalf("question with a form = %+v", q)
	}
	var st Status
	if m.latest(t, "status:1.1.0", &st); st.Code != "system.init" || !strings.Contains(st.Detail.Head, `"claude-x"`) {
		t.Fatalf("status = %+v", st)
	}
	if m.state.OpenTurn != "" || m.state.Turns != 1 {
		t.Fatalf("state = %+v", m.state)
	}
}

func TestTurnStateAndOutcomeAreSeparate(t *testing.T) {
	for _, c := range []struct {
		result, outcome, err string
	}{
		{`{"type":"result","subtype":"success","is_error":true,"result":"Invalid API key"}`, "error", "Invalid API key"},
		{`{"type":"result","subtype":"error_max_turns","is_error":true}`, "max_turns", ""},
		{`{"type":"result","subtype":"error_during_execution","errors":["boom"]}`, "error", "boom"},
	} {
		m := &memJournal{}
		m.feed(t, 1, `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`, c.result)
		var tb Turn
		m.latest(t, "turn:1.1", &tb)
		// A failed turn still completed: the backend answered.
		if tb.State != TurnCompleted || tb.Outcome != c.outcome || tb.Error != c.err {
			t.Errorf("%s: turn = %+v", c.result, tb)
		}
	}

	// A turn its attempt left open is closed by how the attempt ended.
	for outcome, want := range map[string]string{
		"lost": TurnUnverifiable, "": TurnUnverifiable, "handed_back": TurnInterrupted,
		"failed": TurnInterrupted, "succeeded": TurnCompleted,
	} {
		m := &memJournal{}
		m.feed(t, 2, `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}`)
		ws, err := Close(&m.state, outcome, t0.Add(time.Hour), m.look)
		if err != nil || len(ws) != 1 {
			t.Fatalf("close = %v %v", ws, err)
		}
		m.apply(t, ws)
		var tb Turn
		it := m.latest(t, "turn:2.1", &tb)
		if tb.State != want || tb.Outcome != "" || it.Revision != 2 || (want == TurnUnverifiable) != (tb.CompletedAt == nil) {
			t.Errorf("%q: turn = r%d %+v", outcome, it.Revision, tb)
		}
		if ws, _ := Close(&m.state, outcome, t0, m.look); ws != nil {
			t.Errorf("%q: closed twice", outcome)
		}
	}
}

func TestALiveProcessOpensATurnPerResult(t *testing.T) {
	m := &memJournal{}
	m.feed(t, 1,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"one"}]}}`,
		`{"type":"result","subtype":"success"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"two"}]}}`,
		`{"type":"result","subtype":"success"}`)
	if m.state.Turns != 2 {
		t.Fatalf("turns = %d", m.state.Turns)
	}
	if it := m.latest(t, "message:1.3.0", nil); *it.Turn != "turn:1.2" {
		t.Fatalf("second message in %v", *it.Turn)
	}
}

func TestApprovalsAreRequestThenDecision(t *testing.T) {
	m := &memJournal{}
	m.feed(t, 1,
		`{"type":"control_request","request_id":"q1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf /"}}}`,
		`orbit.permission {"request_id":"q1","tool":"Bash","allowed":false,"message":"not in the build policy"}`,
		`{"type":"control_request","request_id":"q2","request":{"subtype":"interrupt"}}`)
	var ap Approval
	it := m.latest(t, "approval:q1", &ap)
	if it.Revision != 2 || ap.State != ApprovalDenied || ap.Tool != "Bash" || ap.Message != "not in the build policy" ||
		!strings.Contains(string(ap.Input), "rm -rf") {
		t.Fatalf("approval = r%d %+v", it.Revision, ap)
	}
	var first Approval
	_ = json.Unmarshal(m.items[1].Body, &first)
	if m.items[1].ID != "approval:q1" || first.State != ApprovalPending {
		t.Fatalf("first revision = %s", m.items[1].Body)
	}
	var st Status
	if m.latest(t, "status:1.3.0", &st); st.Code != "control_request" {
		t.Fatalf("other control request = %+v", st)
	}
}

func TestASteerIsAnItemInItsTurn(t *testing.T) {
	m := &memJournal{}
	m.feed(t, 1,
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`orbit.steer {"message_id":"m2","text":"also banana","author":"kanishk"}`,
		`{"type":"command_lifecycle","command_uuid":"m2","state":"started"}`,
		`{"type":"result","subtype":"success"}`)
	var st Steer
	it := m.latest(t, "steer:m2", &st)
	if it.Kind != KindSteer || *it.Turn != "turn:1.1" || st.Text != "also banana" || st.Author != "kanishk" {
		t.Fatalf("steer = %s %v %+v", it.Kind, it.Turn, st)
	}
	if it, _ := m.look("status:1.3.0"); it != nil {
		t.Fatal("a lifecycle frame is an item")
	}
}

func TestCodexItemsAreOneItemAcrossTheirLines(t *testing.T) {
	m := &memJournal{}
	m.feed(t, 1,
		`{"type":"thread.started","thread_id":"th"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"i0","type":"reasoning","text":"thinking it over"}}`,
		`{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"cat inputs/a.json","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"cat inputs/a.json","aggregated_output":"{}","exit_code":1,"status":"failed"}}`,
		`{"type":"item.started","item":{"id":"i2","type":"todo_list","items":[{"text":"read","completed":false}]}}`,
		`{"type":"item.updated","item":{"id":"i2","type":"todo_list","items":[{"text":"read","completed":true}]}}`,
		`{"type":"item.completed","item":{"id":"i3","type":"file_change","status":"completed","changes":[{"path":"/data/runs/r-1/notes.md","kind":"add"}]}}`,
		`{"type":"item.completed","item":{"id":"i4","type":"agent_message","text":"{\"summary\":\"s\"}"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":7}}`)
	var cmd ToolCall
	if it := m.latest(t, "tool:codex.i1", &cmd); it.Revision != 2 || cmd.State != ToolFailed || cmd.Name != "shell" || cmd.Result.Head != "{}" {
		t.Fatalf("command = r%d %+v", it.Revision, cmd)
	}
	var pl Plan
	if it := m.latest(t, "plan", &pl); it.Revision != 2 || pl.Entries[0].Status != "completed" {
		t.Fatalf("plan = %+v", pl)
	}
	var d Diff
	if m.latest(t, "diff:codex.i3.0", &d); d.Path != "notes.md" || d.Change != "add" || d.Patch != nil {
		t.Fatalf("diff = %+v", d)
	}
	var th Thinking
	if m.latest(t, "thinking:codex.i0", &th); th.TokensEstimate != 4 {
		t.Fatalf("thinking = %+v", th)
	}
	if it, _ := m.look("message:codex.i4"); it != nil {
		t.Fatal("the structured answer became prose")
	}
	var tb Turn
	if m.latest(t, "turn:1.1", &tb); tb.State != TurnCompleted || tb.Outcome != "success" || !strings.Contains(string(tb.Usage), "output_tokens") {
		t.Fatalf("turn = %+v", tb)
	}

	m = &memJournal{}
	m.feed(t, 1, `{"type":"turn.started"}`, `{"type":"turn.failed","error":{"message":"model not supported"}}`)
	if m.latest(t, "turn:1.1", &tb); tb.State != TurnCompleted || tb.Outcome != "error" || tb.Error != "model not supported" {
		t.Fatalf("failed turn = %+v", tb)
	}
}

func TestAnAnthropicResponseIsOneTurn(t *testing.T) {
	m := &memJournal{}
	ws, err := Project(&m.state, Event{Attempt: 1, Seq: 1, At: t0, Kind: "response",
		Payload: []byte(`{"content":[{"type":"text","text":"Here."},{"type":"tool_use","name":"submit","input":{"questions":["Why?"]}}],"stop_reason":"max_tokens"}`)}, m.look)
	if err != nil {
		t.Fatal(err)
	}
	m.apply(t, ws)
	var tb Turn
	if m.latest(t, "turn:1.1", &tb); tb.State != TurnCompleted || tb.Outcome != "max_tokens" {
		t.Fatalf("turn = %+v", tb)
	}
	var q Question
	if m.latest(t, "question:1.1.1", &q); q.Text != "Why?" {
		t.Fatalf("question = %+v", q)
	}
	var msg Message
	if m.latest(t, "message:1.1.0", &msg); msg.Text != "Here." {
		t.Fatalf("message = %+v", msg)
	}
}

func TestProjectionIsDeterministic(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"a"},{"type":"tool_use","id":"x","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"out"}]}}`,
		`{"type":"result","subtype":"success"}`,
	}
	a, b := &memJournal{}, &memJournal{}
	a.feed(t, 3, lines...)
	b.feed(t, 3, lines...)
	ja, _ := json.Marshal(a.items)
	jb, _ := json.Marshal(b.items)
	if string(ja) != string(jb) {
		t.Fatalf("two projections differ:\n%s\n%s", ja, jb)
	}
}

func TestOrbitEventsAreStatusAndUnknownLinesNothing(t *testing.T) {
	m := &memJournal{}
	m.feed(t, 1, `orbit.session {"mode":"fresh"}`, `{"type":"stream_event","event":{}}`, `{"type":"rate_limit_event","rate_limit_info":{}}`)
	if len(m.items) != 2 || m.items[0].Kind != KindStatus || m.items[1].Kind != KindStatus {
		t.Fatalf("items = %s", m.dump())
	}
	var st Status
	_ = json.Unmarshal(m.items[0].Body, &st)
	if st.Code != "orbit.session" || st.Detail.Head != `{"mode":"fresh"}` || st.Detail.Truncated {
		t.Fatalf("status = %+v", st)
	}
	if *m.items[0].Turn != "" {
		t.Fatal("orbit's own step opened a turn")
	}
}

func TestBoundCutsAtACharacter(t *testing.T) {
	s := strings.Repeat("a", HeadMax-1) + "é and more"
	b, blob := Bound([]byte(s))
	if b.Head != strings.Repeat("a", HeadMax-1) || !b.Truncated || b.Bytes != len(s) || string(blob.Content) != s || blob.SHA256 != b.SHA256 {
		t.Fatalf("bound = %d %v %d", len(b.Head), b.Truncated, b.Bytes)
	}
	small, _ := Bound([]byte("x"))
	if small.Head != "x" || small.Truncated || small.SHA256 != "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" {
		t.Fatalf("small = %+v", small)
	}
}

func TestCursors(t *testing.T) {
	for s, ok := range map[string]bool{"1.0": true, "3.17": true, "0.1": false, "1.-1": false, "1.2.0": false,
		"01.2": false, "a.b": false, "": false, "1": false} {
		if _, got := ParseCursor(s); got != ok {
			t.Errorf("ParseCursor(%q) ok = %v", s, got)
		}
	}
	if c, _ := ParseCursor("3.17"); c != (Cursor{3, 17}) || c.String() != "3.17" {
		t.Fatalf("cursor = %+v", c)
	}
	for _, c := range []struct {
		c           Cursor
		epoch, last int
		after       int
		reset       string
	}{
		{Cursor{1, 5}, 1, 9, 5, ""},
		{Cursor{1, 9}, 1, 9, 9, ""},
		{Cursor{1, 0}, 1, 9, 0, ""},
		{Cursor{1, 5}, 2, 9, 0, ResetEpoch},
		{Cursor{2, 10}, 2, 9, 0, ResetAhead},
	} {
		if after, reset := Resume(c.c, c.epoch, c.last); after != c.after || reset != c.reset {
			t.Errorf("Resume(%v, %d, %d) = %d %q", c.c, c.epoch, c.last, after, reset)
		}
	}
}

func TestFoldKeepsFirstPositionLatestBody(t *testing.T) {
	revs := []Item{
		{ID: "tool:a", Revision: 1, Seq: 1, Body: json.RawMessage(`{"state":"running"}`)},
		{ID: "message:x", Revision: 1, Seq: 2},
		{ID: "tool:a", Revision: 2, Seq: 3, Body: json.RawMessage(`{"state":"completed"}`)},
	}
	f := Fold(revs)
	if len(f) != 2 || f[0].ID != "tool:a" || f[0].Revision != 2 || string(f[0].Body) != `{"state":"completed"}` || f[1].ID != "message:x" {
		t.Fatalf("fold = %+v", f)
	}
}

func TestUpcastKeepsNewerRowsAsStored(t *testing.T) {
	body := json.RawMessage(`{"future":true}`)
	if b, err := Upcast(Version+1, "message", body); err != nil || string(b) != string(body) {
		t.Fatalf("newer row = %s %v", b, err)
	}
	if _, err := Upcast(0, "message", body); err == nil {
		t.Fatal("a version 0 row read without error")
	}
}

func TestAStalledTurnIsInterruptedWhateverEndedIt(t *testing.T) {
	// The interrupt's result closed the turn first; the watchdog's event
	// after it says the turn did not finish its work.
	m := &memJournal{}
	m.feed(t, 1, `{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"}]}}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true}`,
		`orbit.stalled {"idle_ms":600000,"stall_ms":600000}`)
	var tb Turn
	m.latest(t, "turn:1.1", &tb)
	if tb.State != TurnInterrupted || tb.Reason != ReasonStalled || tb.Outcome != "" || tb.CompletedAt == nil {
		t.Fatalf("turn = %+v", tb)
	}
	// A kill left the turn open; the event closes it, and the attempt's
	// end finds nothing open.
	m = &memJournal{}
	m.feed(t, 1, `{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"}]}}`,
		`orbit.stalled {"idle_ms":600000,"stall_ms":600000}`)
	m.latest(t, "turn:1.1", &tb)
	if tb.State != TurnInterrupted || tb.Reason != ReasonStalled || m.state.OpenTurn != "" {
		t.Fatalf("turn = %+v, open %q", tb, m.state.OpenTurn)
	}
	if ws, _ := Close(&m.state, "handed_back", t0.Add(time.Hour), m.look); ws != nil {
		t.Fatalf("the hand-back closed the turn again: %+v", ws)
	}
	if !strings.Contains(m.dump(), `"code":"orbit.stalled"`) {
		t.Fatalf("no status item for the event: %s", m.dump())
	}
}
