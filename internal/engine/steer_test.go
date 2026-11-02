package engine_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// steerCLI is a live Claude Code that speaks the CLI's queued-message protocol as 2.1.316 does (claudeproto.TestRealCLISteer). A turn whose prompt ends in WAIT calls a tool that runs until a message arrives on stdin, then: "fold" folds it into the turn (started, one result answering both); "late" queues it past the result and confirms its withdrawal; "unknown" answers the withdrawal with cancelled false; "hold" queues it and waits for an interrupt, which withdraws it (cancel_queued) and ends the turn. Any other turn answers at once.
const steerCLI = `#!/bin/bash
field() { sed -n 's/.*"'"$1"'":"\([^"]*\)".*/\1/p' <<<"$2"; }
say() { printf '%s\n' "$1"; }
n=0
while IFS= read -r line; do
  case "$line" in *'"type":"user"'*) ;; *) continue ;; esac
  n=$((n+1)); u=$(field uuid "$line")
  say '{"type":"system","subtype":"init","session_id":"s-1","capabilities":["msg_lifecycle_v1","interrupt_receipt_v1","interrupt_cancel_queued_v1"]}'
  say '{"type":"command_lifecycle","command_uuid":"'$u'","state":"started"}'
  case "$line" in *'WAIT\n","role"'*) ;; *)
    say '{"type":"result","subtype":"success","is_error":false,"result":"answered","session_id":"s-1"}'
    continue ;;
  esac
  say '{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"t'$n'","name":"Bash","input":{}}]}}'
  IFS= read -r s
  su=$(field uuid "$s"); st=$(field content "$s")
  say '{"type":"command_lifecycle","command_uuid":"'$su'","state":"queued"}'
  case "$st" in
  hold*)
    IFS= read -r c; id=$(field request_id "$c")
    say '{"type":"command_lifecycle","command_uuid":"'$su'","state":"cancelled"}'
    say '{"type":"control_response","response":{"subtype":"success","request_id":"'$id'","response":{"still_queued":[],"cancelled":["'$su'"]}}}'
    say '{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s-1"}'
    continue ;;
  esac
  say '{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"t'$n'","content":"ok"}]}}'
  case "$st" in
  fold*)
    say '{"type":"command_lifecycle","command_uuid":"'$su'","state":"started"}'
    say '{"type":"command_lifecycle","command_uuid":"'$su'","state":"completed"}'
    say '{"type":"result","subtype":"success","is_error":false,"result":"answered and '"$st"'","session_id":"s-1"}' ;;
  *)
    say '{"type":"result","subtype":"success","is_error":false,"result":"answered","session_id":"s-1"}'
    IFS= read -r c; id=$(field request_id "$c")
    if [ "${st#late}" != "$st" ]; then
      say '{"type":"command_lifecycle","command_uuid":"'$su'","state":"cancelled"}'
      say '{"type":"control_response","response":{"subtype":"success","request_id":"'$id'","response":{"cancelled":true}}}'
    else
      say '{"type":"control_response","response":{"subtype":"success","request_id":"'$id'","response":{"cancelled":false}}}'
    fi ;;
  esac
  say '{"type":"command_lifecycle","command_uuid":"'$u'","state":"completed"}'
done
`

