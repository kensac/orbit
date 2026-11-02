package harness

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// ToolSkill is the tool that serves a skill's full text and files.
const ToolSkill = "mcp__orbit__skill"

// SkillsDir is where the engine writes a run's skills in full, under the state directory the tool server is given. The CLI loads only a stub of each (engine/skills.go); the model reads the text through ToolSkill, so what it reads is exactly the version the run was given, and a skill's files are reachable without opening any path outside the workdir.
func SkillsDir(state string) string { return filepath.Join(state, "skills") }

// skillReadMax bounds one read: a SKILL.md is at most 256 KB, and a larger file is read in parts.
const skillReadMax = 256 << 10

// SkillSchema is the skill tool's input.
var SkillSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"name": map[string]any{"type": "string", "description": "The skill's name, as its stub names it."},
		"file": map[string]any{"type": "string", "description": "A file of the skill, as SKILL.md lists it; omit for SKILL.md itself."},
		"offset": map[string]any{"type": "integer", "minimum": 0,
			"description": "Byte offset to read a large file from; the result says when there is more."},
	},
	"required":             []string{"name"},
	"additionalProperties": false,
}

// skillsAvailable reports whether the run was given any skills.
func skillsAvailable(state string) bool {
	if state == "" {
		return false
	}
	st, err := os.Stat(SkillsDir(state))
	return err == nil && st.IsDir()
}

// ReadSkill returns a skill's SKILL.md (file empty) with the list of its files, or one of its files from offset. Paths are confined to the skill's directory after symlinks.
func ReadSkill(state, name, file string, offset int64) (map[string]any, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return nil, fmt.Errorf("no skill named %q", name)
	}
	root := filepath.Join(SkillsDir(state), name)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("no skill named %q in this run", name)
	}
	rel := "SKILL.md"
	if file != "" {
		rel = filepath.Clean(filepath.FromSlash(file))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("file %q is outside the skill", file)
		}
	}
	p := filepath.Join(root, rel)
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, fmt.Errorf("skill %s has no file %q", name, file)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil || !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
		return nil, fmt.Errorf("file %q is outside the skill", file)
	}
	f, err := os.Open(real) //nolint:gosec // confined to the skill's directory above
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return nil, fmt.Errorf("skill %s has no file %q", name, file)
	}
	if offset < 0 || offset > st.Size() {
		return nil, fmt.Errorf("offset %d is past the end (%d bytes)", offset, st.Size())
	}
	buf := make([]byte, min(int64(skillReadMax), st.Size()-offset))
	if n, err := f.ReadAt(buf, offset); err != nil && (!errors.Is(err, io.EOF) || n < len(buf)) {
		return nil, err
	}
	content := string(buf)
	if !textPrefix(buf) && offset == 0 {
		content = fmt.Sprintf("(a binary file of %d bytes: not readable as text)", st.Size())
		buf = buf[:0]
	}
	out := map[string]any{"skill": name, "file": filepath.ToSlash(rel), "bytes": st.Size(), "content": content}
	if end := offset + int64(len(buf)); len(buf) > 0 && end < st.Size() {
		out["next_offset"] = end
	}
	if file == "" {
		var files []string
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if r, _ := filepath.Rel(root, path); r != "SKILL.md" {
					files = append(files, filepath.ToSlash(r))
				}
			}
			return nil
		})
		sort.Strings(files)
		out["files"] = files
		out["base_dir"] = "(served by " + ToolSkill + ": pass a file's path as file)"
	}
	return out, nil
}

// textPrefix reports whether b is UTF-8 but for a rune its end cut in two (a part of a large file).
func textPrefix(b []byte) bool {
	for cut := 0; cut <= 3 && cut <= len(b); cut++ {
		if utf8.Valid(b[:len(b)-cut]) {
			return true
		}
	}
	return false
}
