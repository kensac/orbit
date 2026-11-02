package api_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
)

func TestSearchAPIAndTool(t *testing.T) {
	v := newEnv(t, nil)
	if _, err := v.s.SendToAgent(ctx, "a", "Is the dishwasher warranty still valid?", "kanishk", nil); err != nil {
		t.Fatal(err)
	}
	run, err := v.s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: v.e.Current().Config.Agents["a"].SHA, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.s.ClaimRun(ctx, "w")
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	if err := v.s.FinishRun(ctx, run, store.RunResult{Attempt: c.Attempt, Status: store.StatusSucceeded,
		Summary: "Dishwasher warranty runs to March", Report: "The warranty covers parts."}); err != nil {
		t.Fatal(err)
	}

	code, out := call(t, "GET", v.internal.URL+"/api/v1/search?q=warranty&kind=report", token, "")
	if code != 200 || out["total"] != 1.0 {
		t.Fatalf("search = %d %v", code, out)
	}
	hit := out["results"].([]any)[0].(map[string]any)
	links := hit["links"].(map[string]any)
	if hit["run_id"] != run || links["run"] != "/api/v1/runs/"+run || links["run_page"] != "http://orbit.test/ui/runs/"+run ||
		!strings.HasPrefix(links["conversation"].(string), "/api/v1/conversations/") {
		t.Fatalf("hit = %v", hit)
	}

	today := time.Now().In(time.UTC).Format(time.DateOnly)
	code, out = call(t, "GET", v.srv.URL+"/api/v1/search?q=dishwasher&from="+today+"&to="+today, v.person("kanishk"), "")
	if code != 200 || out["total"] != 2.0 {
		t.Fatalf("person's search for today = %d %v, want the message and the report", code, out)
	}
	for _, bad := range []string{"", "q=x&from=yesterday", "q=x&kind=nope", "q=x&limit=ten", "q=x&run_id=not-a-uuid"} {
		if code, out := call(t, "GET", v.internal.URL+"/api/v1/search?"+bad, token, ""); code != 400 {
			t.Errorf("%q = %d %v, want 400", bad, code, out)
		}
	}

	cs := mcpSession(t, v.internal.URL+"/mcp", token)
	res, isErr := callTool(t, cs, "search", map[string]any{"q": `"dishwasher warranty"`, "kind": "message"})
	if isErr || res["total"] != 1.0 {
		t.Fatalf("search tool = %v", res)
	}
	part := res["results"].([]any)[0].(map[string]any)["snippet_parts"].([]any)
	if len(part) < 2 {
		t.Fatalf("snippet parts = %v, want the match marked", part)
	}
}
