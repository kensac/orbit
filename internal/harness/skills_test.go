package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSkill(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(SkillsDir(state), "close")
	_ = os.MkdirAll(filepath.Join(dir, "references"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("steps"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "references", "a.md"), []byte("A"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "logo.png"), []byte{0x89, 'P', 'N', 'G', 0xff, 0xfe}, 0o600)
	_ = os.WriteFile(filepath.Join(state, "secret"), []byte("no"), 0o600)
	_ = os.Symlink(filepath.Join(state, "secret"), filepath.Join(dir, "link"))

	r, err := ReadSkill(state, "close", "", "", 0)
	if err != nil || r["content"] != "steps" || len(r["files"].([]string)) != 3 {
		t.Fatalf("SKILL.md = %v %v", r, err)
	}
	if r, err := ReadSkill(state, "close", "references/a.md", "", 0); err != nil || r["content"] != "A" {
		t.Fatalf("file = %v %v", r, err)
	}
	if r, _ := ReadSkill(state, "close", "logo.png", "", 0); !strings.Contains(r["content"].(string), "binary") {
		t.Fatalf("binary = %v", r)
	}
	for _, bad := range []string{"../../secret", "/etc/passwd", "link", "references"} {
		if _, err := ReadSkill(state, "close", bad, "", 0); err == nil {
			t.Errorf("read %q was allowed", bad)
		}
	}
	if _, err := ReadSkill(state, "../close", "", "", 0); err == nil {
		t.Error("a name with a slash was read")
	}
	if skillsAvailable(t.TempDir()) || !skillsAvailable(state) {
		t.Error("skillsAvailable")
	}
}

// TestASkillsReferencesAreNamedAndLoadable: reading SKILL.md lists each file under references/ by name with its first heading (none for a file without one), and reference loads one by that name.
func TestASkillsReferencesAreNamedAndLoadable(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(SkillsDir(state), "close")
	_ = os.MkdirAll(filepath.Join(dir, "references", "forms"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("steps"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "references", "format.md"), []byte("Intro\n```\n# not a heading\n```\n## Report format ##\nbody"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "references", "forms", "w2.md"), []byte("# W-2 fields\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "references", "rates.csv"), []byte("a,b\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "references", "large.md"), []byte(strings.Repeat("x", skillReadMax)+"tail"), 0o600)
	_ = os.WriteFile(filepath.Join(state, "secret"), []byte("no"), 0o600)
	_ = os.Symlink(filepath.Join(state, "secret"), filepath.Join(dir, "references", "leak.md"))

	r, err := ReadSkill(state, "close", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	refs := r["references"].([]SkillReference)
	want := []SkillReference{{"format", "Report format"}, {"forms/w2", "W-2 fields"}, {"large", ""}, {"rates.csv", ""}}
	if len(refs) != len(want) {
		t.Fatalf("references = %+v", refs)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("references = %+v, want %+v", refs, want)
		}
	}
	if r, err := ReadSkill(state, "close", "", "format", 0); err != nil || !strings.HasPrefix(r["content"].(string), "Intro") || r["file"] != "references/format.md" {
		t.Fatalf("reference format = %v %v", r, err)
	}
	if r, err := ReadSkill(state, "close", "", "rates.csv", 0); err != nil || r["content"] != "a,b\n" {
		t.Fatalf("reference rates.csv = %v %v", r, err)
	}
	if r, err := ReadSkill(state, "close", "", "large", 0); err != nil || len(r["content"].(string)) != skillReadMax || r["next_offset"] != int64(skillReadMax) {
		t.Fatalf("first large reference part = %v %v", r, err)
	}
	if r, err := ReadSkill(state, "close", "", "large", skillReadMax); err != nil || r["content"] != "tail" || r["next_offset"] != nil {
		t.Fatalf("last large reference part = %v %v", r, err)
	}
	for _, bad := range []string{"leak", "missing", "../../../secret"} {
		if _, err := ReadSkill(state, "close", "", bad, 0); err == nil {
			t.Errorf("reference %q was read", bad)
		}
	}
	if _, err := ReadSkill(state, "close", "SKILL.md", "format", 0); err == nil {
		t.Error("file and reference together were accepted")
	}
}
