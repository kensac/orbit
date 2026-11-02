// Package claudeimport reads what Claude Code keeps on disk (a project's auto memory directory, a skills directory) into orbit's shapes, so a person's existing memories and skills can seed orbit. It reads files only; `orbit import` sends the result to the API.
package claudeimport

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kensac/orbit/internal/store"
)

// Memory is one Claude Code memory file, mapped to an entry's fields.
type Memory struct {
	// File is the file's name in its directory.
	File string `json:"file"`
	Key  string `json:"key"`
	store.EntryFields
}

// frontmatter is the header of a memory file. Claude Code writes type at the top level or, in newer files, under metadata.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Type        string `yaml:"type"`
	Metadata    struct {
		Type string `yaml:"type"`
	} `yaml:"metadata"`
}

// indexLineRE is a line of MEMORY.md, the directory's index:
// "- [Title](file.md) - description" (any dash).
var indexLineRE = regexp.MustCompile(`^\s*[-*]\s*\[([^\]]+)\]\(([^)]+\.md)\)`)

// MemoryIndexFile is the index Claude Code keeps beside the entries; it is read for titles, never imported as an entry.
const MemoryIndexFile = "MEMORY.md"

// ReadMemoryDir reads every memory file in dir (not recursively). A file it cannot map is skipped with a warning naming it, so one odd file does not stop an import.
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
		if base == MemoryIndexFile {
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
	fm, body, err := splitFrontmatter(content)
	if err != nil {
		return Memory{}, err
	}
	var h frontmatter
	if err := yaml.Unmarshal(fm, &h); err != nil {
		// Claude Code reads its own frontmatter leniently, and writes some that is not YAML (a value that opens with a quoted phrase), so read those line by line as it would.
		h = lenientFrontmatter(fm)
	}
	typ := h.Type
	if typ == "" {
		typ = h.Metadata.Type
	}
	if !slices.Contains(store.EntryTypes, typ) {
		return Memory{}, fmt.Errorf("type %q is not one of %s", typ, strings.Join(store.EntryTypes, ", "))
	}
	name := h.Name
	if name == "" {
		name = strings.TrimSuffix(file, filepath.Ext(file))
	}
	if title == "" {
		title = humanize(name)
	}
	m := Memory{File: file, Key: store.EntryKey(name)}
	m.Type, m.Title = typ, oneLine(title, 200)
	m.Description = oneLine(h.Description, 400)
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

// lenientFrontmatter reads "key: value" lines: top-level name, description and type, and an indented type under metadata.
func lenientFrontmatter(fm []byte) frontmatter {
	var h frontmatter
	inMeta := false
	for _, line := range strings.Split(string(fm), "\n") {
		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if !indented {
			inMeta = k == "metadata"
			switch k {
			case "name":
				h.Name = unquote(v)
			case "description":
				h.Description = unquote(v)
			case "type":
				h.Type = unquote(v)
			}
		} else if inMeta && k == "type" {
			h.Metadata.Type = unquote(v)
		}
	}
	return h
}

// unquote strips one pair of quotes around a whole value.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// splitFrontmatter separates a leading "---" YAML block from the rest.
func splitFrontmatter(b []byte) ([]byte, string, error) {
	b = bytes.TrimPrefix(b, []byte("\ufeff"))
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return nil, "", fmt.Errorf("no frontmatter")
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		if strings.HasSuffix(rest, "\n---") {
			return []byte(rest[:len(rest)-4]), "", nil
		}
		return nil, "", fmt.Errorf("frontmatter is not closed")
	}
	return []byte(rest[:end]), strings.TrimSpace(rest[end+5:]), nil
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
