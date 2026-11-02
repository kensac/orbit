package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
)

func TestRememberWritesAnEntryAtOnce(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, "topics: [money]\n")
	a := e.Current().Config.Agents["finance"]
	send := func(bodies ...string) *store.ThreadRun {
		t.Helper()
		var runID string
		for _, b := range bodies {
			r, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: a.SHA, Body: b, Author: "kanishk", Wake: true})
			if err != nil {
				t.Fatal(err)
			}
			if runID == "" {
				runID = r.RunID
			}
		}
		c, err := s.ClaimRun(ctx, "test")
		if err != nil || c == nil {
			t.Fatalf("claim: %v %v", c, err)
		}
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		run, err := s.ThreadRunByID(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}

	run := send("/remember Never push while a question is open. Ask first.")
	want := "Saved to memory: [feedback] Never push while a question is open (profile/never-push-while-a-question-is-open), for every agent."
	if run.Status != "succeeded" || *run.Report != want || !strings.HasPrefix(*run.Summary, "Saved to memory: [feedback] Never push") || fr.calls != 0 {
		t.Fatalf("run = %s %q %q, backend calls %d", run.Status, deref(run.Report), deref(run.Summary), fr.calls)
	}
	entries, _ := s.ListEntries(ctx, store.EntryFilter{Scope: store.ScopeProfile})
	if len(entries) != 1 || entries[0].Body != "Never push while a question is open. Ask first." || entries[0].Source != store.SourcePerson ||
		deref(entries[0].Author) != "kanishk" || deref(entries[0].SourceRunID) != run.ID {
		t.Fatalf("entries = %+v", entries)
	}

	// The same again is not a second entry; a topic the agent does not read is refused with the ones it does.
	if run := send("/remember Never push while a question is open. Ask first."); !strings.HasPrefix(deref(run.Report), "Already in memory: [feedback] Never push") {
		t.Fatalf("again = %q", deref(run.Report))
	}
	if run := send("/remember --topic homelab hetzner has no firewall"); deref(run.Report) != `Not saved: finance does not read the topic "homelab" (it reads: money).` {
		t.Fatalf("unread topic = %q", deref(run.Report))
	}
	if run := send("/remember --topic money --project Rent is 2700"); deref(run.Report) != "Saved to memory: [project] Rent is 2700 (topic/money/rent-is-2700), for the agents that read money." {
		t.Fatalf("topic = %q", deref(run.Report))
	}
	if run := send("/remember --nope x"); !strings.HasPrefix(deref(run.Report), "Not saved: --nope is not a flag of /remember. Write /remember [--agent") {
		t.Fatalf("bad flag = %q", deref(run.Report))
	}
	if fr.calls != 0 {
		t.Fatalf("a /remember called the backend %d times", fr.calls)
	}

	// Beside another message, the model answers that one, is told what was saved, and the reply ends with the confirmation.
	run = send("/remember --agent I am vegetarian", "What did October cost?")
	if fr.calls != 1 || !strings.Contains(fr.got.Prompt, "(orbit has saved to memory what you asked with /remember") ||
		!strings.Contains(fr.got.Prompt, "What did October cost?") || strings.Contains(fr.got.Prompt, "/remember --agent") {
		t.Fatalf("calls %d, prompt:\n%s", fr.calls, fr.got.Prompt)
	}
	if !strings.Contains(fr.got.Prompt, "(agent/i-am-vegetarian)") {
		t.Fatal("the run's index does not hold the entry it just saved")
	}
	if !strings.HasSuffix(deref(run.Report), "\n\nSaved to memory: [user] I am vegetarian (agent/i-am-vegetarian), for finance only.") ||
		!strings.HasPrefix(deref(run.Report), "Spend was fine.") {
		t.Fatalf("mixed report = %q", deref(run.Report))
	}
}

// The learning pass does not learn a /remember again: it was written when it was said.
func TestLearningSkipsRemember(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harness(t, fr)
	a := e.Current().Config.Agents["finance"]
	res, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: a.SHA, Body: "/remember Plain words only", Author: "kanishk", Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.ClaimRun(ctx, "test")
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	id, err := s.EnqueueLearning(ctx, "finance", a.SHA, convOf(t, s, res.MessageID), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, _ = s.ClaimRun(ctx, "test")
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	run, _ := s.GetRun(ctx, id)
	if run.Status != store.StatusSkipped || *run.Gate != "learn:nothing-new" || fr.calls != 0 {
		t.Fatalf("learning run = %s gate %v, backend calls %d", run.Status, run.Gate, fr.calls)
	}
}
