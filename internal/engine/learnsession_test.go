package engine_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// Synthetic imported sessions only; nothing here is from a real transcript.
const (
	learnSessA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	learnSessB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// withBuiltins reloads e's configuration with the built-in agent, which learns from imported sessions, and records it.
func withBuiltins(t *testing.T, e *engine.Engine, s *store.Store) *spec.Agent {
	t.Helper()
	cfg, err := spec.LoadAll(e.ConfigDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	specs := map[string]store.AgentSpec{}
	for n, a := range cfg.Agents {
		b, _ := json.Marshal(a)
		specs[n] = store.AgentSpec{SHA: a.SHA, JSON: b, Enabled: true}
	}
	if err := s.SyncAgents(ctx, specs); err != nil {
		t.Fatal(err)
	}
	return cfg.Agents[spec.BuiltinName]
}

func sessionLines(at time.Time, texts ...string) []store.SessionLine {
	var out []store.SessionLine
	for i, t := range texts {
		typ, content := "user", fmt.Sprintf("%q", t)
		if i%2 == 1 {
			typ, content = "assistant", fmt.Sprintf(`[{"type":"text","text":%q},{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"make release"}}]`, t, i)
		}
		ts := at.Add(time.Duration(i) * time.Second)
		c := fmt.Sprintf(`{"message":{"content":%s,"role":%q},"timestamp":%q,"type":%q,"uuid":"u%d"}`, content, typ, ts.Format(time.RFC3339), typ, i)
		l := store.SessionLine{Content: []byte(c), Type: typ, At: &ts}
		if typ == "user" {
			l.Prompt = t
		}
		out = append(out, l)
	}
	return out
}

func importSession(t *testing.T, s *store.Store, id string, from int64, lines []store.SessionLine) string {
	t.Helper()
	prefix := ""
	if from > 0 {
		prefix = strings.Repeat("1", 64)
	}
	res, err := s.ImportSessions(ctx, "claude-code", []store.SessionChunk{{SessionID: id, FromOffset: from, FromPrefix: prefix,
		ToOffset: from + 100, ToPrefix: strings.Repeat("1", 64), Lines: lines}}, false)
	if err != nil || res[0].Outcome == store.SessionConflict {
		t.Fatalf("import %s: %+v %v", id, res, err)
	}
	return res[0].ID
}

// learnNext enqueues learning and runs the one learning run it queued, returning it.
func learnNext(t *testing.T, e *engine.Engine, s *store.Store) *store.Run {
	t.Helper()
	if err := e.EnqueueLearning(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRunFor(ctx, store.Claim{Worker: "w", Any: true})
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if c.Trigger != store.TriggerLearn {
		t.Fatalf("claimed a %s run", c.Trigger)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetRun(ctx, c.ID)
	return r
}

func TestImportedSessionsAreLearnedOnceAndProposeSkills(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harness(t, fr)
	withBuiltins(t, e, s)
	now := time.Now()
	idA := importSession(t, s, learnSessA, 0, sessionLines(now.Add(-time.Hour), "Cut the widget release", "Tagged and pushed.", "Prefer squash merges here", "Noted."))
	idB := importSession(t, s, learnSessB, 0, sessionLines(now.Add(-3*time.Hour), "Release the gizmo too", "Tagged and pushed."))

	// The newest session first, alone: nothing else is queued meanwhile.
	fr.output = `{"summary":"Kept the merge preference","entries":[{"op":"add","ref":"profile","type":"feedback","title":"Squash merges","description":"","body":"Squash-merge PRs.","why":"","how_to_apply":"","superseded_by":"","note":""}],"procedures":["release: make release, tag, push"],"skills":[]}`
	first := learnNext(t, e, s)
	if first.Status != "succeeded" || !strings.Contains(*first.Report, "Procedures:") || !strings.Contains(fr.got.Prompt, "Cut the widget release") ||
		!strings.Contains(fr.got.Prompt, "[tool Bash: make release]") || strings.Contains(fr.got.Prompt, "gizmo") {
		t.Fatalf("first = %s %v\nprompt:\n%s", first.Status, first.Report, fr.got.Prompt)
	}
	if !strings.Contains(fr.got.Prompt, "id "+idA) {
		t.Fatalf("the first learning run read another session:\n%s", fr.got.Prompt)
	}

	// The second sees the first's procedures and may propose a skill from both; a proposal from one session only is refused in code.
	skill := "---\nname: cut-release\ndescription: Use when cutting a release.\n---\n1. make release\n2. tag and push\n"
	b, _ := json.Marshal(map[string]any{"summary": "Saw the release procedure again", "entries": []any{}, "procedures": []string{"release"},
		"skills": []any{
			map[string]any{"name": "cut-release", "version": 0, "content": skill, "sessions": []string{idB, idA}, "why": "done in both"},
			map[string]any{"name": "lonely", "version": 0, "content": "---\nname: lonely\ndescription: d\n---\nx", "sessions": []string{idB, "cccccccc-cccc-4ccc-8ccc-cccccccccccc"}, "why": ""},
		}})
	fr.output = string(b)
	second := learnNext(t, e, s)
	if second.Status != "succeeded" || !strings.Contains(fr.got.Prompt, "Session "+idA) || !strings.Contains(fr.got.Prompt, "release: make release, tag, push") {
		t.Fatalf("second = %s\nprompt:\n%s", second.Status, fr.got.Prompt)
	}
	if !strings.Contains(*second.Report, "cut-release (version 0, from sessions "+idB+", "+idA+"): waits for approval") ||
		!strings.Contains(*second.Report, "lonely: refused: session cccccccc-cccc-4ccc-8ccc-cccccccccccc was not one shown") {
		t.Fatalf("report = %s", *second.Report)
	}
	if k, err := s.GetSkill(ctx, "cut-release", 0, false); err == nil {
		t.Fatalf("a proposed skill was applied without approval: %+v", k)
	}
	acts, _ := s.ListActions(ctx, spec.BuiltinName, "pending", 0)
	if len(acts) != 1 || acts[0].Type != "skill:write" {
		t.Fatalf("pending = %+v", acts)
	}
	if err := s.DecideAction(ctx, acts[0].ID, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	k, err := s.GetSkill(ctx, "cut-release", 0, false)
	if err != nil || k.Source != store.SkillByAgent || *k.SourceRunID != second.ID {
		t.Fatalf("approved skill = %+v %v", k, err)
	}

	// Learned once per point: nothing is due until a session grows.
	if err := e.EnqueueLearning(ctx); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.ClaimRunFor(ctx, store.Claim{Worker: "w", Any: true}); c != nil {
		t.Fatalf("a learned session was queued again: %+v", c)
	}
	importSession(t, s, learnSessA, 100, sessionLines(now.Add(-40*time.Minute), "Also bump the changelog"))
	fr.output = `{"summary":"nothing","entries":[],"procedures":[],"skills":[]}`
	third := learnNext(t, e, s)
	if third.Status != "succeeded" || !strings.Contains(fr.got.Prompt, "Also bump the changelog") || strings.Contains(fr.got.Prompt, "Cut the widget release") {
		t.Fatalf("third = %s\nprompt:\n%s", third.Status, fr.got.Prompt)
	}

	// On demand, whatever the horizon; again for the same point is a conflict.
	if _, err := e.LearnSessionNow(ctx, idA); err == nil {
		t.Fatal("learning an already learned point again was accepted")
	}
}

func TestASessionWindowWithoutThePersonIsSkipped(t *testing.T) {
	fr := &fakeRunner{output: `{"summary":"x","entries":[],"procedures":[],"skills":[]}`}
	e, s, _ := harness(t, fr)
	withBuiltins(t, e, s)
	at := time.Now().Add(-2 * time.Hour)
	line := fmt.Sprintf(`{"message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}],"role":"user"},"timestamp":%q,"type":"user"}`, at.Format(time.RFC3339))
	id := importSession(t, s, learnSessA, 0, []store.SessionLine{{Content: []byte(line), Type: "user", At: &at}})
	if _, err := e.LearnSessionNow(ctx, id); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRunFor(ctx, store.Claim{Worker: "w", Any: true})
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetRun(ctx, c.ID)
	if r.Status != store.StatusSkipped || fr.calls != 0 {
		t.Fatalf("run = %s, backend calls %d", r.Status, fr.calls)
	}
}
