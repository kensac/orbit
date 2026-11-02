package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
)

func TestARunHasItsSkillsAndWritesItsOwn(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harnessWith(t, fr, "skills: [close]\n")
	e.Harness = "/usr/local/bin/orbit"
	if _, _, err := s.PutSkill(ctx, store.SkillWrite{Name: "close", Content: "---\nname: close\ndescription: Close a month.\n---\nSteps.\n"},
		store.SkillProvenance{Source: store.SkillByPerson}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(goodOutput), &m)
	m["skills"] = []any{
		map[string]any{"op": "write", "name": "close", "content": "---\nname: close\ndescription: Close a month, total first.\n---\nTotal first.\n", "note": "asked for total first"},
		map[string]any{"op": "write", "name": "review-subscriptions", "content": "---\nname: review-subscriptions\ndescription: Review recurring charges.\n---\nList them.\n", "note": "new"},
		map[string]any{"op": "retire", "name": "unknown", "content": "", "note": ""},
	}
	b, _ := json.Marshal(m)
	fr.output = string(b)
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fr.got.SkillsPlugin, "orbit-skills-") || !strings.Contains(fr.got.System, "You have: close (use the Skill tool") {
		t.Fatalf("plugin %q, system:\n%s", fr.got.SkillsPlugin, fr.got.System)
	}
	if !strings.Contains(string(fr.got.OutputSchema), `"skills"`) {
		t.Fatal("no skills in the backend's schema")
	}
	k, _ := s.GetSkill(ctx, "close", 0, true)
	if k.Version != 2 || k.Source != store.SkillByAgent || !strings.Contains(k.Content, "Total first.") {
		t.Fatalf("close = %+v", k)
	}
	if _, err := s.GetSkill(ctx, "review-subscriptions", 0, false); err != nil {
		t.Fatal(err)
	}
	if names, _ := s.AttachedNames(ctx, "finance", store.AttachSkill); len(names) != 2 {
		t.Fatalf("attached = %v", names)
	}
	arts, _ := s.ListArtifacts(ctx, c.ID)
	have := map[string]bool{}
	for _, a := range arts {
		have[a.Name] = true
	}
	if !have["skills.json"] || !have["skill-writes.json"] {
		t.Fatalf("artifacts = %v", have)
	}
	// The next run has both, at their new versions.
	fr.output = goodOutput
	if err := e.Execute(ctx, claim(t, e, s)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fr.got.System, "close, review-subscriptions") {
		t.Fatalf("next system:\n%s", fr.got.System)
	}
}
