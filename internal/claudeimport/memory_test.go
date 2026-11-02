package claudeimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadMemoryDir(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "MEMORY.md", "# Memory index\n\n- [Tea, not coffee](tea_pref.md) - drinks tea\n")
	// Newer Claude Code: type under metadata, Why and How to apply paragraphs.
	write(t, dir, "tea_pref.md", `---
name: tea_pref
description: Prefers tea over coffee
metadata:
  node_type: memory
  type: feedback
  originSessionId: 00000000-0000-0000-0000-000000000000
---

Offer tea, never coffee.

**Why:** Coffee keeps them up.

**How to apply:** When suggesting a drink, suggest tea.
Even in the afternoon.
`)
	// Older Claude Code: type at the top level, no reasons.
	write(t, dir, "home-city.md", "---\nname: home-city\ndescription: Lives in a city\ntype: user\n---\n\nLives in a city by the sea.\n")
	// Not YAML, as Claude Code sometimes writes it: read leniently.
	write(t, dir, "quoted.md", "---\nname: quoted\ndescription: \"roll\" / \"rolled\" means roll\nmetadata:\n  type: project\n---\n\nRoll means roll.\n")
	write(t, dir, "notes.md", "No frontmatter here.\n")
	write(t, dir, "odd.md", "---\nname: odd\ntype: opinion\n---\n\nx\n")

	mems, warnings, err := ReadMemoryDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 3 || len(warnings) != 2 {
		t.Fatalf("read %d, warnings %v", len(mems), warnings)
	}
	by := map[string]Memory{}
	for _, m := range mems {
		by[m.Key] = m
	}
	tea := by["tea-pref"]
	if tea.Type != "feedback" || tea.Title != "Tea, not coffee" || tea.Description != "Prefers tea over coffee" ||
		tea.Body != "Offer tea, never coffee." || tea.Why != "Coffee keeps them up." ||
		tea.HowToApply != "When suggesting a drink, suggest tea.\nEven in the afternoon." {
		t.Fatalf("tea = %+v", tea)
	}
	if c := by["home-city"]; c.Type != "user" || c.Title != "Home city" || c.Why != "" {
		t.Fatalf("home-city = %+v", c)
	}
	if q := by["quoted"]; q.Type != "project" || !strings.HasPrefix(q.Description, `"roll"`) {
		t.Fatalf("quoted = %+v", q)
	}
}

func TestSplitReasonsKeepsTheRest(t *testing.T) {
	rest, why, how := splitReasons("Intro.\n\n**How to apply:** do it\n\n**Why:** because\n")
	if rest != "Intro." || why != "because" || how != "do it" {
		t.Fatalf("rest %q why %q how %q", rest, why, how)
	}
}
