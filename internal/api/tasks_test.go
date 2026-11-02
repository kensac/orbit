package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
)

// delegated records a run of agent a that delegated two tasks to itself,
// b after a, and returns the run and the tasks' conversation.
func delegated(t *testing.T, v *env) (string, string, []store.Delegated) {
	t.Helper()
	run := enqueue(t, v)
	c, att := claim(t, v, "claude-code")
	ids, err := v.s.RecordOutcome(ctx, c, "a", store.RunResult{Attempt: att, Status: "succeeded", Summary: "s"},
		store.Outcome{Actions: []store.ProposedAction{{Type: "delegate", Params: json.RawMessage(`{}`), Rule: "auto"}}})
	if err != nil {
		t.Fatal(err)
	}
	made, err := v.s.Delegate(ctx, store.Delegation{ActionID: ids[0], RunID: run, Tasks: []store.NewTask{
		{Key: "first", Agent: "a", Brief: "do one"}, {Key: "second", Agent: "a", Brief: "do two", After: []string{"first"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var conv string
	_ = v.s.Pool.QueryRow(ctx, `SELECT conversation_id::text FROM runs WHERE id = $1`, run).Scan(&conv)
	return run, conv, made
}

func TestATasksListStreamAndCancel(t *testing.T) {
	v := newEnv(t, fastStreams)
	run, conv, made := delegated(t, v)
	who := v.person("kanishk")

	code, body := call(t, "GET", v.srv.URL+"/api/v1/conversations/"+conv+"/tasks", who, "")
	tasks, _ := body["tasks"].([]any)
	if code != 200 || len(tasks) != 2 {
		t.Fatalf("list = %d %v", code, body)
	}
	second := tasks[1].(map[string]any)
	if second["key"] != "second" || second["status"] != "waiting" || second["depends_on"].([]any)[0] != "first" {
		t.Fatalf("second = %v", second)
	}
	code, body = call(t, "GET", v.srv.URL+"/api/v1/tasks/"+made[0].ID, who, "")
	if code != 200 || body["task"].(map[string]any)["brief"] != "do one" {
		t.Fatalf("get = %d %v", code, body)
	}

	ch, _ := openSSE(t, v, "/api/v1/conversations/"+conv+"/tasks/stream", "")
	if e := expect(t, ch, "tasks", ""); !strings.Contains(e.Data, `"status":"waiting"`) {
		t.Fatalf("first event = %s", e.Data)
	}
	// The person cancels the first; the reconcile cancels its dependent,
	// and the stream says so.
	if code, body := call(t, "POST", v.srv.URL+"/api/v1/tasks/"+made[0].ID+"/cancel", who, `{"reason": "not needed"}`); code != 200 {
		t.Fatalf("cancel = %d %v", code, body)
	}
	for {
		e := expect(t, ch, "tasks", "")
		if strings.Count(e.Data, `"status":"cancelled"`) == 2 {
			if !strings.Contains(e.Data, "cancelled by kanishk: not needed") {
				t.Fatalf("cancel reason missing: %s", e.Data)
			}
			break
		}
	}
	if code, _ := call(t, "POST", v.srv.URL+"/api/v1/tasks/"+made[0].ID+"/cancel", who, ""); code != 409 {
		t.Fatalf("second cancel = %d", code)
	}
	// The run that delegated them carries them in its thread item.
	code, body = call(t, "GET", v.srv.URL+"/api/v1/conversations/"+conv+"/thread", who, "")
	items, _ := body["items"].([]any)
	var found bool
	for _, it := range items {
		m := it.(map[string]any)
		if m["id"] == run {
			found = len(m["tasks"].([]any)) == 2
		}
	}
	if code != 200 || !found {
		t.Fatalf("thread = %d %v", code, body)
	}
	// A task's own conversation names it.
	code, body = call(t, "GET", v.srv.URL+"/api/v1/conversations/"+made[0].ConversationID, who, "")
	if code != 200 || body["conversation"].(map[string]any)["task_id"] != made[0].ID {
		t.Fatalf("task conversation = %d %v", code, body)
	}
}
