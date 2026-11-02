package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/journal"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// claudeTurn is one Claude Code turn: prose, a tool call and its result
// (big enough to be bounded), and the result line.
func claudeTurn(tool, text string) []string {
	out, _ := json.Marshal(strings.Repeat("output ", 2000))
	return []string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + text + `"},` +
			`{"type":"tool_use","id":"` + tool + `","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + tool + `","content":` + string(out) + `}]}}`,
		`{"type":"result","subtype":"success","duration_ms":10}`,
	}
}

func appendLines(t *testing.T, s *store.Store, run string, attempt int, lines ...string) {
	t.Helper()
	evs, _ := s.ListEvents(ctx, run, attempt, 0, 5000)
	for i, l := range lines {
		var h struct{ Type, Subtype string }
		_ = json.Unmarshal([]byte(l), &h)
		kind := h.Type
		if h.Subtype != "" {
			kind += "." + h.Subtype
		}
		if err := s.AppendEvent(ctx, run, attempt, len(evs)+i+1, kind, []byte(l)); err != nil {
			t.Fatal(err)
		}
	}
}

func readAll(t *testing.T, s *store.Store, run string, epoch int) []journal.Item {
	t.Helper()
	items, more, err := s.ReadJournal(ctx, run, epoch, 0, store.MaxJournalLimit)
	if err != nil || more {
		t.Fatal(err, more)
	}
	return items
}

func body[T any](t *testing.T, it journal.Item) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(it.Body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func latest(items []journal.Item, id string) *journal.Item {
	for _, it := range journal.Fold(items) {
		if it.ID == id {
			return &it
		}
	}
	return nil
}

func TestTheJournalIsWrittenWithEachEvent(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	run := manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	appendLines(t, s, run, c.Attempt, claudeTurn("t1", "Looking.")...)

	items := readAll(t, s, run, 1)
	var kinds []string
	for i, it := range items {
		if it.Seq != i+1 || it.Epoch != 1 || it.Attempt != 1 || it.Cursor != (journal.Cursor{Epoch: 1, Seq: i + 1}).String() || it.V != journal.Version {
			t.Fatalf("item %d = %+v", i, it)
		}
		kinds = append(kinds, it.Kind+"/"+it.ID)
	}
	want := []string{"turn/turn:1.1", "status/status:1.1.0", "message/message:1.2.0", "tool_call/tool:t1", "tool_call/tool:t1", "turn/turn:1.1"}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("items = %v", kinds)
	}
	// Append-only: the call's first revision is still the call as made.
	if first := body[journal.ToolCall](t, items[3]); first.State != journal.ToolRunning || first.Result != nil || items[3].Revision != 1 {
		t.Fatalf("first revision = %s", items[3].Body)
	}
	done := body[journal.ToolCall](t, items[4])
	if items[4].Revision != 2 || done.State != journal.ToolCompleted || !done.Result.Truncated || *items[4].Turn != "turn:1.1" {
		t.Fatalf("result revision = %s", items[4].Body)
	}

	// The whole output is fetched by digest, and only through a run that
	// holds it.
	full, err := s.JournalBody(ctx, run, done.Result.SHA256)
	if err != nil || len(full) != done.Result.Bytes || !strings.HasPrefix(string(full), done.Result.Head) {
		t.Fatalf("body = %d bytes, %v", len(full), err)
	}
	other := manual(t, s, "a")
	if _, err := s.JournalBody(ctx, other, done.Result.SHA256); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another run's body = %v", err)
	}
	h, err := s.GetJournalHead(ctx, run, 0)
	if err != nil || h.Epoch != 1 || h.Last != 6 || len(h.Epochs) != 1 {
		t.Fatalf("head = %+v %v", h, err)
	}
	if page, more, _ := s.ReadJournal(ctx, run, 1, 2, 3); len(page) != 3 || !more || page[0].Seq != 3 {
		t.Fatalf("page = %d %v", len(page), more)
	}
}

