// Package orbitskills are the skills built into orbit: how to use its own tools and result (query, calc, entries, actions, continuations), compiled into the binary and given to every Claude Code run that has orbit's skill tool, so the system contract states the rules and leaves the how-to to a skill the model loads when it needs it (docs/ARCHITECTURE.md, Skills). Their names are reserved: the library refuses a skill of the same name.
package orbitskills

import (
	"embed"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
)

//go:embed skills
var files embed.FS

// Names of the built-in skills, sorted.
const (
	Query         = "orbit-query"
	Calc          = "orbit-calc"
	Entries       = "orbit-entries"
	Actions       = "orbit-actions"
	Continuations = "orbit-continuations"
)

// File is one file of a built-in skill beside its SKILL.md.
type File struct {
	Path    string
	Content []byte
}

// Skill is one built-in skill.
type Skill struct {
	Name    string
	Content string
	Files   []File
}

var all = load()

func load() map[string]Skill {
	out := map[string]Skill{}
	dirs, err := fs.ReadDir(files, "skills")
	if err != nil {
		panic(err)
	}
	for _, d := range dirs {
		root := path.Join("skills", d.Name())
		k := Skill{Name: d.Name()}
		err := fs.WalkDir(files, root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			b, err := files.ReadFile(p)
			if err != nil {
				return err
			}
			rel := strings.TrimPrefix(p, root+"/")
			if rel == "SKILL.md" {
				k.Content = string(b)
			} else {
				k.Files = append(k.Files, File{Path: rel, Content: b})
			}
			return nil
		})
		if err != nil {
			panic(err)
		}
		sort.Slice(k.Files, func(i, j int) bool { return k.Files[i].Path < k.Files[j].Path })
		out[k.Name] = k
	}
	return out
}

// Names are every built-in skill's name, sorted.
func Names() []string {
	out := make([]string, 0, len(all))
	for n := range all {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Has reports whether name is a built-in skill's.
func Has(name string) bool {
	_, ok := all[name]
	return ok
}

// Get returns a built-in skill by name.
func Get(name string) (Skill, bool) {
	k, ok := all[name]
	return k, ok
}
