package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/store"
)

// ------------------------------------------------------------ helpers

func specSHA(v *env) string { return v.e.Current().Config.Agents["a"].SHA }

// claim enqueues (when id is "") or reclaims a run and returns its id and
// attempt.
func claim(t *testing.T, v *env, kind string) (string, int) {
	t.Helper()
	c, err := v.s.ClaimRun(ctx, "w")
	if err != nil || c == nil {
		t.Fatalf("claim: %+v %v", c, err)
	}
	if err := v.s.SetRunner(ctx, c.ID, c.Attempt, kind, "", specSHA(v)); err != nil {
		t.Fatal(err)
	}
	return c.ID, c.Attempt
}

func enqueue(t *testing.T, v *env) string {
	t.Helper()
	id, err := v.s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: specSHA(v), Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func appendEvents(t *testing.T, v *env, id string, attempt int, lines ...string) {
	t.Helper()
	var n int
	_ = v.s.Pool.QueryRow(ctx, `SELECT count(*) FROM run_events WHERE run_id = $1 AND attempt = $2`, id, attempt).Scan(&n)
	for i, l := range lines {
		kind := "assistant"
		var h struct{ Type, Subtype string }
		_ = json.Unmarshal([]byte(l), &h)
		if h.Type != "" {
			kind = h.Type
			if h.Subtype != "" {
				kind += "." + h.Subtype
			}
		}
		if err := v.s.AppendEvent(ctx, id, attempt, n+i+1, kind, []byte(l)); err != nil {
			t.Fatal(err)
		}
	}
}

func assistantText(s string) string {
	b, _ := json.Marshal(s)
	return `{"type":"assistant","message":{"content":[{"type":"text","text":` + string(b) + `}]}}`
}

func assistantTool(name, input string) string {
	return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"` + name + `","input":` + input + `}]}}`
}

func get(t *testing.T, u, tok string) (int, map[string]any) {
	t.Helper()
	return call(t, "GET", u, tok, "")
}

// ------------------------------------------------------------ auth

func TestPublicListenerTakesAuthentikTokensOnly(t *testing.T) {
	v := newEnv(t, nil)
	code, body := get(t, v.srv.URL+"/api/v1/agents", token)
	if code != 401 || body["check"] != "token" || !strings.Contains(body["error"].(string), "internal listener") {
		t.Fatalf("operator token on the public listener = %d %v", code, body)
	}
	if code, _ := get(t, v.srv.URL+"/api/v1/agents", v.person("kanishk")); code != 200 {
		t.Fatalf("person = %d", code)
	}
	if code, _ := get(t, v.internal.URL+"/api/v1/agents", v.person("kanishk")); code != 200 {
		t.Fatalf("person on the internal listener = %d", code)
	}
	wrongAud := v.is.Valid("someone-else", group, "kanishk")
	if code, body := get(t, v.srv.URL+"/api/v1/agents", v.is.Mint(wrongAud)); code != 401 || body["check"] != "aud" {
		t.Fatalf("wrong aud = %d %v", code, body)
	}
	noGroup := v.is.Valid(clientID, "users", "kanishk")
	if code, body := get(t, v.srv.URL+"/api/v1/agents", v.is.Mint(noGroup)); code != 401 || body["check"] != "groups" {
		t.Fatalf("no group = %d %v", code, body)
	}
	if code, body := get(t, v.srv.URL+"/api/v1/agents", ""); code != 401 || body["check"] != "token" {
		t.Fatalf("no token = %d %v", code, body)
	}
	// /ui is not on the internal listener at all.
	if resp, _ := http.Get(v.internal.URL + "/ui/runs/x"); resp.StatusCode != 404 {
		t.Fatalf("internal /ui = %d", resp.StatusCode)
	}
}

func TestThePersonAuthorsAndDecides(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	code, body := call(t, "POST", v.srv.URL+"/api/v1/agents/a/messages", kanishk, `{"body":"hi","author":"mallory","wake":false}`)
	if code != 201 {
		t.Fatalf("send = %d %v", code, body)
	}
	msgs, _ := v.s.ListMessages(ctx, "a", 0)
	if len(msgs) != 1 || msgs[0].Author == nil || *msgs[0].Author != "kanishk" {
		t.Fatalf("author = %+v, want the token's person", msgs)
	}

	enqueue(t, v)
	id, _ := claim(t, v, "claude-code")
	ids, err := v.s.RecordOutcome(ctx, id, "a", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{
			{Type: "notify", Params: json.RawMessage(`{}`), Rule: "approve"},
			{Type: "notify", Params: json.RawMessage(`{}`), Rule: "approve"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	code, body = call(t, "POST", v.srv.URL+"/api/v1/actions/"+ids[0]+"/approve", kanishk, `{"by":"mallory"}`)
	if code != 200 || body["ok"] != true {
		t.Fatalf("approve = %d %v", code, body)
	}
	if a, _ := v.s.GetAction(ctx, ids[0]); *a.DecidedBy != "kanishk" {
		t.Fatalf("decided_by = %s", *a.DecidedBy)
	}
	// The operator token names whoever its script says, or "operator".
	if code, _ := call(t, "POST", v.internal.URL+"/api/v1/actions/"+ids[1]+"/reject", token, ""); code != 200 {
		t.Fatalf("operator reject = %d", code)
	}
	if a, _ := v.s.GetAction(ctx, ids[1]); a.Status != "rejected" || *a.DecidedBy != "operator" {
		t.Fatalf("operator decision = %s by %s", a.Status, *a.DecidedBy)
	}
}

// ------------------------------------------------------------ thread

type threadPage struct {
	Items []struct {
		Kind       string     `json:"kind"`
		ID         string     `json:"id"`
		At         time.Time  `json:"at"`
		From       string     `json:"from"`
		Body       string     `json:"body"`
		Author     *string    `json:"author"`
		RunID      *string    `json:"run_id"`
		Trigger    string     `json:"trigger"`
		Status     string     `json:"status"`
		Steps      int        `json:"steps"`
		StartedAt  *time.Time `json:"started_at"`
		Finished   *time.Time `json:"finished_at"`
		Report     *string    `json:"report"`
		Open       *bool      `json:"open"`
		NeedsInput bool       `json:"needs_input"`
		Questions  []struct {
			ID   string `json:"id"`
			Body string `json:"body"`
			Open bool   `json:"open"`
		} `json:"questions"`
		Actions []struct {
			ID     string `json:"id"`
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"actions"`
	} `json:"items"`
	HasMore    bool    `json:"has_more"`
	NextBefore *string `json:"next_before"`
}

