package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
)

func TestTheJournalIsReadByCursor(t *testing.T) {
	v := newEnv(t, nil)
	run := enqueue(t, v)
	_, attempt := claim(t, v, "claude-code")
	big := strings.Repeat("x", 9000)
	appendEvents(t, v, run, attempt,
		`{"type":"system","subtype":"init"}`,
		assistantText("Reading."),
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"/data/runs/`+run+`-1/inputs/a.json"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"r1","content":"`+big+`"}]}}`)
	base := v.internal.URL + "/api/v1/runs/" + run + "/journal"

	code, all := get(t, base, token)
	items, _ := all["items"].([]any)
	if code != 200 || all["epoch"] != float64(1) || len(items) != 5 || all["cursor"] != "1.5" || all["has_more"] != false || all["reset"] != nil {
		t.Fatalf("journal = %d %v", code, all)
	}
	tool := items[4].(map[string]any)
	b := tool["body"].(map[string]any)
	res := b["result"].(map[string]any)
	if tool["item_id"] != "tool:r1" || tool["revision"] != float64(2) || tool["cursor"] != "1.5" || b["summary"] != "inputs/a.json" ||
		res["truncated"] != true || res["bytes"] != float64(9000) {
		t.Fatalf("tool = %v", tool)
	}

	_, page := get(t, base+"?after=1.3&limit=1", token)
	if p := page["items"].([]any); len(p) != 1 || p[0].(map[string]any)["seq"] != float64(4) || page["has_more"] != true || page["cursor"] != "1.4" {
		t.Fatalf("page = %v", page)
	}
	// A cursor from another timeline is a reset, from the start.
	_, reset := get(t, base+"?after=7.2", token)
	if reset["reset"] != "epoch_changed" || len(reset["items"].([]any)) != 5 {
		t.Fatalf("reset = %v", reset)
	}
	if code, _ := get(t, base+"?after=1.2.0", token); code != 400 {
		t.Fatalf("an old stream id = %d", code)
	}

	_, it := get(t, base+"/items/tool:r1", token)
	if it["item"].(map[string]any)["revision"] != float64(2) {
		t.Fatalf("item = %v", it)
	}
	if code, _ := get(t, base+"/items/nope", token); code != 404 {
		t.Fatalf("unknown item = %d", code)
	}

	req, _ := http.NewRequest("GET", base+"/bodies/"+res["sha256"].(string), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	full, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(full) != big || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("body = %d %d bytes %s", resp.StatusCode, len(full), resp.Header.Get("Content-Type"))
	}
	if code, _ := get(t, v.internal.URL+"/api/v1/runs/"+enqueue(t, v)+"/journal/bodies/"+res["sha256"].(string), token); code != 404 {
		t.Fatalf("another run's body = %d", code)
	}
	if code, _ := get(t, v.internal.URL+"/api/v1/runs/not-a-run/journal", token); code != 400 {
		t.Fatalf("bad id = %d", code)
	}
}

func TestTheStreamSendsJournalRows(t *testing.T) {
	v := newEnv(t, fastStreams)
	run := enqueue(t, v)
	_, attempt := claim(t, v, "claude-code")
	appendEvents(t, v, run, attempt,
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"plan it"},{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b1","content":"a.json"}]}}`,
		assistantText("Done."),
		`{"type":"result","subtype":"success","duration_ms":7}`)
	if err := v.s.FinishRun(ctx, run, store.RunResult{Attempt: attempt, Status: "succeeded", Report: "r"}); err != nil {
		t.Fatal(err)
	}
	collect := func(lastID string) (items []map[string]any, events []string, done map[string]any) {
		ch, _ := resumeStream(t, v, run, lastID)
		for e := range ch {
			switch e.Event {
			case "item":
				var it map[string]any
				_ = json.Unmarshal([]byte(e.Data), &it)
				if it["cursor"] != e.ID {
					t.Fatalf("item id %s, cursor %v", e.ID, it["cursor"])
				}
				items = append(items, it)
			case "done":
				_ = json.Unmarshal([]byte(e.Data), &done)
			}
			if e.Event != "ping" {
				events = append(events, e.Event)
			}
		}
		return
	}
	items, events, done := collect("")
	want := "status agent_status item item item tool item item text item item done"
	if strings.Join(events, " ") != want {
		t.Fatalf("events = %v", events)
	}
	if items[0]["kind"] != "turn" || items[2]["kind"] != "thinking" || items[4]["revision"] != float64(2) || len(items) != 7 {
		t.Fatalf("items = %v", items)
	}
	turn, _ := done["turn"].(map[string]any)
	if turn["state"] != "completed" || turn["outcome"] != "success" || done["steps"] != float64(1) {
		t.Fatalf("done = %v", done)
	}

	// Resuming sends only the rows after the one named.
	after, _, _ := collect(items[3]["cursor"].(string))
	if len(after) != 3 || after[0]["seq"] != float64(5) {
		t.Fatalf("resumed = %v", after)
	}
	// An id of the earlier (attempt.seq.index) form is no cursor: a full replay.
	if old, _, _ := collect("1.2.0"); len(old) != 7 {
		t.Fatalf("old id replay = %d rows", len(old))
	}

	// The run page shows the journal.
	req, _ := http.NewRequest("GET", v.srv.URL+"/ui/runs/"+run, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, s := range []string{"Bash ls (completed)", "about 2 tokens", "completed, success", "Done."} {
		if !strings.Contains(string(page), s) {
			t.Fatalf("run page lacks %q:\n%s", s, page)
		}
	}
}
