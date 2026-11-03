package journal

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Event is one stored transcript entry (a run_events row).
type Event struct {
	Attempt int
	Seq     int
	At      time.Time
	Kind    string
	Payload []byte
}

// Write is one item revision Project produces. The store gives it its seq
// and its revision (one past the item's latest).
type Write struct {
	ID     string
	Kind   string
	Turn   string
	Parent string
	At     time.Time
	Body   any
	Blobs  []Blob
}

// Lookup returns the latest revision of an item in the timeline being
// projected, nil when there is none.
type Lookup func(id string) (*Item, error)

// AttemptState is what projecting one attempt carries from event to
// event: the turn open now, and how many it has opened.
type AttemptState struct {
	OpenTurn string `json:"open_turn,omitempty"`
	Turns    int    `json:"turns"`
}

// submitTools are the calls through which a backend returns its final
// answer (Claude Code's --json-schema tool, the anthropic runner's forced
// tool). The answer is the run's report, not a step; its questions are
// question items.
var submitTools = map[string]bool{"StructuredOutput": true, "submit": true}

// Project turns one transcript event into journal writes. Event kinds
// are disjoint across backends (Claude Code's stream-json, Codex's exec
// --json, the Messages API's response, orbit's own orbit.*), so the event
// alone says how to read it. An event that carries nothing a person would
// see (a partial stream frame, an unknown line) writes nothing; run_events
// keeps it.
func Project(a *AttemptState, ev Event, look Lookup) ([]Write, error) {
	p := &projector{a: a, ev: ev, look: look}
	p.event()
	return p.out, p.err
}

type projector struct {
	a    *AttemptState
	ev   Event
	look Lookup
	out  []Write
	err  error
	n    int // writes keyed by event position so far
}

// local is an id for an item this event creates that has no identity of
// its own: unique within the run, the same on every projection.
func (p *projector) local(prefix string) string {
	p.n++
	return prefix + ":" + strconv.Itoa(p.ev.Attempt) + "." + strconv.Itoa(p.ev.Seq) + "." + strconv.Itoa(p.n-1)
}

func (p *projector) write(id, kind, parent string, body any, blobs ...Blob) {
	p.out = append(p.out, Write{ID: id, Kind: kind, Turn: p.a.OpenTurn, Parent: parent, At: p.ev.At, Body: body, Blobs: blobs})
}

// latest reads an item's newest revision, seeing this event's own writes
// first.
func (p *projector) latest(id string, into any) bool {
	for i := len(p.out) - 1; i >= 0; i-- {
		if p.out[i].ID == id {
			b, _ := json.Marshal(p.out[i].Body)
			return json.Unmarshal(b, into) == nil
		}
	}
	if p.err != nil {
		return false
	}
	it, err := p.look(id)
	if err != nil {
		p.err = err
		return false
	}
	return it != nil && json.Unmarshal(it.Body, into) == nil
}

func (p *projector) status() {
	d, blob := Bound(p.ev.Payload)
	p.write(p.local("status"), KindStatus, "", Status{Code: p.ev.Kind, Detail: d}, blob)
}

// openTurn starts a turn unless one is open.
func (p *projector) openTurn() {
	if p.a.OpenTurn != "" {
		return
	}
	p.a.Turns++
	p.a.OpenTurn = "turn:" + strconv.Itoa(p.ev.Attempt) + "." + strconv.Itoa(p.a.Turns)
	p.write(p.a.OpenTurn, KindTurn, "", Turn{State: TurnRunning, StartedAt: p.ev.At})
}

// closeTurn ends the open turn with the backend's verdict.
func (p *projector) closeTurn(outcome, errText string, durationMS *int64, cost *float64, usage json.RawMessage) {
	if p.a.OpenTurn == "" {
		p.openTurn()
	}
	var t Turn
	if !p.latest(p.a.OpenTurn, &t) {
		t = Turn{StartedAt: p.ev.At}
	}
	at := p.ev.At
	t.State, t.Outcome, t.Error, t.CompletedAt, t.DurationMS, t.CostUSD = TurnCompleted, outcome, errText, &at, durationMS, cost
	if len(usage) > 0 && string(usage) != "null" {
		t.Usage = usage
	}
	p.write(p.a.OpenTurn, KindTurn, "", t)
	p.a.OpenTurn = ""
}