func thread(t *testing.T, v *env, query string) threadPage {
	t.Helper()
	req, _ := http.NewRequest("GET", v.srv.URL+"/api/v1/agents/a/thread"+query, nil)
	req.Header.Set("Authorization", "Bearer "+v.person("kanishk"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("thread%s = %d", query, resp.StatusCode)
	}
	var p threadPage
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestThread(t *testing.T) {
	v := newEnv(t, nil)
	msg, err := v.s.SendToAgent(ctx, "a", "How much on Uber Eats?", "kanishk", nil)
	if err != nil {
		t.Fatal(err)
	}
	enqueue(t, v)
	run, attempt := claim(t, v, "claude-code")
	if _, err := v.s.StartRunContext(ctx, run, "a"); err != nil {
		t.Fatal(err)
	}
	appendEvents(t, v, run, attempt,
		assistantTool("Read", `{"file_path":"/data/runs/x-1/inputs/transactions.json"}`),
		assistantText("Summing."),
		assistantTool("StructuredOutput", `{}`))
	if _, err := v.s.RecordOutcome(ctx, run, "a", store.RunResult{Attempt: 1, Status: "succeeded", Summary: "s", Report: "$84"}, store.Outcome{
		Questions: []string{"Was the 12th a refund?", "Count tips?"},
		Actions:   []store.ProposedAction{{Type: "notify", Params: json.RawMessage(`{"channel":"c"}`), Reason: "r", Rule: "approve"}},
	}); err != nil {
		t.Fatal(err)
	}
	queued := enqueue(t, v)

	p := thread(t, v, "")
	kinds := []string{}
	for _, it := range p.Items {
		kinds = append(kinds, it.Kind+":"+it.From)
	}
	if strings.Join(kinds, ",") != "message:person,run:,message:agent,message:agent,run:" || p.HasMore || p.NextBefore != nil {
		t.Fatalf("thread = %v has_more %v", kinds, p.HasMore)
	}
	m, r := p.Items[0], p.Items[1]
	if m.ID != msg || m.Body != "How much on Uber Eats?" || *m.Author != "kanishk" || m.RunID == nil || *m.RunID != run {
		t.Fatalf("message item = %+v", m)
	}
	if r.ID != run || r.Status != "succeeded" || r.Trigger != "manual" || *r.Report != "$84" || r.Steps != 1 ||
		r.StartedAt == nil || r.Finished == nil || len(r.Actions) != 1 || r.Actions[0].Status != "pending" {
		t.Fatalf("run item = %+v", r)
	}
	// Each question carries its message's id, in the order asked.
	if len(r.Questions) != 2 || r.Questions[0].ID != p.Items[2].ID || r.Questions[1].ID != p.Items[3].ID ||
		r.Questions[0].Body != "Was the 12th a refund?" || *p.Items[2].RunID != run {
		t.Fatalf("questions = %+v vs %+v %+v", r.Questions, p.Items[2], p.Items[3])
	}
	if last := p.Items[4]; last.ID != queued || last.Status != "queued" || last.StartedAt != nil || last.Questions == nil || last.Actions == nil {
		t.Fatalf("queued run = %+v", last)
	}

	if !r.NeedsInput || !r.Questions[0].Open || p.Items[2].Open == nil || !*p.Items[2].Open || p.Items[4].NeedsInput {
		t.Fatalf("needs_input = %v, open = %v", r.NeedsInput, r.Questions[0].Open)
	}
	if _, body := get(t, v.srv.URL+"/api/v1/agents", v.person("kanishk")); body["agents"].([]any)[0].(map[string]any)["needs_input"] != true {
		t.Fatalf("agents needs_input = %v", body)
	}
	// A reply closes the questions; the pending action still waits.
	if _, err := v.s.SendToAgent(ctx, "a", "yes, a refund", "kanishk", &r.Questions[0].ID); err != nil {
		t.Fatal(err)
	}
	p = thread(t, v, "")
	if r := p.Items[1]; r.Questions[0].Open || r.Questions[1].Open || !r.NeedsInput {
		t.Fatalf("after reply: %+v", r)
	}

	if code, _ := get(t, v.srv.URL+"/api/v1/agents/nope/thread", v.person("kanishk")); code != 404 {
		t.Fatalf("unknown agent = %d", code)
	}
	if code, _ := get(t, v.srv.URL+"/api/v1/agents/a/thread?before=garbage", v.person("kanishk")); code != 400 {
		t.Fatalf("bad cursor = %d", code)
	}
}

func TestThreadPagingNeverSkipsOrRepeats(t *testing.T) {
	v := newEnv(t, nil)
	for i := 0; i < 7; i++ {
		if _, err := v.s.SendToAgent(ctx, "a", fmt.Sprint("m", i), "kanishk", nil); err != nil {
			t.Fatal(err)
		}
		enqueue(t, v)
	}
	// Ties: give five messages and three runs one timestamp, the case a
	// time-only cursor gets wrong.
	if _, err := v.s.Pool.Exec(ctx, `
		UPDATE messages SET created_at = '2026-11-01T12:00:00Z' WHERE id IN (SELECT id FROM messages ORDER BY created_at LIMIT 5);
		UPDATE runs SET created_at = '2026-11-01T12:00:00Z' WHERE id IN (SELECT id FROM runs ORDER BY created_at LIMIT 3)`); err != nil {
		t.Fatal(err)
	}
	all := thread(t, v, "?limit=200")
	if len(all.Items) != 14 {
		t.Fatalf("all = %d items", len(all.Items))
	}
	for _, limit := range []int{1, 2, 3, 5} {
		var got []string
		q := fmt.Sprintf("?limit=%d", limit)
		for pages := 0; ; pages++ {
			p := thread(t, v, q)
			if len(p.Items) > limit {
				t.Fatalf("page of %d with limit %d", len(p.Items), limit)
			}
			ids := []string{}
			for _, it := range p.Items {
				ids = append(ids, it.ID)
			}
			got = append(ids, got...)
			if !p.HasMore {
				break
			}
			q = fmt.Sprintf("?limit=%d&before=%s", limit, *p.NextBefore)
		}
		var want []string
		for _, it := range all.Items {
			want = append(want, it.ID)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("limit %d: paged\n%v\nwant\n%v", limit, got, want)
		}
	}
}

// ------------------------------------------------------------ stream

type sse struct {
	ID    string
	Event string
	Data  string
}

// openStream reads a run's stream into a channel; "ping" is a heartbeat
// comment. The channel closes when the stream ends.
func openStream(t *testing.T, v *env, run string) (<-chan sse, context.CancelFunc) {
	t.Helper()
	return resumeStream(t, v, run, "")
}

func resumeStream(t *testing.T, v *env, run, lastID string) (<-chan sse, context.CancelFunc) {
	t.Helper()
	return openSSE(t, v, "/api/v1/runs/"+run+"/stream", lastID)
}

// openSSE reads the Server-Sent Events at path as the person kanishk.
func openSSE(t *testing.T, v *env, path, lastID string) (<-chan sse, context.CancelFunc) {
	t.Helper()
	c, cancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(c, "GET", v.srv.URL+path, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	req.Header.Set("Authorization", "Bearer "+v.person("kanishk"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ch := make(chan sse, 100)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var cur sse
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == ": ping":
				ch <- sse{Event: "ping"}
			case strings.HasPrefix(line, "id: "):
				cur.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.Data = strings.TrimPrefix(line, "data: ")
			case line == "" && cur.Event != "":
				ch <- cur
				cur = sse{}
			}
		}
	}()
	t.Cleanup(cancel)
	return ch, cancel
}

// next returns the next event that is neither a ping nor an agent_status
// (TestStreamAgentStatus reads those).
func next(t *testing.T, ch <-chan sse) sse {
	t.Helper()
	return nextEvent(t, ch, true)
}

func nextEvent(t *testing.T, ch <-chan sse, skipAgentStatus bool) sse {
	t.Helper()
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatal("stream ended early")
			}
			// item events are the journal rows; tests of them read them
			// with nextItem. agent_status events are read by expect only
			// when asked for.
			if e.Event != "ping" && e.Event != "item" && (!skipAgentStatus || e.Event != "agent_status") {
				return e
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no event within 5s")
		}
	}
}