func TestSteeringARunningReply(t *testing.T) {
	s := testdb.New(t)
	dir := t.TempDir()
	cli := filepath.Join(dir, "claude")
	if err := os.WriteFile(cli, []byte(steerCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(`
defaults: {runner: cc, channels: [log]}
runners: {cc: {kind: claude-code, binary: `+cli+`}}
channels: {log: {kind: log}}
`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "helper.yaml"), []byte(`
name: helper
charter: Help.
chat: true
wake_on_message: true
session: {resume: true, idle: 1h, live: 5m}
`), 0o644)
	env := func(k string) (string, bool) {
		v, ok := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-1"}[k]
		return v, ok
	}
	e := &engine.Engine{Store: s, DataDir: t.TempDir(), ConfigDir: dir, Env: env, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewRunner: runner.New, Live: runner.NewLive(2)}
	defer e.Live.Close()
	if err := e.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	a := e.Current().Config.Agents["helper"]
	post := func(body, delivery string) store.PostResult {
		t.Helper()
		r, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "helper", SpecSHA: a.SHA, Body: body, Wake: true,
			Delivery: delivery, Steerable: a.Chat})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// waitFor polls until the run has an event of kind whose payload holds text.
	waitFor := func(runID, kind, text string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM run_events WHERE run_id = $1 AND kind = $2 AND payload::text LIKE '%' || $3 || '%'`,
				runID, kind, text).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n > 0 {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("run %s: no %s event with %q", runID, kind, text)
	}
	// start claims the queued run and executes it in the background.
	start := func(want string) (*store.ClaimedRun, chan error) {
		t.Helper()
		c, err := s.ClaimRun(ctx, "test")
		if err != nil || c == nil || (want != "" && c.ID != want) {
			t.Fatalf("claim = %+v %v, want %s", c, err, want)
		}
		done := make(chan error, 1)
		go func() { done <- e.Execute(ctx, c) }()
		return c, done
	}
	finish := func(c *store.ClaimedRun, done chan error) *store.ThreadRun {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			rows, _ := s.Pool.Query(ctx, `SELECT kind, left(payload::text, 300) FROM run_events WHERE run_id = $1 ORDER BY seq`, c.ID)
			for rows.Next() {
				var k, p string
				_ = rows.Scan(&k, &p)
				t.Log(k, p)
			}
			t.Fatal("the run did not finish")
		}
		run, err := s.ThreadRunByID(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	message := func(id string) (runID, steered *string) {
		t.Helper()
		if err := s.Pool.QueryRow(ctx, `SELECT run_id::text, steered_into::text FROM messages WHERE id = $1`, id).Scan(&runID, &steered); err != nil {
			t.Fatal(err)
		}
		return runID, steered
	}
	queued := func() []string {
		t.Helper()
		rows, err := s.Pool.Query(ctx, `SELECT id::text FROM runs WHERE agent = 'helper' AND status = 'queued'`)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		return ids
	}

	// A steer during a tool call is folded into the reply.
	m1 := post("one WAIT", store.DeliveryQueue)
	c, done := start(m1.RunID)
	waitFor(c.ID, "assistant", "tool_use")
	st := post("fold: banana", store.DeliverySteer)
	if st.Delivered != store.DeliveredSteered || st.RunID != c.ID {
		t.Fatalf("steer = %+v", st)
	}
	run := finish(c, done)
	if run.Status != "succeeded" || deref(run.Report) != "answered and fold: banana" {
		t.Fatalf("run = %s %q %q", run.Status, deref(run.Report), deref(run.Error))
	}
	if read, into := message(st.MessageID); read == nil || *read != c.ID || into == nil || *into != c.ID {
		t.Fatalf("the steered message: read by %v, steered into %v", read, into)
	}
	var steerItem string
	if err := s.Pool.QueryRow(ctx, `SELECT body->>'text' FROM journal_items WHERE run_id = $1 AND kind = 'steer'`, c.ID).Scan(&steerItem); err != nil || steerItem != "fold: banana" {
		t.Fatalf("steer item = %q %v", steerItem, err)
	}
	if q := queued(); len(q) != 0 || e.Live.Len() != 1 {
		t.Fatalf("queued %v, live %d: nothing left to answer, the process kept", q, e.Live.Len())
	}

	// After the last tool call: withdrawn when the result arrives, so the next run answers it.
	m2 := post("two WAIT", store.DeliveryQueue)
	c, done = start(m2.RunID)
	waitFor(c.ID, "assistant", "tool_use")
	late := post("late: x", store.DeliverySteer)
	run = finish(c, done)
	if run.Status != "succeeded" || deref(run.Report) != "answered" {
		t.Fatalf("run = %s %q %q", run.Status, deref(run.Report), deref(run.Error))
	}
	waitFor(c.ID, "orbit.steer_withdrawn", `"confirmed": true`)
	if read, into := message(late.MessageID); read != nil || into != nil {
		t.Fatalf("a withdrawn steer was taken: %v %v", read, into)
	}
	q := queued()
	if len(q) != 1 || e.Live.Len() != 1 {
		t.Fatalf("queued %v, live %d: want a run for the withdrawn steer, the process kept", q, e.Live.Len())
	}
	c, done = start(q[0])
	if run = finish(c, done); run.Status != "succeeded" {
		t.Fatalf("the woken run = %s %q", run.Status, deref(run.Error))
	}
	if read, _ := message(late.MessageID); read == nil || *read != c.ID {
		t.Fatalf("the woken run did not read the steer: %v", read)
	}

	// The CLI does not confirm the withdrawal: the process is closed, the message is the next run's.
	m3 := post("three WAIT", store.DeliveryQueue)
	c, done = start(m3.RunID)
	waitFor(c.ID, "assistant", "tool_use")
	unknown := post("unknown: y", store.DeliverySteer)
	if run = finish(c, done); run.Status != "succeeded" {
		t.Fatalf("run = %s %q", run.Status, deref(run.Error))
	}
	waitFor(c.ID, "orbit.steer_withdrawn", `"confirmed": false`)
	if read, _ := message(unknown.MessageID); read != nil || e.Live.Len() != 0 || len(queued()) != 1 {
		t.Fatalf("read %v, live %d, queued %v: want unread, the process closed, a run for it", read, e.Live.Len(), queued())
	}
	c, done = start("")
	if run = finish(c, done); run.Status != "succeeded" {
		t.Fatalf("the woken run = %s %q", run.Status, deref(run.Error))
	}

	// Stop with a steer queued: the interrupt withdraws it with the turn.
	m4 := post("four WAIT", store.DeliveryQueue)
	c, done = start(m4.RunID)
	waitFor(c.ID, "assistant", "tool_use")
	hold := post("hold: z", store.DeliverySteer)
	waitFor(c.ID, "command_lifecycle", hold.MessageID)
	if _, err := s.CancelRun(ctx, c.ID, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if run = finish(c, done); run.Status != "cancelled" {
		t.Fatalf("run = %s %q", run.Status, deref(run.Error))
	}
	waitFor(c.ID, "orbit.steer_withdrawn", hold.MessageID)
	if read, _ := message(hold.MessageID); read != nil || len(queued()) != 1 {
		t.Fatalf("read %v, queued %v: the stopped reply's steer is the next run's", read, queued())
	}

	// Nothing running: queued, and says so.
	c, done = start("")
	finish(c, done)
	if r := post("anyone?", store.DeliverySteer); r.Delivered != store.DeliveredQueued || r.RunID == "" {
		t.Fatalf("a steer with nothing running = %+v", r)
	}
}
