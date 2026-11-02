package engine_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	orbitharness "github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/status"
	"github.com/kensac/orbit/internal/store"
)

// A claude-code run's hooks post to the attempt's listener: each event of
// its turn is in the run's stream as orbit.hook, and the turn's state is
// on the run while it runs, then kept with the final row.
func TestHookStatusRecorded(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harness(t, fr)
	e.Harness = "/opt/orbit"
	var during *store.Run
	fr.during = func(req runner.Request) {
		if req.Harness == nil || req.Harness.Status == "" {
			t.Error("the hooks were configured without the status socket")
			return
		}
		o := orbitharness.HookOptions{Root: req.Workdir, Status: req.Harness.Status}
		for _, in := range []string{
			`{"hook_event_name":"SessionStart","source":"startup"}`,
			`{"hook_event_name":"UserPromptSubmit","prompt_id":"p1","prompt":"check October"}`,
			`{"hook_event_name":"PreToolUse","prompt_id":"p1","tool_name":"Read","tool_input":{"file_path":"inputs/tx.json"}}`,
			`{"hook_event_name":"PermissionRequest","prompt_id":"p1","tool_name":"Write"}`,
			// Another turn's (a live process's earlier run, read late).
			`{"hook_event_name":"Stop","prompt_id":"p0"}`,
		} {
			if err := orbitharness.Hook(strings.NewReader(in), io.Discard, o); err != nil {
				t.Error(err)
			}
		}
		during, _ = s.GetRun(ctx, req.RunID)
		_ = orbitharness.Hook(strings.NewReader(`{"hook_event_name":"Stop","prompt_id":"p1"}`), io.Discard, o)
	}
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	snap := func(r *store.Run) *status.Snapshot {
		var sn status.Snapshot
		if r == nil || json.Unmarshal(r.HookStatus, &sn) != nil {
			t.Fatalf("no hook status on %+v", r)
		}
		return &sn
	}
	if sn := snap(during); sn.State != status.Waiting || sn.Kind != status.Permission || sn.Detail != "Write" || sn.Attempt != 1 {
		t.Fatalf("while running: %+v", sn)
	}
	if st := status.Derive(status.Run{Status: during.Status, Attempt: during.Attempt, Hook: snap(during)}); st.State != status.Waiting || st.Source != status.SourceHooks {
		t.Fatalf("derived while running: %+v", st)
	}
	run, _ := s.GetRun(ctx, c.ID)
	if sn := snap(run); run.Status != "succeeded" || sn.State != status.Done || sn.Event != "Stop" || sn.PromptID != "p1" {
		t.Fatalf("final: %s %+v", run.Status, sn)
	}
	evs, err := s.ListEvents(ctx, c.ID, 1, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ev := range evs {
		if ev.Kind == "orbit.hook" {
			var he status.Event
			_ = json.Unmarshal(ev.Payload, &he)
			names = append(names, he.Name)
		}
	}
	if got := strings.Join(names, ","); got != "SessionStart,UserPromptSubmit,PreToolUse,PermissionRequest,Stop" {
		t.Fatalf("orbit.hook events = %s", got)
	}
}

// Without orbit's binary there are no hooks and no listener.
func TestHookStatusOffWithoutHarness(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harness(t, fr)
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if fr.got.Harness != nil {
		t.Fatal("harness without a binary")
	}
	run, _ := s.GetRun(ctx, c.ID)
	if run.HookStatus != nil {
		t.Fatalf("hook status = %s", run.HookStatus)
	}
}
