package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
)

// withEntries is goodOutput with an entries list.
func withEntries(t *testing.T, base string, entries string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(base), &m); err != nil {
		t.Fatal(err)
	}
	var e any
	if err := json.Unmarshal([]byte(entries), &e); err != nil {
		t.Fatal(err)
	}
	m["entries"] = e
	b, _ := json.Marshal(m)
	return string(b)
}

func TestARunWritesItsOwnEntries(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harnessWith(t, fr, "topics: [money]\n")
	shown, err := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeTopic, Owner: "money"},
		EntryFields: store.EntryFields{Type: "project", Title: "Rent", Body: "Rent is $2,000."}}, store.Provenance{Source: store.SourcePerson})
	if err != nil {
		t.Fatal(err)
	}
	fr.output = withEntries(t, goodOutput, `[
	  {"op":"add","ref":"profile","type":"feedback","title":"Round to dollars","description":"","body":"Report whole dollars.","why":"","how_to_apply":"","superseded_by":"","note":"asked for it"},
	  {"op":"update","ref":"topic/money/rent","type":"","title":"","description":"","body":"Rent is $2,100.","why":"","how_to_apply":"","superseded_by":"","note":"rent went up"},
	  {"op":"add","ref":"topic/elsewhere","type":"user","title":"x","description":"","body":"y","why":"","how_to_apply":"","superseded_by":"","note":""},
	  {"op":"retire","ref":"profile/unknown","type":"","title":"","description":"","body":"","why":"","how_to_apply":"","superseded_by":"","note":""}
	]`)
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fr.got.System, "- entries: durable things to remember") || !strings.Contains(fr.got.System, "topic/<name> for a topic you read (money)") {
		t.Fatalf("the system prompt does not offer entries:\n%s", fr.got.System)
	}
	if !strings.Contains(string(fr.got.OutputSchema), `"entries"`) {
		t.Fatal("the backend schema has no entries")
	}
	got, _, _ := s.GetEntry(ctx, shown.ID)
	if got.Body != "Rent is $2,100." || got.Version != 2 || got.Source != store.SourceLearned || *got.SourceRunID != c.ID {
		t.Fatalf("updated = %+v", got)
	}
	prof, _ := s.ListEntries(ctx, store.EntryFilter{Scope: store.ScopeProfile})
	if len(prof) != 1 || prof[0].Title != "Round to dollars" {
		t.Fatalf("profile = %+v", prof)
	}
	arts, _ := s.ListArtifacts(ctx, c.ID)
	var rows []map[string]any
	for _, a := range arts {
		if a.Name == "entries.json" {
			b, _, _ := s.GetArtifactContent(ctx, a.ID)
			_ = json.Unmarshal(b, &rows)
		}
	}
	if len(rows) != 4 || rows[0]["result"].(map[string]any)["outcome"] != "written" || rows[2]["result"].(map[string]any)["outcome"] != "refused" ||
		rows[3]["result"].(map[string]any)["outcome"] != "refused" {
		t.Fatalf("entries.json = %v", rows)
	}
}

func TestLearningRunAfterAConversationGoesQuiet(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, h := harnessWith(t, fr, "wake_on_message: true\nlearn: {idle: 5m}\n")
	a := e.Current().Config.Agents["finance"]
	res, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: a.SHA, Body: "Card payments are never spending.", Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.ClaimRun(ctx, "test")
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueLearning(ctx); err != nil {
		t.Fatal(err)
	}
	if n := queued(t, s); n != 0 {
		t.Fatalf("learning was enqueued before the conversation went quiet (%d queued)", n)
	}
	// Five minutes pass.
	if _, err := s.Pool.Exec(ctx, `UPDATE conversations SET last_activity_at = last_activity_at - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET finished_at = finished_at - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if err := e.EnqueueLearning(ctx); err != nil {
		t.Fatal(err)
	}
	if n := queued(t, s); n != 1 {
		t.Fatalf("queued = %d, want the learning run", n)
	}
	h.mu.Lock()
	before := len(h.messages)
	h.mu.Unlock()
	fr.output = `{"summary":"Kept one preference.","entries":[{"op":"add","ref":"agent","type":"feedback","title":"Card payments are not spending",
	  "description":"Exclude card payments","body":"Paying a card from checking is not spending.","why":"The person said so.","how_to_apply":"Leave them out of totals.",
	  "superseded_by":"","note":"Said: card payments are never spending."}]}`
	lc, _ := s.ClaimRun(ctx, "test")
	if lc == nil || lc.Trigger != store.TriggerLearn {
		t.Fatalf("claimed %+v, want the learning run", lc)
	}
	if err := e.Execute(ctx, lc); err != nil {
		t.Fatal(err)
	}
	run, _ := s.GetRun(ctx, lc.ID)
	if run.Status != "succeeded" || *run.Summary != "Kept one preference." || !strings.Contains(*run.Report, "add Card payments are not spending") {
		t.Fatalf("learning run = %s %v %v", run.Status, run.Error, run.Report)
	}
	if !strings.Contains(fr.got.Prompt, "Card payments are never spending.") || !strings.Contains(fr.got.System, "learning pass") {
		t.Fatalf("learning prompt:\n%s", fr.got.Prompt)
	}
	ents, _ := s.ListEntries(ctx, store.EntryFilter{Scope: store.ScopeAgent, Owner: "finance"})
	if len(ents) != 1 || ents[0].Source != store.SourceLearned || *ents[0].SourceConversationID == "" {
		t.Fatalf("entries = %+v", ents)
	}
	h.mu.Lock()
	after := len(h.messages)
	h.mu.Unlock()
	if after != before {
		t.Fatal("a learning run notified")
	}
	// Nothing new: not enqueued again.
	if err := e.EnqueueLearning(ctx); err != nil {
		t.Fatal(err)
	}
	if n := queued(t, s); n != 0 {
		t.Fatalf("learning again with nothing new (%d queued)", n)
	}
	// The person's next run in that conversation is shown the new entry, not the learning run.
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: a.SHA, Body: "And October?", Wake: true, ConversationID: convOf(t, s, res.MessageID)}); err != nil {
		t.Fatal(err)
	}
	fr.output = goodOutput
	next, _ := s.ClaimRun(ctx, "test")
	if err := e.Execute(ctx, next); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fr.got.Prompt, "Card payments are not spending") || strings.Contains(fr.got.Prompt, "Kept one preference") {
		t.Fatalf("next prompt:\n%s", fr.got.Prompt)
	}
}

func TestLearningWithNothingFromThePersonIsSkipped(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harness(t, fr)
	a := e.Current().Config.Agents["finance"]
	res, _, _ := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: a.SHA, Body: "hi"})
	conv := convOf(t, s, res.MessageID)
	// A window that ends before the message holds nothing from the person.
	id, err := s.EnqueueLearning(ctx, "finance", a.SHA, conv, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET learn_through = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	c, _ := s.ClaimRun(ctx, "test")
	calls := fr.calls
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	run, _ := s.GetRun(ctx, id)
	if run.Status != store.StatusSkipped || *run.Gate != "learn:nothing-new" || fr.calls != calls {
		t.Fatalf("run = %s gate %v, backend calls %d", run.Status, run.Gate, fr.calls-calls)
	}
}

func queued(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE status = 'queued'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func convOf(t *testing.T, s *store.Store, messageID string) string {
	t.Helper()
	var id string
	if err := s.Pool.QueryRow(ctx, `SELECT conversation_id::text FROM messages WHERE id = $1`, messageID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