// Close ends an attempt's open turn when the attempt itself has ended
// (outcome is run_attempts.outcome, or "" when the attempt has no row and
// a later claim exists): the turn never reached its result, so how it
// ended is what orbit saw of its process.
func Close(a *AttemptState, outcome string, at time.Time, look Lookup) ([]Write, error) {
	if a.OpenTurn == "" {
		return nil, nil
	}
	it, err := look(a.OpenTurn)
	if err != nil {
		return nil, err
	}
	var t Turn
	if it != nil {
		_ = json.Unmarshal(it.Body, &t)
	}
	switch outcome {
	case "succeeded", "skipped":
		t.State = TurnCompleted
	case "failed", "handed_back":
		t.State = TurnInterrupted
	default: // lost, or unknown
		t.State = TurnUnverifiable
	}
	if t.State != TurnUnverifiable {
		t.CompletedAt = &at
	}
	w := Write{ID: a.OpenTurn, Kind: KindTurn, Turn: a.OpenTurn, At: at, Body: t}
	a.OpenTurn = ""
	return []Write{w}, nil
}

func (p *projector) event() {
	switch k := p.ev.Kind; {
	case strings.HasPrefix(k, "orbit."):
		p.orbit()
	case k == "assistant" || k == "user":
		p.claudeMessage()
	case k == "result" || strings.HasPrefix(k, "result."):
		p.claudeResult()
	case k == "system.init":
		p.openTurn()
		p.status()
	case strings.HasPrefix(k, "system"), k == "rate_limit_event", k == "raw", k == "thread.started", k == "error":
		p.status()
	case k == "control_request":
		p.controlRequest()
	case k == "turn.started":
		p.openTurn()
	case k == "turn.completed":
		var ev struct {
			Usage json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal(p.ev.Payload, &ev)
		p.closeTurn("success", "", nil, nil, ev.Usage)
	case k == "turn.failed":
		var ev struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(p.ev.Payload, &ev)
		p.closeTurn("error", ev.Error.Message, nil, nil, nil)
	case k == "item.started" || k == "item.updated" || k == "item.completed":
		p.codexItem()
	case k == "response":
		p.anthropic()
	}
}

// ---------------------------------------------------------------- orbit

func (p *projector) orbit() {
	if p.ev.Kind == "orbit.permissions" {
		var body Permissions
		if json.Unmarshal(p.ev.Payload, &body) != nil || body.Level == "" {
			p.status()
			return
		}
		p.write(p.local("permissions"), KindPermissions, "", body)
		return
	}
	if p.ev.Kind == "orbit.steer" {
		var st Steer
		if json.Unmarshal(p.ev.Payload, &st) != nil || st.MessageID == "" {
			p.status()
			return
		}
		p.write("steer:"+st.MessageID, KindSteer, "", st)
		return
	}
	if p.ev.Kind != "orbit.permission" {
		p.status()
		return
	}
	var ev struct {
		RequestID string `json:"request_id"`
		Tool      string `json:"tool"`
		Allowed   bool   `json:"allowed"`
		Message   string `json:"message"`
	}
	if json.Unmarshal(p.ev.Payload, &ev) != nil || ev.RequestID == "" {
		p.status()
		return
	}
	id := "approval:" + ev.RequestID
	var ap Approval
	if !p.latest(id, &ap) {
		ap = Approval{RequestID: ev.RequestID, Tool: ev.Tool, Input: json.RawMessage(`{}`)}
	}
	ap.State, ap.Message = ApprovalDenied, ev.Message
	if ev.Allowed {
		ap.State = ApprovalAllowed
	}
	p.write(id, KindApproval, "", ap)
}

// ---------------------------------------------------------------- claude code

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// claudeMessage reads a stream-json assistant or user line. A line from
// inside a subagent names the Task call that spawned it
// (parent_tool_use_id), which becomes each item's parent.
func (p *projector) claudeMessage() {
	var ev struct {
		ParentToolUseID string `json:"parent_tool_use_id"`
		Message         struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(p.ev.Payload, &ev) != nil {
		return
	}
	parent := ""
	if ev.ParentToolUseID != "" {
		parent = "tool:" + ev.ParentToolUseID
	}
	role := p.ev.Kind
	var blocks []block
	if json.Unmarshal(ev.Message.Content, &blocks) != nil {
		var s string
		if json.Unmarshal(ev.Message.Content, &s) == nil && strings.TrimSpace(s) != "" {
			p.openTurn()
			p.write(p.local("message"), KindMessage, parent, Message{Role: role, Text: s})
		}
		return
	}
	p.openTurn()
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				p.write(p.local("message"), KindMessage, parent, Message{Role: role, Text: b.Text})
			}
		case "thinking":
			t, blob := Bound([]byte(b.Thinking))
			p.write(p.local("thinking"), KindThinking, parent, Thinking{Text: t, TokensEstimate: estimateTokens(b.Thinking)}, blob)
		case "redacted_thinking":
			t, blob := Bound(nil)
			p.write(p.local("thinking"), KindThinking, parent, Thinking{Text: t, Redacted: true}, blob)
		case "tool_use":
			p.toolUse(b, parent)
		case "tool_result":
			p.toolResult(b, parent)
		}
	}
}

