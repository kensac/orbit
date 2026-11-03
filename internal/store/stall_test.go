package store_test

import (
	"testing"

	"github.com/kensac/orbit/internal/testdb"
)

func TestToolCallRunningReadsTheJournal(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := manual(t, s, "a")
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	running := func(want bool) {
		t.Helper()
		got, err := s.ToolCallRunning(ctx, id, c.Attempt)
		if err != nil || got != want {
			t.Fatalf("running = %v %v, want %v", got, err, want)
		}
	}
	running(false)
	if err := s.AppendEvent(ctx, id, c.Attempt, 1, "assistant", []byte(`{"type":"assistant","message":{"content":[`+
		`{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"make test"}}]}}`)); err != nil {
		t.Fatal(err)
	}
	running(true)
	if err := s.AppendEvent(ctx, id, c.Attempt, 2, "user", []byte(`{"type":"user","message":{"content":[`+
		`{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}`)); err != nil {
		t.Fatal(err)
	}
	running(false)
	if stalled, err := s.AttemptStalled(ctx, id, c.Attempt); err != nil || stalled {
		t.Fatalf("stalled = %v %v", stalled, err)
	}
	if err := s.AppendEvent(ctx, id, c.Attempt, 3, "orbit.stalled", []byte(`{"idle_ms":1,"stall_ms":1}`)); err != nil {
		t.Fatal(err)
	}
	if stalled, err := s.AttemptStalled(ctx, id, c.Attempt); err != nil || !stalled {
		t.Fatalf("stalled = %v %v", stalled, err)
	}
}
