package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/store"
)

func TestWriteSkillsReconcilesByDigest(t *testing.T) {
	home := t.TempDir()
	k := store.Skill{Name: "close", Description: "Use to close a month.", SHA256: "a", Content: "---\nname: close\ndescription: x\n---\nSteps.",
		Files: []store.SkillFile{{Path: "references/a.md", Content: []byte("A")}}}
	dir, err := writeSkills(home, []store.Skill{k}, true)
	if err != nil {
		t.Fatal(err)
	}
	stub, _ := os.ReadFile(filepath.Join(dir, "skills", "close", "SKILL.md"))
	if !strings.Contains(string(stub), "description: \"Use to close a month.\"") || !strings.Contains(string(stub), harness.ToolSkill) || strings.Contains(string(stub), "Steps.") {
		t.Fatalf("stub = %s", stub)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatal(err)
	}
	full, _ := os.ReadFile(filepath.Join(harness.SkillsDir(harness.StateDir(home)), "close", "references", "a.md"))
	if string(full) != "A" {
		t.Fatalf("full file = %q", full)
	}
	// A changed set is a new directory; the old one is removed.
	k.SHA256 = "b"
	dir2, _ := writeSkills(home, []store.Skill{k}, true)
	if dir2 == dir {
		t.Fatal("a changed skill kept the plugin directory, so a live process would not notice")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the old plugin directory was kept")
	}
	// No skills: nothing is left.
	if d, _ := writeSkills(home, nil, true); d != "" {
		t.Fatal("a plugin for no skills")
	}
	if left, _ := filepath.Glob(filepath.Join(home, ".claude", skillsPluginPrefix+"*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	// Without orbit's tool the plugin carries the text itself.
	d3, _ := writeSkills(home, []store.Skill{k}, false)
	if b, _ := os.ReadFile(filepath.Join(d3, "skills", "close", "SKILL.md")); !strings.Contains(string(b), "Steps.") {
		t.Fatalf("plugin without the tool = %s", b)
	}
}
