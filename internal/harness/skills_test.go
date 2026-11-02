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

	r, err := ReadSkill(state, "close", "", 0)
	if err != nil || r["content"] != "steps" || len(r["files"].([]string)) != 3 {
		t.Fatalf("SKILL.md = %v %v", r, err)
	}
	if r, err := ReadSkill(state, "close", "references/a.md", 0); err != nil || r["content"] != "A" {
		t.Fatalf("file = %v %v", r, err)
	}
	if r, _ := ReadSkill(state, "close", "logo.png", 0); !strings.Contains(r["content"].(string), "binary") {
		t.Fatalf("binary = %v", r)
	}
	for _, bad := range []string{"../../secret", "/etc/passwd", "link", "references"} {
		if _, err := ReadSkill(state, "close", bad, 0); err == nil {
			t.Errorf("read %q was allowed", bad)
		}
	}
	if _, err := ReadSkill(state, "../close", "", 0); err == nil {
		t.Error("a name with a slash was read")
	}
	if skillsAvailable(t.TempDir()) || !skillsAvailable(state) {
		t.Error("skillsAvailable")
	}
}
