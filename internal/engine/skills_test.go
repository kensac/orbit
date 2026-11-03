package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/actions"
	orbitharness "github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

func skillWriteOutput(writes ...map[string]any) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(goodOutput), &m)
	var acts []any
	for _, w := range writes {
		acts = append(acts, map[string]any{"type": "skill:write", "params": w, "reason": "a procedure worth keeping"})
	}
	m["actions"] = acts
	b, _ := json.Marshal(m)
	return string(b)
}

// TestARunHasItsSkillsAndWritesThemUnderARule: a run is given its skills; its skill:write proposals are resolved by code against the versions it had (a retire of one it lacks, or a fourth write, is denied), carry what they change, and under auto apply when the action loop runs them, attached to the agent.
func TestARunHasItsSkillsAndWritesThemUnderARule(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harnessWith(t, fr, "  skill:write: auto\nskills: [close]\n")
	e.Harness = "/usr/local/bin/orbit"
	if _, _, err := s.PutSkill(ctx, store.SkillWrite{Name: "close", Content: "---\nname: close\ndescription: Close a month.\n---\nSteps.\n"},
		store.SkillProvenance{Source: store.SkillByPerson}); err != nil {
		t.Fatal(err)
	}
	fr.output = skillWriteOutput(
		map[string]any{"op": "write", "name": "close", "content": "---\nname: close\ndescription: Close a month, total first.\n---\nTotal first.\n", "note": "asked for total first"},
		map[string]any{"op": "write", "name": "review-subscriptions", "content": "---\nname: review-subscriptions\ndescription: Review recurring charges.\n---\nList them.\n", "note": "new"},
		map[string]any{"op": "retire", "name": "unknown", "content": "", "note": ""},
		map[string]any{"op": "write", "name": "fourth", "content": "---\nname: fourth\ndescription: d\n---\nx\n", "note": ""},
	)
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fr.got.SkillsPlugin, "orbit-skills-") || !strings.Contains(fr.got.System, "You have: close (use the Skill tool") ||
		!strings.Contains(fr.got.System, "Write or retire one with the skill:write action") {
		t.Fatalf("plugin %q, system:\n%s", fr.got.SkillsPlugin, fr.got.System)
	}
	if strings.Contains(string(fr.got.OutputSchema), `"skills"`) {
		t.Fatal("the output still has a skills property; writes are actions")
	}
	run, _ := s.ThreadRunByID(ctx, c.ID)
	status := []string{}
	for _, a := range run.Actions {
		if a.Type == "skill:write" {
			status = append(status, a.Status)
		}
	}
	if strings.Join(status, ",") != "approved,approved,denied,denied" {
		t.Fatalf("skill:write statuses = %v", status)
	}
	var first actions.SkillWrite
	for _, a := range run.Actions {
		if a.Type == "skill:write" {
			_ = json.Unmarshal(a.Params, &first)
			break
		}
	}
	if first.Version != 1 || first.Changes == nil || first.Changes.From != 1 || first.Changes.Added != 2 || first.Changes.Removed != 2 ||
		!strings.Contains(first.Changes.Diff, "+Total first.") || !strings.Contains(first.Changes.Diff, "-Steps.") {
		t.Fatalf("the first write's params = %+v", first)
	}
	// Proposed is not applied: the action loop applies them.
	if k, _ := s.GetSkill(ctx, "close", 0, false); k.Version != 1 {
		t.Fatalf("close was written before its action ran: %+v", k)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	k, _ := s.GetSkill(ctx, "close", 0, true)
	if k.Version != 2 || k.Source != store.SkillByAgent || *k.SourceRunID != c.ID || !strings.Contains(k.Content, "Total first.") {
		t.Fatalf("close = %+v", k)
	}
	if _, err := s.GetSkill(ctx, "review-subscriptions", 0, false); err != nil {
		t.Fatal(err)
	}
	if names, _ := s.AttachedNames(ctx, "finance", store.AttachSkill); len(names) != 2 {
		t.Fatalf("attached = %v", names)
	}
	acts, _ := s.ListActions(ctx, "finance", "executed", 0)
	executed := 0
	for _, a := range acts {
		if a.Type == "skill:write" {
			executed++
			var res struct {
				Outcome string                `json:"outcome"`
				Changes *actions.SkillChanges `json:"changes"`
			}
			if json.Unmarshal(a.Result, &res) != nil || res.Outcome != "written" || res.Changes == nil {
				t.Fatalf("result = %s", a.Result)
			}
		}
	}
	if executed != 2 {
		t.Fatalf("executed skill writes = %d", executed)
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

// TestASkillWriteWaitsWithItsScriptsListed: under approve a skill:write waits, and the approval names the change and every script or executable the skill holds; an agent that does not name skill:write is neither offered it nor told about writing skills.
func TestASkillWriteWaitsWithItsScriptsListed(t *testing.T) {
	fr := &fakeRunner{}
	e, s, h := harnessWith(t, fr, "  skill:write: approve\nskills: [deploy]\n")
	e.Harness = "/usr/local/bin/orbit"
	if _, _, err := s.PutSkill(ctx, store.SkillWrite{Name: "deploy", Content: "---\nname: deploy\ndescription: Deploy.\n---\nRun scripts/go.sh.\n",
		Files: []store.SkillFile{{Path: "scripts/go.sh", Content: []byte("echo")}, {Path: "tools/check", Content: []byte("#!/usr/bin/env python3\n")},
			{Path: "references/notes.md", Content: []byte("# Notes\n")}}}, store.SkillProvenance{Source: store.SkillByPerson}); err != nil {
		t.Fatal(err)
	}
	fr.output = skillWriteOutput(map[string]any{"op": "write", "name": "deploy", "content": "---\nname: deploy\ndescription: Deploy.\n---\nRun scripts/go.sh, then tools/check.\n", "note": "check after"})
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	run, _ := s.ThreadRunByID(ctx, c.ID)
	var pending []string
	for _, a := range run.Actions {
		if a.Type == "skill:write" {
			pending = append(pending, a.Status)
		}
	}
	if len(pending) != 1 || pending[0] != "pending" {
		t.Fatalf("skill:write = %v", pending)
	}
	h.mu.Lock()
	var approval string
	for _, m := range h.messages {
		if strings.Contains(m["title"].(string), "skill:write") {
			approval = m["body"].(string)
		}
	}
	h.mu.Unlock()
	for _, want := range []string{"Writes skill deploy (version 1 to 2): SKILL.md +1 -1 lines; its 3 other files stay.",
		"Scripts and executables in this skill (they run only if the agent's tools let them, when SKILL.md says to): scripts/go.sh, tools/check",
		"+Run scripts/go.sh, then tools/check."} {
		if !strings.Contains(approval, want) {
			t.Fatalf("approval lacks %q:\n%s", want, approval)
		}
	}
	if strings.Contains(approval, "references/notes.md,") {
		t.Fatalf("a reference is listed as a script:\n%s", approval)
	}

	fr2 := &fakeRunner{output: goodOutput}
	e2, s2, _ := harnessWith(t, fr2, "")
	if err := e2.Execute(ctx, claim(t, e2, s2)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fr2.got.System, "skill:write") || strings.Contains(string(fr2.got.OutputSchema), "skill:write") {
		t.Fatalf("an agent without the rule is offered skill writes:\n%s", fr2.got.System)
	}
}

func TestARunGetsItsAgentsHooks(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, "hooks:\n  - {event: Stop, say: Check every figure.}\n  - {event: PostToolUse, matcher: Write, run: [gofmt, -l, .]}\n")
	e.Harness = "/usr/local/bin/orbit"
	if err := e.Execute(ctx, claim(t, e, s)); err != nil {
		t.Fatal(err)
	}
	h := fr.got.Harness
	if h == nil || len(h.AgentHooks) != 2 || h.AgentHooks[0].Def.Say != "Check every figure." || h.AgentHooks[1].Matcher != "Write" ||
		h.AgentHooks[1].Def.Timeout.Seconds() != 10 {
		t.Fatalf("hooks = %+v", h)
	}
}

// TestBuiltInSkillsCarryTheHowTo: a Claude Code run with orbit's tools gets the built-in skills (query, calc, entries, actions, and continuations when it may continue) as stubs in its plugin and in full for the skill tool, references included; its system contract states the rules and points to them, shorter than a run without them; their names cannot be written or stored.
func TestBuiltInSkillsCarryTheHowTo(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harnessWith(t, fr, "  continue: auto\n  skill:write: auto\n")
	e.Harness = "/usr/local/bin/orbit"
	var stubs, full []string
	var reference map[string]any
	fr.seen = func(req runner.Request) {
		dirs, _ := os.ReadDir(filepath.Join(req.SkillsPlugin, "skills"))
		for _, d := range dirs {
			stubs = append(stubs, d.Name())
		}
		state := orbitharness.StateDir(req.Session.Home)
		dirs, _ = os.ReadDir(orbitharness.SkillsDir(state))
		for _, d := range dirs {
			full = append(full, d.Name())
		}
		reference, _ = orbitharness.ReadSkill(state, "orbit-query", "", "examples", 0)
	}
	fr.output = skillWriteOutput(map[string]any{"op": "write", "name": "orbit-query", "content": "---\nname: orbit-query\ndescription: mine\n---\nx\n", "note": ""})
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	want := "orbit-actions,orbit-calc,orbit-continuations,orbit-entries,orbit-query"
	if strings.Join(stubs, ",") != want || strings.Join(full, ",") != want {
		t.Fatalf("plugin stubs %v, full copies %v", stubs, full)
	}
	if reference == nil || !strings.Contains(reference["content"].(string), "Spend per category") {
		t.Fatalf("the orbit-query examples reference = %v", reference)
	}
	withSkills := fr.got.System
	for _, wantText := range []string{"The orbit-entries skill says how", "the orbit-query and orbit-calc skills say how", "The orbit-actions skill says how"} {
		if !strings.Contains(withSkills, wantText) {
			t.Fatalf("system lacks %q:\n%s", wantText, withSkills)
		}
	}
	if strings.Contains(withSkills, "- add: ref is where it goes") {
		t.Fatalf("the entries how-to is still in the contract:\n%s", withSkills)
	}
	run, _ := s.ThreadRunByID(ctx, c.ID)
	for _, a := range run.Actions {
		if a.Type == "skill:write" && a.Status != "denied" {
			t.Fatalf("a write to a built-in skill = %s", a.Status)
		}
	}
	ins, _ := s.AttemptArtifacts(ctx, c.ID, c.Attempt, "in")
	found := false
	for _, a := range ins {
		if a.Name == "skills.json" {
			found = true
			if !strings.Contains(string(a.Content), `"name":"orbit-query","version":0`) {
				t.Fatalf("skills.json = %s", a.Content)
			}
		}
	}
	if !found {
		t.Fatal("no skills.json")
	}
	if _, _, err := s.PutSkill(ctx, store.SkillWrite{Name: "orbit-calc", Content: "---\nname: orbit-calc\ndescription: d\n---\n"}, store.SkillProvenance{Source: store.SkillByPerson}); err == nil {
		t.Fatal("the library stored a skill under a built-in's name")
	}

	// Without orbit's tools the run gets none, and the contract keeps the how-to.
	e.Harness = ""
	fr.seen, fr.output = nil, goodOutput
	if err := e.Execute(ctx, claim(t, e, s)); err != nil {
		t.Fatal(err)
	}
	if fr.got.SkillsPlugin != "" || !strings.Contains(fr.got.System, "- add: ref is where it goes") {
		t.Fatalf("without the harness: plugin %q, system:\n%s", fr.got.SkillsPlugin, fr.got.System)
	}
	t.Logf("system contract: %d bytes with the built-in skills, %d without", len(withSkills), len(fr.got.System))
}