func expect(t *testing.T, ch <-chan sse, event, data string) sse {
	t.Helper()
	e := nextEvent(t, ch, event != "agent_status")
	if e.Event != event || (data != "" && e.Data != data) {
		t.Fatalf("got %s %s, want %s %s", e.Event, e.Data, event, data)
	}
	return e
}

func fastStreams(a *api.Server, hs *http.Server) {
	a.StreamPoll, a.StreamPing = 20*time.Millisecond, 100*time.Millisecond
	// Far shorter than the stream lives: the stream must clear it.
	hs.WriteTimeout = 300 * time.Millisecond
}

func TestStreamFollowsARunThroughARequeue(t *testing.T) {
	v := newEnv(t, fastStreams)
	run := enqueue(t, v)
	ch, _ := openStream(t, v, run)
	expect(t, ch, "status", `{"attempt":0,"status":"queued"}`)

	_, a1 := claim(t, v, "claude-code")
	expect(t, ch, "status", `{"attempt":1,"epoch":1,"status":"running"}`)
	appendEvents(t, v, run, a1, assistantText("first try"))
	firstTry := expect(t, ch, "text", `{"text":"first try"}`)
	if firstTry.ID != "1.2" {
		t.Fatalf("event id = %q", firstTry.ID)
	}

	// A deploy requeues it; the next claim is a new attempt, streamed from
	// its own start.
	if err := v.s.HandBack(ctx, run, 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	expect(t, ch, "status", `{"attempt":1,"epoch":1,"status":"queued"}`)
	_, a2 := claim(t, v, "claude-code")
	// As the engine does before an attempt's first event: it starts over.
	if err := v.s.SetRecovery(ctx, run, a2, 0, "the interrupted attempt's transcript is gone"); err != nil {
		t.Fatal(err)
	}
	expect(t, ch, "status", `{"attempt":2,"epoch":2,"status":"running"}`)
	appendEvents(t, v, run, a2,
		`{"type":"system","subtype":"init"}`,
		assistantText("Reading."),
		assistantTool("Read", `{"file_path":"/data/runs/`+run+`-2/inputs/transactions.json"}`),
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"..."}]}}`,
		assistantText("Done."))
	expect(t, ch, "text", `{"text":"Reading."}`)
	expect(t, ch, "tool", `{"name":"Read","summary":"inputs/transactions.json"}`)
	expect(t, ch, "text", `{"text":"\n\nDone."}`)

	// Past the server's write timeout, the stream is still alive and
	// heartbeating.
	deadline := time.After(time.Second)
	pings := 0
wait:
	for {
		select {
		case e := <-ch:
			if e.Event == "item" {
				continue // the rows the text and tool events came from
			}
			if e.Event != "ping" {
				t.Fatalf("unexpected %v", e)
			}
			pings++
		case <-deadline:
			break wait
		}
	}
	if pings < 3 {
		t.Fatalf("pings in 1s = %d", pings)
	}

	if _, err := v.s.RecordOutcome(ctx, run, "a", store.RunResult{Attempt: a2, Status: "succeeded", Report: "r"},
		store.Outcome{Questions: []string{"q?"}}); err != nil {
		t.Fatal(err)
	}
	expect(t, ch, "status", `{"attempt":2,"epoch":2,"status":"succeeded"}`)
	done := next(t, ch)
	var d struct {
		Kind      string `json:"kind"`
		ID        string `json:"id"`
		Status    string `json:"status"`
		Steps     int    `json:"steps"`
		Report    string `json:"report"`
		Questions []struct{ ID, Body string }
	}
	if done.Event != "done" || json.Unmarshal([]byte(done.Data), &d) != nil || d.Kind != "run" || d.ID != run ||
		d.Status != "succeeded" || d.Steps != 1 || d.Report != "r" || len(d.Questions) != 1 {
		t.Fatalf("done = %v", done)
	}
	// done is a thread run item, byte for byte.
	p := thread(t, v, "")
	for _, it := range p.Items {
		if it.Kind == "run" && it.ID == run {
			b, _ := json.Marshal(mustThreadRun(t, v, run))
			if string(b) != done.Data {
				t.Fatalf("done %s\n!= thread item %s", done.Data, b)
			}
		}
	}
	if _, ok := <-ch; ok {
		t.Fatal("stream continued after done")
	}

	// A late client replays the final attempt, then gets done at once.
	late, _ := openStream(t, v, run)
	expect(t, late, "status", `{"attempt":2,"epoch":2,"status":"succeeded"}`)
	expect(t, late, "text", `{"text":"Reading."}`)
	tool := expect(t, late, "tool", "")
	expect(t, late, "text", `{"text":"\n\nDone."}`)
	expect(t, late, "done", done.Data)

	// Resuming after the tool sends only what followed it, with the same
	// separator a full replay has.
	resumed, _ := resumeStream(t, v, run, tool.ID)
	expect(t, resumed, "status", `{"attempt":2,"epoch":2,"status":"succeeded"}`)
	expect(t, resumed, "text", `{"text":"\n\nDone."}`)
	expect(t, resumed, "done", done.Data)

	// An id from the abandoned attempt replays the latest one in full.
	stale, _ := resumeStream(t, v, run, firstTry.ID)
	expect(t, stale, "status", "")
	expect(t, stale, "text", `{"text":"Reading."}`)
}

func mustThreadRun(t *testing.T, v *env, id string) *store.ThreadRun {
	t.Helper()
	r, err := v.s.ThreadRunByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestStreamEndsWhenTheClientLeaves(t *testing.T) {
	v := newEnv(t, fastStreams)
	run := enqueue(t, v)
	ch, cancel := openStream(t, v, run)
	expect(t, ch, "status", "")
	cancel()
	for range ch {
	}
	// The handler returned: closing the server does not wait on it (the
	// cleanup's Close would hang otherwise).
	done := make(chan struct{})
	go func() { v.srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream handler still running after the client left")
	}
	if code, _ := get(t, v.internal.URL+"/api/v1/runs/00000000-0000-0000-0000-000000000000/stream", token); code != 404 {
		t.Fatalf("unknown run = %d", code)
	}
}

// TestStepsMatchTheStream holds the store's step count to the stream:
// a run's steps are exactly the tool events its stream shows.
func TestStepsMatchTheStream(t *testing.T) {
	v := newEnv(t, fastStreams)
	cases := map[string][]string{
		"claude-code": {
			`{"type":"system","subtype":"init"}`,
			`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"a"},` +
				`{"type":"tool_use","name":"Glob","input":{"pattern":"*.json"}},{"type":"tool_use","name":"Grep","input":{"pattern":"x","path":"/data/runs/r-1/inputs"}}]}}`,
			assistantTool("Bash", `{"command":"ls\nwc -l"}`),
			assistantTool("StructuredOutput", `{"summary":"s"}`),
			`{"type":"assistant","message":{"content":"not an array"}}`,
			`{"type":"result","subtype":"success"}`,
		},
		"codex": {
			`{"type":"thread.started","thread_id":"t"}`,
			`{"type":"item.completed","item":{"type":"reasoning","text":"thinking it over"}}`,
			`{"type":"item.started","item":{"type":"command_execution","command":"cat inputs/a.json"}}`,
			`{"type":"item.completed","item":{"type":"command_execution","command":"cat inputs/a.json"}}`,
			`{"type":"item.started","item":{"type":"web_search","query":"x"}}`,
			`{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"/data/runs/r-1/notes.md"}]}}`,
			`{"type":"item.completed","item":{"type":"agent_message","text":"{\"summary\":\"s\"}"}}`,
		},
	}
	for kind, lines := range cases {
		run := enqueue(t, v)
		_, attempt := claim(t, v, kind)
		appendEvents(t, v, run, attempt, lines...)
		if err := v.s.FinishRun(ctx, run, store.RunResult{Attempt: 1, Status: "succeeded"}); err != nil {
			t.Fatal(err)
		}
		ch, _ := openStream(t, v, run)
		tools := 0
		for e := range ch {
			switch e.Event {
			case "tool":
				tools++
			case "done":
				var d struct{ Steps int }
				_ = json.Unmarshal([]byte(e.Data), &d)
				if d.Steps != tools || tools == 0 {
					t.Fatalf("%s: steps %d, stream showed %d tools", kind, d.Steps, tools)
				}
			}
		}
		// The runs list and a run's detail count the same steps.
		_, got := call(t, "GET", v.internal.URL+"/api/v1/runs/"+run, token, "")
		if n := got["run"].(map[string]any)["steps"]; n != float64(tools) {
			t.Fatalf("%s: GET /runs/{id} steps = %v, want %d", kind, n, tools)
		}
		_, list := call(t, "GET", v.internal.URL+"/api/v1/runs?agent=a", token, "")
		for _, it := range list["runs"].([]any) {
			if r := it.(map[string]any); r["id"] == run && r["steps"] != float64(tools) {
				t.Fatalf("%s: GET /runs steps = %v, want %d", kind, r["steps"], tools)
			}
		}
	}
}

// TestActionsFilterByRunAndPage: run_id narrows the list, and walking
// next_before visits every action once, newest first.
func TestActionsFilterByRunAndPage(t *testing.T) {
	v := newEnv(t, nil)
	var runs []string
	for range 2 {
		run := enqueue(t, v)
		_, attempt := claim(t, v, "claude-code")
		acts := make([]store.ProposedAction, 3)
		for i := range acts {
			acts[i] = store.ProposedAction{Type: "notify", Params: json.RawMessage(`{}`), Reason: "r", Rule: "approve"}
		}
		if _, err := v.s.RecordOutcome(ctx, run, "a", store.RunResult{Attempt: attempt, Status: "succeeded"},
			store.Outcome{Actions: acts}); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	_, one := call(t, "GET", v.internal.URL+"/api/v1/actions?run_id="+runs[0], token, "")
	if got := one["actions"].([]any); len(got) != 3 || one["has_more"] != false || one["next_before"] != nil {
		t.Fatalf("run_id filter = %v", one)
	}
	for _, a := range one["actions"].([]any) {
		if a.(map[string]any)["run_id"] != runs[0] {
			t.Fatalf("action of another run: %v", a)
		}
	}
	if code, _ := call(t, "GET", v.internal.URL+"/api/v1/actions?run_id=nope", token, ""); code != 400 {
		t.Fatalf("bad run_id = %d", code)
	}
	if code, _ := call(t, "GET", v.internal.URL+"/api/v1/actions?before=nope", token, ""); code != 400 {
		t.Fatalf("bad cursor = %d", code)
	}
	seen := map[string]bool{}
	before := ""
	for pages := 0; ; pages++ {
		u := v.internal.URL + "/api/v1/actions?limit=4"
		if before != "" {
			u += "&before=" + before
		}
		_, page := call(t, "GET", u, token, "")
		for _, a := range page["actions"].([]any) {
			id := a.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("action %s repeated", id)
			}
			seen[id] = true
		}
		if page["has_more"] == false {
			if page["next_before"] != nil || pages != 1 {
				t.Fatalf("last page = %v after %d pages", page, pages)
			}
			break
		}
		before = page["next_before"].(string)
	}
	if len(seen) != 6 {
		t.Fatalf("saw %d actions, want 6", len(seen))
	}
}

// ------------------------------------------------------------ idempotency

func TestRetriedSendsActOnce(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	send := `{"body":"the $500 was a deductible","wake":true,"client_message_id":"c-1"}`
	code, first := call(t, "POST", v.srv.URL+"/api/v1/agents/a/messages", kanishk, send)
	if code != 201 || first["run_id"] == nil {
		t.Fatalf("first = %d %v", code, first)
	}
	code, again := call(t, "POST", v.srv.URL+"/api/v1/agents/a/messages", kanishk, send)
	if code != 201 || again["message_id"] != first["message_id"] || again["run_id"] != first["run_id"] {
		t.Fatalf("retry = %d %v, want %v", code, again, first)
	}
	msgs, _ := v.s.ListMessages(ctx, "a", 0)
	runs, _ := v.s.ListRuns(ctx, store.RunFilter{Agent: "a"})
	if len(msgs) != 1 || len(runs) != 1 {
		t.Fatalf("messages %d runs %d, want one of each", len(msgs), len(runs))
	}
	if code, _ := call(t, "POST", v.srv.URL+"/api/v1/agents/a/messages", kanishk,
		`{"body":"something else","client_message_id":"c-1"}`); code != 409 {
		t.Fatalf("reused id for another body = %d", code)
	}

	id, _ := claim(t, v, "claude-code")
	ids, err := v.s.RecordOutcome(ctx, id, "a", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{{Type: "notify", Params: json.RawMessage(`{}`), Rule: "approve"}}})
	if err != nil {
		t.Fatal(err)
	}
	approve := `{"client_request_id":"d-1"}`
	for i := 0; i < 2; i++ {
		if code, body := call(t, "POST", v.srv.URL+"/api/v1/actions/"+ids[0]+"/approve", kanishk, approve); code != 200 || body["ok"] != true {
			t.Fatalf("approve %d = %d %v", i, code, body)
		}
	}
	if code, _ := call(t, "POST", v.srv.URL+"/api/v1/actions/"+ids[0]+"/approve", kanishk, ""); code != 409 {
		t.Fatalf("approve without an id after a decision = %d", code)
	}
}

// A stream sends the agent's status with every status, and on its own when
// the run's hooks change it; done carries it too.
func TestStreamAgentStatus(t *testing.T) {
	v := newEnv(t, fastStreams)
	run := enqueue(t, v)
	ch, _ := openStream(t, v, run)
	agentStatus := func(want string) {
		t.Helper()
		e := expect(t, ch, "agent_status", "")
		var st struct {
			State, Kind, Detail, Source string
			RunID                       string `json:"run_id"`
		}
		_ = json.Unmarshal([]byte(e.Data), &st)
		if st.RunID != run {
			t.Fatalf("agent_status run_id = %q", st.RunID)
		}
		if got := st.State + "/" + st.Kind + "/" + st.Detail + "/" + st.Source; got != want {
			t.Fatalf("agent_status = %s, want %s (%s)", got, want, e.Data)
		}
	}
	expect(t, ch, "status", `{"attempt":0,"status":"queued"}`)
	agentStatus("queued///run")
	_, a1 := claim(t, v, "claude-code")
	expect(t, ch, "status", `{"attempt":1,"epoch":1,"status":"running"}`)
	agentStatus("working///run")
	snap := `{"state":"waiting","kind":"permission","detail":"Write","event":"PermissionRequest","at":"2026-11-01T12:00:00Z","attempt":1}`
	if err := v.s.SetHookStatus(ctx, run, a1, json.RawMessage(snap)); err != nil {
		t.Fatal(err)
	}
	agentStatus("waiting/permission/Write/hooks")
	if _, err := v.s.RecordOutcome(ctx, run, "a", store.RunResult{Attempt: a1, Status: "succeeded", Report: "r"},
		store.Outcome{Questions: []string{"q?"}}); err != nil {
		t.Fatal(err)
	}
	expect(t, ch, "status", `{"attempt":1,"epoch":1,"status":"succeeded"}`)
	agentStatus("waiting/question//run")
	done := expect(t, ch, "done", "")
	if !strings.Contains(done.Data, `"agent_status":{"state":"waiting","kind":"question"`) {
		t.Fatalf("done = %s", done.Data)
	}
	if err := v.s.SetHookStatus(ctx, run, a1, json.RawMessage(snap)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a finished run's hook status: %v", err)
	}
}
