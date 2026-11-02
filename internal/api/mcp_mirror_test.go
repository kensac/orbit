package api_test

import (
	"encoding/json"
	"net/url"
	"reflect"
	"testing"

	"github.com/kensac/orbit/internal/store"
)

// TestMCPMirrorsHTTP pins every MCP tool that mirrors an HTTP list or read to the same body as its HTTP counterpart, paging and errors included (docs/api.md, MCP).
func TestMCPMirrorsHTTP(t *testing.T) {
	v := newEnv(t, nil)
	api := v.internal.URL + "/api/v1"
	cs := mcpSession(t, v.internal.URL+"/mcp", token)

	// Two messages in the default conversation, the first run proposing two actions, and a second conversation.
	for _, body := range []string{"How much on Uber Eats?", "And on groceries?"} {
		if code, out := call(t, "POST", api+"/agents/a/messages", token, `{"body":"`+body+`","author":"kanishk"}`); code != 201 {
			t.Fatalf("post = %d %v", code, out)
		}
	}
	c, err := v.s.ClaimRun(ctx, "w")
	if err != nil || c == nil {
		t.Fatalf("claim = %v %v", c, err)
	}
	notify := json.RawMessage(`{"channel":"c","title":"t","body":"b"}`)
	if _, err := v.s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: c.Attempt, Status: "succeeded", Summary: "s",
		Report: "You spent $84 on Uber Eats."}, store.Outcome{Actions: []store.ProposedAction{
		{Type: "notify", Params: notify, Reason: "one", Rule: "approve"},
		{Type: "notify", Params: notify, Reason: "two", Rule: "approve"}}}); err != nil {
		t.Fatal(err)
	}
	code, conv := call(t, "POST", api+"/agents/a/conversations", token, `{"title":"Other","body":"New topic"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, conv)
	}
	convID := conv["conversation_id"].(string)

	same := func(t *testing.T, tool string, args map[string]any, path string) map[string]any {
		t.Helper()
		got, isErr := callTool(t, cs, tool, args)
		if isErr {
			t.Fatalf("%s %v: %v", tool, args, got)
		}
		code, want := get(t, api+path, token)
		if code != 200 {
			t.Fatalf("GET %s = %d %v", path, code, want)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s %v =\n%v\nGET %s =\n%v", tool, args, got, path, want)
		}
		return got
	}
	next := func(page map[string]any) string {
		if page["has_more"] != true {
			t.Fatalf("expected another page: %v", page)
		}
		return page["next_before"].(string)
	}

	t.Run("list_conversations", func(t *testing.T) {
		all := same(t, "list_conversations", map[string]any{"agent": "a"}, "/agents/a/conversations")
		if _, ok := all["next_before"]; !ok || all["next_before"] != nil {
			t.Fatalf("a last page has next_before null: %v", all)
		}
		first := same(t, "list_conversations", map[string]any{"agent": "a", "limit": 1}, "/agents/a/conversations?limit=1")
		b := next(first)
		same(t, "list_conversations", map[string]any{"agent": "a", "limit": 1, "before": b}, "/agents/a/conversations?limit=1&before="+url.QueryEscape(b))
		same(t, "list_conversations", map[string]any{"agent": "a", "archived": true}, "/agents/a/conversations?archived=true")
	})

	t.Run("read_thread", func(t *testing.T) {
		all := same(t, "read_thread", map[string]any{"agent": "a", "limit": 50}, "/agents/a/thread?limit=50")
		if _, ok := all["next_before"]; !ok || all["next_before"] != nil {
			t.Fatalf("a last page has next_before null: %v", all)
		}
		first := same(t, "read_thread", map[string]any{"agent": "a", "limit": 1}, "/agents/a/thread?limit=1")
		b := next(first)
		same(t, "read_thread", map[string]any{"agent": "a", "limit": 1, "before": b}, "/agents/a/thread?limit=1&before="+url.QueryEscape(b))
		same(t, "read_thread", map[string]any{"conversation_id": convID, "limit": 50}, "/conversations/"+convID+"/thread?limit=50")
	})

	t.Run("list_actions", func(t *testing.T) {
		pending := same(t, "list_actions", nil, "/actions?status=pending")
		if len(pending["actions"].([]any)) != 2 {
			t.Fatalf("pending = %v", pending)
		}
		same(t, "list_actions", map[string]any{"status": "all"}, "/actions")
		first := same(t, "list_actions", map[string]any{"status": "all", "limit": 1}, "/actions?limit=1")
		b := next(first)
		same(t, "list_actions", map[string]any{"status": "all", "limit": 1, "before": b}, "/actions?limit=1&before="+url.QueryEscape(b))
		same(t, "list_actions", map[string]any{"agent": "a", "run_id": c.ID}, "/actions?agent=a&status=pending&run_id="+c.ID)
	})

	t.Run("search", func(t *testing.T) {
		same(t, "search", map[string]any{"q": "Uber"}, "/search?q=Uber")
		same(t, "search", map[string]any{"q": "topic", "conversation_id": convID, "limit": 1}, "/search?q=topic&limit=1&conversation_id="+convID)
	})

	// A tool error is worded as the HTTP error is.
	t.Run("errors", func(t *testing.T) {
		for _, tc := range []struct {
			tool         string
			args         map[string]any
			method, path string
		}{
			{"read_thread", map[string]any{"agent": "a", "before": "junk"}, "GET", "/agents/a/thread?before=junk"},
			{"read_thread", map[string]any{"agent": "nope"}, "GET", "/agents/nope/thread"},
			{"read_thread", map[string]any{"conversation_id": "nope"}, "GET", "/conversations/nope/thread"},
			{"list_conversations", map[string]any{"agent": "a", "before": "junk"}, "GET", "/agents/a/conversations?before=junk"},
			{"list_actions", map[string]any{"run_id": "nope"}, "GET", "/actions?run_id=nope"},
			{"list_actions", map[string]any{"before": "junk"}, "GET", "/actions?before=junk"},
			{"approve_action", map[string]any{"action_id": "nope"}, "POST", "/actions/nope/approve"},
			{"search", map[string]any{"q": "x", "from": "yesterday"}, "GET", "/search?q=x&from=yesterday"},
		} {
			got, isErr := callTool(t, cs, tc.tool, tc.args)
			_, want := call(t, tc.method, api+tc.path, token, "")
			if !isErr || got["error"] != want["error"] {
				t.Errorf("%s %v = %v, want the HTTP error %q", tc.tool, tc.args, got, want["error"])
			}
		}
		// A second decision is the same conflict either way.
		id := v.mustPending(t)
		if _, isErr := callTool(t, cs, "approve_action", map[string]any{"action_id": id}); isErr {
			t.Fatal("approve failed")
		}
		got, _ := callTool(t, cs, "reject_action", map[string]any{"action_id": id})
		_, want := call(t, "POST", api+"/actions/"+id+"/reject", token, "")
		if got["error"] != want["error"] {
			t.Errorf("second decision = %v, HTTP %v", got, want)
		}
	})
}

func (v *env) mustPending(t *testing.T) string {
	t.Helper()
	acts, err := v.s.ListActions(ctx, "a", "pending", 1)
	if err != nil || len(acts) == 0 {
		t.Fatalf("pending = %v %v", acts, err)
	}
	return acts[0].ID
}