func (p *projector) toolUse(b block, parent string) {
	input := b.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	if submitTools[b.Name] {
		p.outputQuestions(in, parent)
		return
	}
	id := p.local("tool")
	callID := b.ID
	if callID != "" {
		id = "tool:" + callID
	}
	p.write(id, KindToolCall, parent, ToolCall{CallID: callID, Name: b.Name, Input: input,
		Summary: Summary(b.Name, in), State: ToolRunning})
	switch b.Name {
	case "TodoWrite":
		var v struct {
			Todos []struct {
				Content string `json:"content"`
				Status  string `json:"status"`
			} `json:"todos"`
		}
		_ = json.Unmarshal(input, &v)
		pl := Plan{Entries: []PlanEntry{}}
		for _, t := range v.Todos {
			pl.Entries = append(pl.Entries, PlanEntry{Text: t.Content, Status: t.Status})
		}
		p.write("plan", KindPlan, parent, pl)
	case "ExitPlanMode":
		var v struct {
			Plan string `json:"plan"`
		}
		_ = json.Unmarshal(input, &v)
		var pl Plan
		if !p.latest("plan", &pl) {
			pl.Entries = []PlanEntry{}
		}
		pl.Text = v.Plan
		p.write("plan", KindPlan, parent, pl)
	case "AskUserQuestion":
		var v struct {
			Questions []struct {
				Question    string           `json:"question"`
				Header      string           `json:"header"`
				MultiSelect bool             `json:"multiSelect"`
				Options     []QuestionOption `json:"options"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(input, &v)
		for i, q := range v.Questions {
			opts := q.Options
			if opts == nil {
				opts = []QuestionOption{}
			}
			p.write("question:"+strings.TrimPrefix(id, "tool:")+"."+strconv.Itoa(i), KindQuestion, parent,
				Question{Text: q.Question, Header: q.Header, Options: opts, Multi: q.MultiSelect})
		}
	}
}

// outputQuestions makes the final answer's questions (the output
// contract's questions: a string, or an object whose question is the text
// and whose form the thread shows) into question items.
func (p *projector) outputQuestions(in map[string]any, parent string) {
	qs, _ := in["questions"].([]any)
	for _, q := range qs {
		s, ok := q.(string)
		if m, isObj := q.(map[string]any); isObj {
			s, ok = m["question"].(string)
		}
		if ok && strings.TrimSpace(s) != "" {
			p.write(p.local("question"), KindQuestion, parent, Question{Text: s, Options: []QuestionOption{}})
		}
	}
}

// toolResult revises the call a result answers. A result naming no call
// this timeline has is dropped: there is nothing to attach it to.
func (p *projector) toolResult(b block, parent string) {
	if b.ToolUseID == "" {
		return
	}
	id := "tool:" + b.ToolUseID
	var call ToolCall
	if !p.latest(id, &call) {
		return
	}
	res, blob := Bound(resultText(b.Content))
	call.Result, call.State = &res, ToolCompleted
	if b.IsError {
		call.State = ToolFailed
	}
	p.write(id, KindToolCall, parent, call, blob)
	if !b.IsError {
		p.claudeDiff(call, parent)
	}
}

// resultText is a tool result's content as text: a string as it is, a
// list of blocks as their texts joined (an image as a placeholder).
func resultText(raw json.RawMessage) []byte {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []byte(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return raw
	}
	var out []string
	for _, pt := range parts {
		if pt.Type == "text" {
			out = append(out, pt.Text)
		} else {
			out = append(out, "["+pt.Type+"]")
		}
	}
	return []byte(strings.Join(out, "\n"))
}

// claudeDiff records the change a successful file tool made, as a unified
// diff of the strings it replaced (Claude Code reports no line numbers, so
// hunks carry none).
func (p *projector) claudeDiff(call ToolCall, parent string) {
	var in struct {
		FilePath   string `json:"file_path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ContentRaw string `json:"content"`
		Edits      []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if json.Unmarshal(call.Input, &in) != nil || in.FilePath == "" {
		return
	}
	path := RelPath(in.FilePath)
	var patch strings.Builder
	var change string
	switch call.Name {
	case "Edit":
		change = "edit"
		hunk(&patch, in.OldString, in.NewString)
	case "MultiEdit":
		change = "edit"
		for _, e := range in.Edits {
			hunk(&patch, e.OldString, e.NewString)
		}
	case "Write":
		change = "write"
		hunk(&patch, "", in.ContentRaw)
	default:
		return
	}
	body := "--- a/" + path + "\n+++ b/" + path + "\n" + patch.String()
	b, blob := Bound([]byte(body))
	p.write("diff:"+call.CallID, KindDiff, parent, Diff{CallID: call.CallID, Path: path, Change: change, Patch: &b}, blob)
}

func hunk(w *strings.Builder, old, new string) {
	w.WriteString("@@\n")
	for _, l := range lines(old) {
		w.WriteString("-" + l + "\n")
	}
	for _, l := range lines(new) {
		w.WriteString("+" + l + "\n")
	}
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// claudeResult ends the turn with the CLI's verdict. is_error is the field
// to trust: a failed login reports subtype success with is_error.
func (p *projector) claudeResult() {
	var r struct {
		Subtype    string          `json:"subtype"`
		IsError    bool            `json:"is_error"`
		Result     string          `json:"result"`
		Errors     []string        `json:"errors"`
		DurationMS *int64          `json:"duration_ms"`
		CostUSD    *float64        `json:"total_cost_usd"`
		Usage      json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(p.ev.Payload, &r)
	outcome, errText := "success", ""
	switch {
	case r.Subtype == "error_max_turns":
		outcome = "max_turns"
	case r.IsError || strings.HasPrefix(r.Subtype, "error"):
		outcome = "error"
	}
	if outcome != "success" {
		errText = strings.Join(r.Errors, "; ")
		if errText == "" {
			errText = r.Result
		}
		errText = Clip(errText)
	}
	p.closeTurn(outcome, errText, r.DurationMS, r.CostUSD, r.Usage)
}

// controlRequest is the CLI asking orbit whether a tool may run (a build's
// stdio permission protocol); orbit.permission records the decision.
func (p *projector) controlRequest() {
	var ev struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype  string          `json:"subtype"`
			ToolName string          `json:"tool_name"`
			Input    json.RawMessage `json:"input"`
		} `json:"request"`
	}
	if json.Unmarshal(p.ev.Payload, &ev) != nil || ev.Request.Subtype != "can_use_tool" || ev.RequestID == "" {
		p.status()
		return
	}
	in := ev.Request.Input
	if len(in) == 0 {
		in = json.RawMessage(`{}`)
	}
	p.openTurn()
	p.write("approval:"+ev.RequestID, KindApproval, "", Approval{RequestID: ev.RequestID, Tool: ev.Request.ToolName,
		Input: in, State: ApprovalPending})
}

// ---------------------------------------------------------------- codex

// codexItem reads `codex exec --json` items. An item with an id is one
// journal item across its started, updated and completed lines; one
// without (older CLIs) is recorded where it is first seen.
func (p *projector) codexItem() {
	var ev struct {
		Item struct {
			ID               string          `json:"id"`
			Type             string          `json:"type"`
			Text             string          `json:"text"`
			Command          string          `json:"command"`
			AggregatedOutput *string         `json:"aggregated_output"`
			ExitCode         *int            `json:"exit_code"`
			Status           string          `json:"status"`
			Query            string          `json:"query"`
			Server           string          `json:"server"`
			Tool             string          `json:"tool"`
			Arguments        json.RawMessage `json:"arguments"`
			Result           json.RawMessage `json:"result"`
			Error            json.RawMessage `json:"error"`
			Message          string          `json:"message"`
			Changes          []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"changes"`
			Items []struct {
				Text      string `json:"text"`
				Completed bool   `json:"completed"`
			} `json:"items"`
		} `json:"item"`
	}
	if json.Unmarshal(p.ev.Payload, &ev) != nil {
		return
	}
	it := ev.Item
	started, completed := p.ev.Kind == "item.started", p.ev.Kind == "item.completed"
	p.openTurn()
	id := func(prefix string) string {
		if it.ID != "" {
			return prefix + ":codex." + it.ID
		}
		return p.local(prefix)
	}
	switch it.Type {
	case "reasoning", "agent_message":
		t := strings.TrimSpace(it.Text)
		// The final agent_message is the structured answer, which the
		// run's report already shows.
		if !completed || t == "" || json.Valid([]byte(t)) {
			return
		}
		if it.Type == "agent_message" {
			p.write(id("message"), KindMessage, "", Message{Role: "assistant", Text: it.Text})
			return
		}
		b, blob := Bound([]byte(it.Text))
		p.write(id("thinking"), KindThinking, "", Thinking{Text: b, TokensEstimate: estimateTokens(it.Text)}, blob)
	case "command_execution", "web_search", "mcp_tool_call":
		name, input, summary := "shell", map[string]any{"command": it.Command}, Clip(it.Command)
		switch it.Type {
		case "web_search":
			name, input, summary = "web_search", map[string]any{"query": it.Query}, Clip(it.Query)
		case "mcp_tool_call":
			args := any(map[string]any{})
			if len(it.Arguments) > 0 {
				args = it.Arguments
			}
			name, input, summary = it.Server+"."+it.Tool, map[string]any{"arguments": args}, ""
		}
		in, _ := json.Marshal(input)
		if started {
			p.write(id("tool"), KindToolCall, "", ToolCall{CallID: it.ID, Name: name, Input: in, Summary: summary, State: ToolRunning})
			return
		}
		if !completed || it.ID == "" {
			return
		}
		key := "tool:codex." + it.ID
		var call ToolCall
		if !p.latest(key, &call) {
			call = ToolCall{CallID: it.ID, Name: name, Input: in, Summary: summary}
		}
		call.State = ToolCompleted
		if it.Status == "failed" || (it.ExitCode != nil && *it.ExitCode != 0) || (len(it.Error) > 0 && string(it.Error) != "null") {
			call.State = ToolFailed
		}
		var out []byte
		switch {
		case it.AggregatedOutput != nil:
			out = []byte(*it.AggregatedOutput)
		case len(it.Result) > 0 && string(it.Result) != "null":
			out = it.Result
		case len(it.Error) > 0 && string(it.Error) != "null":
			out = it.Error
		}
		var blobs []Blob
		if out != nil {
			b, blob := Bound(out)
			call.Result, blobs = &b, []Blob{blob}
		}
		p.write(key, KindToolCall, "", call, blobs...)
	case "file_change":
		if !completed {
			return
		}
		var paths []string
		for _, c := range it.Changes {
			paths = append(paths, RelPath(c.Path))
		}
		in, _ := json.Marshal(map[string]any{"changes": it.Changes})
		key := id("tool")
		state := ToolCompleted
		if it.Status == "failed" {
			state = ToolFailed
		}
		p.write(key, KindToolCall, "", ToolCall{CallID: it.ID, Name: "edit", Input: in,
			Summary: Clip(strings.Join(paths, ", ")), State: state})
		if state == ToolFailed {
			return
		}
		for i, c := range it.Changes {
			p.write("diff:"+strings.TrimPrefix(key, "tool:")+"."+strconv.Itoa(i), KindDiff, "",
				Diff{CallID: it.ID, Path: RelPath(c.Path), Change: c.Kind})
		}
	case "todo_list":
		pl := Plan{Entries: []PlanEntry{}}
		for _, t := range it.Items {
			st := "pending"
			if t.Completed {
				st = "completed"
			}
			pl.Entries = append(pl.Entries, PlanEntry{Text: t.Text, Status: st})
		}
		p.write("plan", KindPlan, "", pl)
	case "error":
		p.status()
	}
}

// ---------------------------------------------------------------- anthropic

// anthropic reads a Messages API response: one whole turn, whose only tool
// call is the forced submit.
func (p *projector) anthropic() {
	var m struct {
		Content    []block         `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(p.ev.Payload, &m) != nil {
		return
	}
	p.openTurn()
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				p.write(p.local("message"), KindMessage, "", Message{Role: "assistant", Text: b.Text})
			}
		case "thinking":
			t, blob := Bound([]byte(b.Thinking))
			p.write(p.local("thinking"), KindThinking, "", Thinking{Text: t, TokensEstimate: estimateTokens(b.Thinking)}, blob)
		case "tool_use":
			p.toolUse(b, "")
		}
	}
	outcome := "success"
	switch m.StopReason {
	case "", "end_turn", "tool_use", "stop_sequence":
	case "max_tokens":
		outcome = "max_tokens"
	case "refusal":
		outcome = "refused"
	default:
		outcome = m.StopReason
	}
	p.closeTurn(outcome, "", nil, nil, m.Usage)
}
