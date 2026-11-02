package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/store"
)

func boardRows(t *testing.T, v *env, me, query string) map[string]map[string]any {
	t.Helper()
	code, body := call(t, "GET", v.srv.URL+"/api/v1/board"+query, me, "")
	if code != 200 || body["as_of"] == nil || body["idle_after_seconds"] != float64(1800) {
		t.Fatalf("board = %d %v", code, body)
	}
	out := map[string]map[string]any{}
	for _, r := range body["rows"].([]any) {
		row := r.(map[string]any)
		out[row["conversation_id"].(string)] = row
	}
	return out
}

func TestBoardReadPointsAndThread(t *testing.T) {
	v := newEnv(t, nil)
	me := v.person("kanishk")
	code, created := call(t, "POST", v.srv.URL+"/api/v1/conversations", me, `{"agent": "a", "body": "Summarize October"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, created)
	}
	cid := created["conversation_id"].(string)
	row := boardRows(t, v, me, "")[cid]
	if row == nil || row["state"] != "working" || row["agent"] != "a" || row["title"] != "Summarize October" ||
		row["preview"].(map[string]any)["from"] != "person" || row["counts"].(map[string]any)["unread"] != float64(0) || row["task"] != nil {
		t.Fatalf("queued row = %v", row)
	}
	run, attempt := claim(t, v, "claude-code")
	if err := v.s.FinishRun(ctx, run, store.RunResult{Attempt: attempt, Status: "succeeded", Summary: "October: $2,140", Report: "October: $2,140"}); err != nil {
		t.Fatal(err)
	}
	row = boardRows(t, v, me, "")[cid]
	if row["state"] != "done_unread" || row["last_run"].(map[string]any)["status"] != "succeeded" || row["preview"].(map[string]any)["text"] != "October: $2,140" {
		t.Fatalf("finished row = %v", row)
	}

	// The conversation list carries the last run and the read point.
	_, list := call(t, "GET", v.srv.URL+"/api/v1/agents/a/conversations", me, "")
	var conv map[string]any
	for _, c := range list["conversations"].([]any) {
		if c.(map[string]any)["id"] == cid {
			conv = c.(map[string]any)
		}
	}
	if conv == nil || conv["last_run"].(map[string]any)["id"] != run || conv["read_through"] != nil {
		t.Fatalf("conversation = %v", conv)
	}

	// A thread read without mark_read changes nothing; with it, the read
	// point moves and the response carries where it was.
	code, page := call(t, "GET", v.srv.URL+"/api/v1/conversations/"+cid+"/thread", me, "")
	if code != 200 || page["read_through"] != nil || len(page["items"].([]any)) != 2 {
		t.Fatalf("thread = %d %v", code, page)
	}
	if _, page := call(t, "GET", v.srv.URL+"/api/v1/conversations/"+cid+"/thread?mark_read=1", me, ""); page["read_through"] != nil {
		t.Fatalf("first marked read = %v, want the earlier (null) point", page["read_through"])
	}
	_, page = call(t, "GET", v.srv.URL+"/api/v1/conversations/"+cid+"/thread", me, "")
	if page["read_through"] == nil {
		t.Fatalf("after mark_read = %v", page)
	}
	if row := boardRows(t, v, me, "")[cid]; row["state"] != "read" {
		t.Fatalf("read row = %v", row)
	}

	// POST read takes an optional point; it never moves back.
	code, res := call(t, "POST", v.srv.URL+"/api/v1/conversations/"+cid+"/read", me, `{"through": "2001-01-01T00:00:00Z"}`)
	if code != 200 || res["read_through"] != page["read_through"] {
		t.Fatalf("read back = %d %v, want %v", code, res, page["read_through"])
	}
	if code, _ := call(t, "POST", v.srv.URL+"/api/v1/conversations/"+cid+"/read", me, ""); code != 200 {
		t.Fatalf("read now = %d", code)
	}
	if code, _ := call(t, "POST", v.srv.URL+"/api/v1/conversations/"+cid+"/read", me, `{"at": 1}`); code != 400 {
		t.Fatalf("an unknown field = %d", code)
	}
	if code, _ := call(t, "POST", v.srv.URL+"/api/v1/conversations/00000000-0000-0000-0000-000000000000/read", me, ""); code != 404 {
		t.Fatalf("an unknown conversation = %d", code)
	}
	if code, _ := call(t, "GET", v.srv.URL+"/api/v1/conversations/nope/thread", me, ""); code != 400 {
		t.Fatalf("a bad id = %d", code)
	}
	if code, _ := call(t, "GET", v.srv.URL+"/api/v1/agents/nope/thread", me, ""); code != 404 {
		t.Fatalf("an unknown agent's thread = %d", code)
	}

	// Quiet past board.idle: left out unless idle=1.
	if _, err := v.s.Pool.Exec(ctx, `UPDATE conversations SET last_activity_at = now() - interval '1 hour' WHERE id = $1`, cid); err != nil {
		t.Fatal(err)
	}
	if row := boardRows(t, v, me, "")[cid]; row != nil {
		t.Fatalf("idle row listed = %v", row)
	}
	if row := boardRows(t, v, me, "?idle=1")[cid]; row == nil || row["state"] != "idle" {
		t.Fatalf("idle=1 row = %v", row)
	}
}

func TestEventsFeedResumes(t *testing.T) {
	v := newEnv(t, func(a *api.Server, _ *http.Server) { a.StreamPoll = 20 * time.Millisecond })
	me := v.person("kanishk")
	next := func(ch <-chan sse, want string) sse {
		t.Helper()
		timeout := time.After(20 * time.Second)
		for {
			select {
			case e, ok := <-ch:
				if !ok {
					t.Fatalf("stream ended waiting for %s", want)
				}
				if e.Event == want {
					return e
				}
			case <-timeout:
				t.Fatalf("no %s event", want)
			}
		}
	}
	ch, cancel := openSSE(t, v, "/api/v1/events", "")
	ready := next(ch, "ready")
	var r struct {
		Cursor  string
		Resumed bool
	}
	if err := json.Unmarshal([]byte(ready.Data), &r); err != nil || r.Resumed || r.Cursor != ready.ID {
		t.Fatalf("ready = %+v %v", ready, err)
	}
	code, created := call(t, "POST", v.srv.URL+"/api/v1/conversations", me, `{"agent": "a", "body": "hi"}`)
	if code != 201 {
		t.Fatal(code)
	}
	cid := created["conversation_id"].(string)
	seen := map[string]bool{}
	var last sse
	for !seen["run"] || !seen["message"] || !seen["conversation"] {
		last = next(ch, "change")
		var c struct {
			Kind, Conversation, Cursor string
			Run                        *string
		}
		if err := json.Unmarshal([]byte(last.Data), &c); err != nil || c.Cursor != last.ID {
			t.Fatalf("change = %+v %v", last, err)
		}
		// The agent's default conversation, written before the stream
		// opened, may settle after it did (other transactions in a shared
		// test server hold the feed back); it is delivered, not skipped.
		if c.Conversation == cid {
			seen[c.Kind] = true
		}
	}
	cancel()

	// Resuming from the last id replays only what came after it.
	if code, _ := call(t, "POST", v.srv.URL+"/api/v1/conversations/"+cid+"/read", me, ""); code != 200 {
		t.Fatal(code)
	}
	ch, cancel = openSSE(t, v, "/api/v1/events", last.ID)
	defer cancel()
	if err := json.Unmarshal([]byte(next(ch, "ready").Data), &r); err != nil || !r.Resumed {
		t.Fatalf("resumed ready = %+v", r)
	}
	if e := next(ch, "change"); !json.Valid([]byte(e.Data)) || e.Data[:len(`{"kind":"conversation"`)] != `{"kind":"conversation"` {
		t.Fatalf("first change after resume = %+v, want the read", e)
	}

	// A cursor that is not one starts at the head.
	ch2, cancel2 := openSSE(t, v, "/api/v1/events?cursor=bogus", "")
	defer cancel2()
	if err := json.Unmarshal([]byte(next(ch2, "ready").Data), &r); err != nil || r.Resumed {
		t.Fatalf("bogus cursor ready = %+v", r)
	}
}
