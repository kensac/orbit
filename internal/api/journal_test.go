package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
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
