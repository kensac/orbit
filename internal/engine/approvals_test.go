package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

// answer decides the next pending approval of conv as a person would, once one exists.
func answer(t *testing.T, s *store.Store, conv string, allow bool, scope, msg string) <-chan store.Approval {
	t.Helper()
	out := make(chan store.Approval, 1)
	go func() {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			list, err := s.ListApprovals(ctx, conv, store.ApprovalPending, 0)
			if err != nil || len(list) == 0 {
				continue
			}
			a, err := s.DecideApproval(ctx, list[0].ID, allow, scope, msg, "kanishk")
			if err == nil {
				out <- a
				return
			}
		}
		close(out)
	}()
	return out
}

var npmTest = json.RawMessage(`[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm test:*"}],"behavior":"allow","destination":"localSettings"}]`)

func TestAskingAPersonForAToolCall(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, "chat: true\npermissions: {level: ask, ceiling: edits}\n")
	e.ApprovalTimeout = 1500 * time.Millisecond
	a := e.Current().Config.Agents["finance"]
	var conv string
	if err := s.Pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE agent = 'finance' AND is_default`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	var got []claudeproto.Permission
	fr.seen = func(req runner.Request) {
		ask := func(id, tool, input string, sugg json.RawMessage, user bool) {
			got = append(got, req.Chat.Prompt(context.Background(), claudeproto.PermissionRequest{RequestID: id, Tool: tool,
				Input: json.RawMessage(input), Suggestions: sugg, DecisionReason: "why " + id, RequiresUserInteraction: user}))
		}
		decided := answer(t, s, conv, true, store.ScopeConversation, "")
		ask("q1", "Bash", `{"command":"npm test"}`, npmTest, false)
		if a, ok := <-decided; !ok || a.DecisionReason != "why q1" || a.Level != "ask" {
			t.Errorf("the person saw %+v", a)
		}
		// The same suggestion is the conversation's grant: answered without asking.
		ask("q2", "Bash", `{"command":"npm test -- -v"}`, npmTest, false)
		// A plan is always the person's, and a deny's message reaches the model.
		decided = answer(t, s, conv, false, store.ScopeOnce, "add a test first")
		ask("q3", "ExitPlanMode", `{"plan":"edit main.go"}`, nil, true)
		<-decided
		// Nobody answers: denied at the timeout.
		ask("q4", "Write", `{"file_path":"x"}`, nil, false)
	}
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: a.SHA, Body: "go", Wake: true}); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || !got[0].Allow || !got[1].Allow || got[2].Allow || got[2].Message != "add a test first" || got[3].Allow || !strings.Contains(got[3].Message, "nobody answered") {
		t.Fatalf("answers = %+v", got)
	}
	all, _ := s.ListApprovals(ctx, conv, "", 0)
	states := map[string]string{}
	for _, ap := range all {
		states[ap.RequestID] = ap.State + "/" + ap.Kind
	}
	if len(all) != 3 || states["q1"] != "allowed/tool" || states["q3"] != "denied/plan" || states["q4"] != "expired/tool" {
		t.Fatalf("approvals = %v: q2 was a grant, not a row", states)
	}
	var events int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM run_events WHERE run_id = $1 AND kind = 'orbit.approval'`, c.ID).Scan(&events)
	if events != 7 { // q1, q3, q4 opened and ended; q2's grant
		t.Fatalf("orbit.approval events = %d", events)
	}
	// Each request put to the person had an attention row, and every one closed with its approval.
	var rows int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM attention WHERE kind = 'permission' AND push AND run_id = $1`, c.ID).Scan(&rows)
	if open, err := s.ListAttention(ctx, 0); rows != 3 || err != nil || len(open) != 0 {
		t.Fatalf("permission rows = %d, open = %+v %v", rows, open, err)
	}

	// An unattended run asks nobody.
	fr.seen = func(req runner.Request) {
		got = []claudeproto.Permission{req.Chat.Prompt(context.Background(), claudeproto.PermissionRequest{RequestID: "u1", Tool: "Bash", Input: json.RawMessage(`{}`)})}
	}
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "finance", SpecSHA: a.SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	c, _ = s.ClaimRun(ctx, "test")
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got[0].Allow || !strings.Contains(got[0].Message, "nobody is attending") {
		t.Fatalf("unattended = %+v", got)
	}
	if all, _ := s.ListApprovals(ctx, conv, "", 0); len(all) != 3 {
		t.Fatalf("an unattended request was put to the person: %d approvals", len(all))
	}
	if _ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM attention WHERE kind = 'permission'`).Scan(&rows); rows != 3 {
		t.Fatalf("an unattended request reached the inbox: %d rows", rows)
	}
}