func TestBackfillWritesWhatTheLiveWriterWould(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a", "b")
	live := manual(t, s, "a")
	lc, _ := s.ClaimRun(ctx, "w")
	lines := append(claudeTurn("t1", "one"), `{"type":"assistant","message":{"content":[{"type":"text","text":"still going"}]}}`)
	appendLines(t, s, live, lc.Attempt, lines...)

	// The same transcript written by an instance that predates the journal.
	old := manual(t, s, "b")
	oc, _ := s.ClaimRun(ctx, "w")
	if _, err := s.Pool.Exec(ctx, `INSERT INTO run_events (run_id, attempt, seq, at, kind, payload)
		SELECT $2, $3, seq, at, kind, payload FROM run_events WHERE run_id = $1`, live, old, oc.Attempt); err != nil {
		t.Fatal(err)
	}
	if n, err := s.BackfillJournal(ctx, true); err != nil || n != 1 {
		t.Fatalf("backfill = %d %v", n, err)
	}
	strip := func(items []journal.Item) string {
		for i := range items {
			items[i].RunID = ""
		}
		b, _ := json.Marshal(items)
		return string(b)
	}
	if a, b := strip(readAll(t, s, live, 1)), strip(readAll(t, s, old, 1)); a != b {
		t.Fatalf("backfill differs from live:\n%s\n%s", a, b)
	}
	// Caught up, a second pass writes nothing.
	if n, err := s.BackfillJournal(ctx, true); err != nil || n != 0 {
		t.Fatalf("second backfill = %d %v", n, err)
	}
	// A reader's sync catches up a run that kept being written.
	if _, err := s.Pool.Exec(ctx, `INSERT INTO run_events (run_id, attempt, seq, kind, payload)
		VALUES ($1, $2, 99, 'result.success', '{"type":"result","subtype":"success"}')`, old, oc.Attempt); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncJournal(ctx, old); err != nil {
		t.Fatal(err)
	}
	if tb := body[journal.Turn](t, *latest(readAll(t, s, old, 1), "turn:1.2")); tb.State != journal.TurnCompleted {
		t.Fatalf("turn after sync = %+v", tb)
	}
}

func TestEpochsFollowWhetherAnAttemptContinues(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	run := manual(t, s, "a")
	c1, _ := s.ClaimRun(ctx, "w")
	appendLines(t, s, run, c1.Attempt, `{"type":"assistant","message":{"content":[{"type":"text","text":"first"}]}}`)
	// A drain hands it back mid-turn: the turn is interrupted, in the
	// same transaction.
	if err := s.HandBack(ctx, run, 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if tb := body[journal.Turn](t, *latest(readAll(t, s, run, 1), "turn:1.1")); tb.State != journal.TurnInterrupted || tb.CompletedAt == nil {
		t.Fatalf("handed back turn = %+v", tb)
	}

	// The next attempt resumes the session: same timeline, seq continues.
	c2, _ := s.ClaimRun(ctx, "w")
	if err := s.SetRecovery(ctx, run, c2.Attempt, 1, ""); err != nil {
		t.Fatal(err)
	}
	appendLines(t, s, run, c2.Attempt, `{"type":"assistant","message":{"content":[{"type":"text","text":"continuing"}]}}`)
	items := readAll(t, s, run, 1)
	if last := items[len(items)-1]; last.Attempt != 2 || last.Seq != 5 || last.ID != "message:2.1.0" {
		t.Fatalf("continued = %+v", last)
	}

	// It is lost (no heartbeat): nothing saw its turn end.
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReapStale(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if tb := body[journal.Turn](t, *latest(readAll(t, s, run, 1), "turn:2.1")); tb.State != journal.TurnUnverifiable || tb.CompletedAt != nil || tb.Outcome != "" {
		t.Fatalf("lost turn = %+v", tb)
	}

	// The third starts over: a new epoch, named by its attempt.
	c3, _ := s.ClaimRun(ctx, "w")
	if err := s.SetRecovery(ctx, run, c3.Attempt, 0, "transcript gone"); err != nil {
		t.Fatal(err)
	}
	appendLines(t, s, run, c3.Attempt, claudeTurn("t9", "again")...)
	if err := s.FinishRun(ctx, run, store.RunResult{Attempt: c3.Attempt, Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	h, _ := s.GetJournalHead(ctx, run, 0)
	if h.Epoch != 3 || h.Last != 6 || len(h.Epochs) != 2 || h.Epochs[0] != 1 || h.Epochs[1] != 3 {
		t.Fatalf("head = %+v", h)
	}
	if first := readAll(t, s, run, 3)[0]; first.Seq != 1 || first.Attempt != 3 {
		t.Fatalf("epoch 3 starts with %+v", first)
	}
	// Its result closed the turn; finishing adds no revision.
	turn := latest(readAll(t, s, run, 3), "turn:3.1")
	if tb := body[journal.Turn](t, *turn); turn.Revision != 2 || tb.Outcome != "success" {
		t.Fatalf("finished turn = r%d %+v", turn.Revision, tb)
	}
	it, err := s.JournalItem(ctx, run, 3, "tool:t9")
	if err != nil || it.Revision != 2 {
		t.Fatalf("item = %+v %v", it, err)
	}
	if _, err := s.JournalItem(ctx, run, 3, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown item = %v", err)
	}
}

func TestAFailedRunInterruptsItsOpenTurn(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	run := manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	appendLines(t, s, run, c.Attempt, `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}`)
	if err := s.FinishRun(ctx, run, store.RunResult{Attempt: c.Attempt, Status: "failed", Error: "claude exited during the turn"}); err != nil {
		t.Fatal(err)
	}
	if tb := body[journal.Turn](t, *latest(readAll(t, s, run, 1), "turn:1.1")); tb.State != journal.TurnInterrupted || tb.Outcome != "" {
		t.Fatalf("turn = %+v", tb)
	}
}
