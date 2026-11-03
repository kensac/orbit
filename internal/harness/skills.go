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
		"name":      map[string]any{"type": "string", "description": "The skill's name, as its stub names it."},
		"file":      map[string]any{"type": "string", "description": "A file of the skill, as SKILL.md lists it; omit for SKILL.md itself."},
		"reference": map[string]any{"type": "string", "description": "One of the skill's references by name, as reading its SKILL.md lists them (references/<name>.md); in place of file."},
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

// referencesDir is where a skill keeps its references, as Claude Code's skills do: documents SKILL.md points to, read when a step needs one.
const referencesDir = "references"

// referenceTitleScan bounds how much of a reference is read for its first heading.
const referenceTitleScan = 8 << 10

// ReadSkill returns a skill's SKILL.md (file and reference empty) with the list of its files and its named references, or one of its files, or one reference by name, from offset. Paths are confined to the skill's directory after symlinks.
func ReadSkill(state, name, file, reference string, offset int64) (map[string]any, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return nil, fmt.Errorf("no skill named %q", name)
	}
	root := filepath.Join(SkillsDir(state), name)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("no skill named %q in this run", name)
	}
	if reference != "" {
		if file != "" {
			return nil, errors.New("give file or reference, not both")
		}
		file = referencesDir + "/" + reference + ".md"
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(file))); err != nil {
			file = referencesDir + "/" + reference
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(file))); err != nil {
			return nil, fmt.Errorf("skill %s has no reference %q", name, reference)
		}
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
		out["references"] = skillReferences(root)
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

// SkillReference is one of a skill's references as its SKILL.md read lists it: a name to load it by, and its first Markdown heading.
type SkillReference struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

// skillReferences lists the regular files under a skill's references/, by name (the path below it, without .md), each with its first heading ("" for none, or for a binary file).
func skillReferences(root string) []SkillReference {
	out := []SkillReference{}
	base := filepath.Join(root, referencesDir)
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return nil
		}
		ref := SkillReference{Name: strings.TrimSuffix(filepath.ToSlash(rel), ".md")}
		if f, err := os.Open(p); err == nil { //nolint:gosec // a regular file under the skill's own directory, found by walking it
			buf := make([]byte, referenceTitleScan)
			n, _ := io.ReadFull(f, buf)
			_ = f.Close()
			if textPrefix(buf[:n]) {
				ref.Title = firstHeading(string(buf[:n]))
			}
		}
		out = append(out, ref)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// firstHeading is the text of a Markdown document's first ATX heading ("# Title"), outside code fences.
func firstHeading(doc string) string {
	fence := false
	for _, line := range strings.Split(doc, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = !fence
			continue
		}
		if fence || !strings.HasPrefix(t, "#") {
			continue
		}
		h := strings.TrimLeft(t, "#")
		if len(t)-len(h) <= 6 && (h == "" || h[0] == ' ' || h[0] == '\t') {
			return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(h), "#"))
		}
	}
	return ""
}
