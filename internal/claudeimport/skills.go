package claudeimport

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kensac/orbit/internal/frontmatter"
	"github.com/kensac/orbit/internal/store"
)

// Skill is one Claude Code skill directory: its SKILL.md and every other file under it.
type Skill struct {
	Name    string            `json:"name"`
	Content string            `json:"content"`
	Files   []store.SkillFile `json:"files"`
}

// skipNames are files and directories that are a tool's droppings, not part of a skill.
var skipNames = map[string]bool{"__pycache__": true, ".git": true, "node_modules": true, ".venv": true}

// junkNames are what an operating system leaves beside a person's files: Finder's view settings and the folders macOS keeps on a volume, Windows' thumbnail cache and folder settings, and the folder a macOS zip holds resource forks in.
var junkNames = map[string]bool{
	".DS_Store": true, ".localized": true, ".AppleDouble": true, ".Spotlight-V100": true, ".Trashes": true,
	".fseventsd": true, ".TemporaryItems": true, "__MACOSX": true, "Thumbs.db": true, "desktop.ini": true,
}

// Junk reports whether a file or directory name is an operating system's dropping rather than something a person wrote: one of junkNames, or an AppleDouble file ("._name", the resource fork and extended attributes macOS writes beside a file on a volume or into a tar it makes). An import skips these by name, whatever they contain.
func Junk(name string) bool {
	return junkNames[name] || strings.HasPrefix(name, "._")
}

// ReadSkillsDir reads each directory directly under dir that holds a SKILL.md, as Claude Code's ~/.claude/skills and a project's .claude/skills are laid out. A directory without one (a plugin, a sync bucket) or a skill orbit cannot take (a name or size out of bounds, a symlink) is skipped with a warning naming it; an operating system's droppings (Junk) are left out without one.
func ReadSkillsDir(dir string) ([]Skill, []string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var out []Skill
	var warnings []string
	for _, e := range ents {
		if !e.IsDir() || skipNames[e.Name()] || Junk(e.Name()) {
			continue
		}
		k, err := ReadSkill(filepath.Join(dir, e.Name()))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: skipped: %v", e.Name(), err))
			continue
		}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, warnings, nil
}

// ReadSkill reads one skill directory, without tool and operating system droppings (skipNames, Junk). Its name is SKILL.md's frontmatter name, or the directory's.
func ReadSkill(dir string) (Skill, error) {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Skill{}, fmt.Errorf("no SKILL.md")
	}
	name := filepath.Base(dir)
	if fm, _, err := frontmatter.Split(b); err == nil {
		if n := frontmatter.Fields(fm)["name"]; n != "" {
			name = n
		}
	}
	if !store.SkillNameRE.MatchString(name) {
		return Skill{}, fmt.Errorf("name %q must match %s", name, store.SkillNameRE)
	}
	if _, err := store.SkillDescription(name, string(b)); err != nil {
		return Skill{}, err
	}
	k := Skill{Name: name, Content: string(b), Files: []store.SkillFile{}}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipNames[d.Name()] || (p != dir && Junk(d.Name())) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; orbit stores a skill's own files only", p)
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "SKILL.md" || strings.HasSuffix(rel, ".pyc") {
			return nil
		}
		c, err := os.ReadFile(p) //nolint:gosec // a path under the directory the operator named
		if err != nil {
			return err
		}
		k.Files = append(k.Files, store.SkillFile{Path: rel, Content: c})
		return nil
	})
	return k, err
}
