// Package claudeimport reads what Claude Code keeps on disk (a project's auto memory directory, a skills directory) into orbit's shapes, so a person's existing memories and skills can seed orbit. It reads files only; `orbit import` sends the result to the API.
package claudeimport

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/kensac/orbit/internal/frontmatter"
	"github.com/kensac/orbit/internal/store"
)

// Memory is one Claude Code memory file, mapped to an entry's fields.
type Memory struct {
	// File is the file's name in its directory.
	File string `json:"file"`
	Key  string `json:"key"`
	store.EntryFields
}

// indexLineRE is a line of MEMORY.md, the directory's index:
// "- [Title](file.md) - description" (any dash).
var indexLineRE = regexp.MustCompile(`^\s*[-*]\s*\[([^\]]+)\]\(([^)]+\.md)\)`)

// MemoryIndexFile is the index Claude Code keeps beside the entries; it is read for titles, never imported as an entry.
const MemoryIndexFile = "MEMORY.md"

// ReadMemoryDir reads every memory file in dir (not recursively), leaving out an operating system's droppings (Junk). A file it cannot map is skipped with a warning naming it, so one odd file does not stop an import.
func ReadMemoryDir(dir string) ([]Memory, []string, error) {
	titles, err := indexTitles(filepath.Join(dir, MemoryIndexFile))
	if err != nil {
		return nil, nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(files)
	var out []Memory
	var warnings []string
	keys := map[string]string{}
	for _, f := range files {
		base := filepath.Base(f)
		if base == MemoryIndexFile || Junk(base) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		m, err := ParseMemory(base, b, titles[base])
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: skipped: %v", base, err))
			continue
		}
		if other, dup := keys[m.Key]; dup {
			warnings = append(warnings, fmt.Sprintf("%s: skipped: key %q is also %s's", base, m.Key, other))
			continue
		}
		keys[m.Key] = base
		out = append(out, m)
	}
	return out, warnings, nil
}

// indexTitles maps file names to their titles in MEMORY.md; a missing index gives none.
func indexTitles(path string) (map[string]string, error) {
	titles := map[string]string{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return titles, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if m := indexLineRE.FindStringSubmatch(sc.Text()); m != nil {
			titles[filepath.Base(m[2])] = strings.TrimSpace(m[1])
		}
	}
	return titles, sc.Err()
}

// ParseMemory maps one memory file. title, when not empty, is its title from the index; otherwise the name gives one.
func ParseMemory(file string, content []byte, title string) (Memory, error) {
	fm, body, err := frontmatter.Split(content)
	if err != nil {
		return Memory{}, err
	}
	f := frontmatter.Fields(fm)
	// Claude Code writes type at the top level or, in newer files, under metadata.
	typ := f["type"]
	if typ == "" {
		typ = f["metadata.type"]
	}
	if !slices.Contains(store.EntryTypes, typ) {
		return Memory{}, fmt.Errorf("type %q is not one of %s", typ, strings.Join(store.EntryTypes, ", "))
	}
	name := f["name"]
	if name == "" {
		name = strings.TrimSuffix(file, filepath.Ext(file))
	}
	if title == "" {
		title = humanize(name)
	}
	m := Memory{File: file, Key: store.EntryKey(name)}
	m.Type, m.Title = typ, oneLine(title, 200)
	m.Description = oneLine(f["description"], 400)
	m.Body, m.Why, m.HowToApply = splitReasons(body)
	if m.Body == "" {
		// A memory that is all reason still needs a body: its summary.
		m.Body = m.Description
	}
	if m.Body == "" {
		return Memory{}, fmt.Errorf("no body")
	}
	return m, nil
}

// reasonRE finds the paragraphs Claude Code memories mark "**Why:**" and "**How to apply:**" at the start of a line.
var reasonRE = regexp.MustCompile(`(?m)^\*\*(Why|How to apply):\*\*[ \t]*`)

// splitReasons takes the Why and How to apply paragraphs out of a body:
// each runs from its marker to the next marker or the end.
func splitReasons(body string) (rest, why, how string) {
	locs := reasonRE.FindAllStringSubmatchIndex(body, -1)
	if len(locs) == 0 {
		return strings.TrimSpace(body), "", ""
	}
	var parts []string
	prev := 0
	for i, l := range locs {
		parts = append(parts, body[prev:l[0]])
		end := len(body)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		text := strings.TrimSpace(body[l[1]:end])
		switch body[l[2]:l[3]] {
		case "Why":
			why = joinPara(why, text)
		default:
			how = joinPara(how, text)
		}
		prev = end
	}
	parts = append(parts, body[prev:])
	return strings.TrimSpace(strings.Join(parts, "")), why, how
}

func joinPara(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n\n" + b
}

func humanize(name string) string {
	s := strings.Join(strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' }), " ")
	if s == "" {
		return name
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// oneLine collapses whitespace and cuts to n bytes on a rune boundary.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := n - 3
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "..."
}
