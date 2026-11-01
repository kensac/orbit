package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
)

func artifactNames(t *testing.T, s *store.Store, runID string) map[string]bool {
	t.Helper()
	arts, err := s.ListArtifacts(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, a := range arts {
		names[a.Name] = true
	}
	return names
}

func TestTheNextRunSeesTheConversation(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harness(t, fr)
	first := claim(t, e, s)
	if err := e.Execute(ctx, first); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fr.got.Prompt, "# Conversation") || artifactNames(t, s, first.ID)["conversation.md"] {
		t.Fatal("a first run has no conversation to show")
	}
	if _, err := s.SendToAgent(ctx, "finance", "And September?", "k", nil); err != nil {
		t.Fatal(err)
	}
	second := claim(t, e, s)
	if err := e.Execute(ctx, second); err != nil {
		t.Fatal(err)
	}
	p := fr.got.Prompt
	convo := p[strings.Index(p, "# Conversation"):strings.Index(p, "# Messages from your person")]
	for _, want := range []string{"you, report: October spend was $5,914", "Spend was fine.", "you asked", "Was the $500 a deductible?"} {
		if !strings.Contains(convo, want) {
			t.Errorf("conversation lacks %q:\n%s", want, convo)
		}
	}
	// The new message is shown as new, not as history.
	if strings.Contains(convo, "And September?") || !strings.Contains(p, "And September?") {
		t.Fatalf("prompt:\n%s", p)
	}
	if !artifactNames(t, s, second.ID)["conversation.md"] {
		t.Fatal("the conversation a run saw is not stored")
	}
}

const sessionSpec = "session: {resume: true, idle: 1h}\n"

func TestSessionIsResumedAfterASuccessfulRun(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, sessionSpec)
	first := claim(t, e, s)
	if err := e.Execute(ctx, first); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session == nil || fr.got.Session.Resume != "" {
		t.Fatalf("first run session = %+v", fr.got.Session)
	}
	work := fr.got.Workdir
	if !strings.HasPrefix(fr.got.Inputs[0].Path, filepath.Join("inputs", first.ID+"-1")) {
		t.Fatalf("inputs must be in a directory of their own: %s", fr.got.Inputs[0].Path)
	}

	second := claim(t, e, s)
	if err := e.Execute(ctx, second); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session.Resume != "session-1" || fr.got.Workdir != work {
		t.Fatalf("second run resumed %q in %s, want session-1 in %s", fr.got.Session.Resume, fr.got.Workdir, work)
	}
	if !strings.Contains(fr.got.Prompt, "Session: continued") || strings.Contains(fr.got.Prompt, "# Conversation") {
		t.Fatalf("a continued session is not re-told the thread:\n%s", fr.got.Prompt)
	}
	run, _ := s.GetRun(ctx, second.ID)
	if *run.SessionMode != "resumed" || *run.SessionParent != "session-1" || *run.SessionDepth != 2 || *run.SessionID != "session-2" {
		t.Fatalf("run session = %v %v %v %v", *run.SessionMode, *run.SessionParent, *run.SessionDepth, *run.SessionID)
	}
	// The workdir stays for the session; this run's inputs do not.
	if _, err := os.Stat(filepath.Join(work, "inputs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inputs left behind: %v", err)
	}

	// A failed run ends the session: the next starts fresh and is told the
	// thread instead.
	fr.err = errors.New("boom")
	third := claim(t, e, s)
	_ = e.Execute(ctx, third)
	fr.err = nil
	fourth := claim(t, e, s)
	if err := e.Execute(ctx, fourth); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session.Resume != "" || !strings.Contains(fr.got.Prompt, "# Conversation") {
		t.Fatalf("after a failure: resume %q", fr.got.Session.Resume)
	}

	// A transcript that is gone (a /data that did not survive a restart)
	// means a fresh session, not a failed resume.
	if err := os.RemoveAll(fr.got.Session.Home); err != nil {
		t.Fatal(err)
	}
	fifth := claim(t, e, s)
	if err := e.Execute(ctx, fifth); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session.Resume != "" {
		t.Fatalf("resumed a session whose transcript is gone: %q", fr.got.Session.Resume)
	}
	ev, _ := s.ListEvents(ctx, fifth.ID, 0, 0, 0)
	found := false
	for _, x := range ev {
		found = found || (x.Kind == "orbit.session" && strings.Contains(string(x.Payload), "transcript is gone"))
	}
	if !found {
		t.Fatal("the fresh start does not say why")
	}
}

const continueOutput = `{
  "summary": "Half way", "report": "Imported 3 of 6 months.",
  "notify": {"send": false, "priority": "low", "title": ""},
  "memory": {"update": false, "content": ""},
  "questions": [],
  "actions": [{"type": "continue", "params": {"after_minutes": 5, "note": "import months 4 to 6"}, "reason": "the export is paged"}]
}`

func TestContinueWakesTheAgentWithItsNote(t *testing.T) {
	fr := &fakeRunner{output: continueOutput}
	e, s, _ := harnessWith(t, fr, "  continue: auto\n")
	if !strings.Contains(e.Current().Config.Agents["finance"].Charter, "spending") {
		t.Fatal("harness")
	}
	first := claim(t, e, s)
	if err := e.Execute(ctx, first); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fr.got.System, "- continue (auto)") {
		t.Fatalf("the continue action is not offered:\n%s", fr.got.System)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	acts, _ := s.ListActions(ctx, "finance", "", 0)
	if len(acts) != 1 || acts[0].Status != "executed" {
		t.Fatalf("actions = %+v", acts)
	}
	queued, _ := s.ListRuns(ctx, store.RunFilter{Agent: "finance", Status: "queued"})
	if len(queued) != 1 || queued[0].Trigger != "continue" || queued[0].Chain != 1 {
		t.Fatalf("queued = %+v", queued)
	}
	if c, _ := s.ClaimRun(ctx, "w"); c != nil {
		t.Fatal("a continuation ran before its delay")
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET run_after = now() WHERE id = $1`, queued[0].ID); err != nil {
		t.Fatal(err)
	}
	c, _ := s.ClaimRun(ctx, "w")
	if c == nil || c.Trigger != "continue" {
		t.Fatalf("claim = %+v", c)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Trigger: continue", "continuation 1 of at most 12", "import months 4 to 6"} {
		if !strings.Contains(fr.got.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, fr.got.Prompt)
		}
	}
}

func TestContinueIsDeniedAtTheChainLimit(t *testing.T) {
	fr := &fakeRunner{output: continueOutput}
	e, s, _ := harnessWith(t, fr, "  continue: auto\n")
	a := e.Current().Config.Agents["finance"]
	var parent string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO runs (agent, spec_sha, trigger, status, finished_at) VALUES ('finance', $1, 'manual', 'succeeded', now()) RETURNING id::text`, a.SHA).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO runs (agent, spec_sha, trigger, prompt, run_after, continues, chain)
		VALUES ('finance', $1, 'continue', 'n', now(), $2, 12)`, a.SHA, parent); err != nil {
		t.Fatal(err)
	}
	c, _ := s.ClaimRun(ctx, "w")
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	acts, _ := s.ListActions(ctx, "finance", "", 0)
	if len(acts) != 1 || acts[0].Status != "denied" || !strings.Contains(*acts[0].Error, "continuation limit") {
		t.Fatalf("actions = %+v", acts)
	}
}
