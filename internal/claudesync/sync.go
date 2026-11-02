// Package claudesync writes orbit's skills and memory entries back into Claude Code's files on disk (a skills directory, a project's memory directory and its MEMORY.md), the reverse of claudeimport. It never overwrites a person's own edit: a local copy is replaced or removed only when it is exactly some version orbit holds, so nothing it writes over is lost. `orbit sync claude` plans, prints the plan's digest, and applies only that plan.
package claudesync

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kensac/orbit/internal/claudeimport"
	"github.com/kensac/orbit/internal/store"
)

// Actions a plan's operation takes.
const (
	Create         = "create"
	Update         = "update"
	Remove         = "remove"
	Unchanged      = "unchanged"
	ChangedLocally = "changed locally"
)

// Kinds of operation.
const (
	KindSkill  = "skill"
	KindMemory = "memory"
	KindIndex  = "index"
)

// Op is one thing a sync does to disk.
type Op struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Action string `json:"action"`
	// Before is the SHA-256 of the local copy as planned ("" when there is none): a write goes ahead only while it still is. For a skill it is its digest (store.SkillDigest), for a file its bytes'.
	Before string `json:"before"`
	// After is the SHA-256 of what would be written ("" for a removal or no write).
	After string `json:"after"`
	// Version is the orbit version written, or matched.
	Version int `json:"version,omitempty"`
	// Why says why a local copy is left alone.
	Why string `json:"why,omitempty"`

	files map[string][]byte // a skill's files to write, by path; a memory or index file's content under ""
}

// Writes reports whether an operation changes disk.
func (o Op) Writes() bool { return o.Action == Create || o.Action == Update || o.Action == Remove }

// Plan is a sync's operations in order (skills by name, memory entries by key, then the index) and its digest.
type Plan struct {
	Ops    []Op   `json:"ops"`
	Digest string `json:"digest"`
}

// NewPlan computes ops' digest: the SHA-256 over every operation's kind, name, path, action, before and after, so a plan applied later is exactly the plan that was shown.
func NewPlan(ops []Op) Plan {
	h := sha256.New()
	for _, o := range ops {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\n", o.Kind, o.Name, o.Path, o.Action, o.Before, o.After)
	}
	return Plan{Ops: ops, Digest: hex.EncodeToString(h.Sum(nil))}
}

// SkillState is one of orbit's skills: its current version in full (content and files) and every version's digest.
type SkillState struct {
	Current  store.Skill
	Versions []store.Skill
}

func digestIn(d string, versions []store.Skill) (int, bool) {
	for _, v := range versions {
		if v.SHA256 == d {
			return v.Version, true
		}
	}
	return 0, false
}

// localSkill is a skill directory's digest: "" when the directory is absent, an error when it is there but not a skill orbit can read (then it is the person's).
func localSkill(dir string) (string, error) {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	k, err := claudeimport.ReadSkill(dir)
	if err != nil {
		return "", err
	}
	return store.SkillDigest(k.Content, k.Files), nil
}

// PlanSkills plans each skill into dir/<name>: an active one is created, or updated when the local copy is an older version of it; a retired one's local copy is removed when it is a version of it. A local copy that is no version orbit holds is the person's: changed locally, left alone.
func PlanSkills(dir string, skills []SkillState) []Op {
	var ops []Op
	sorted := slices.Clone(skills)
	slices.SortFunc(sorted, func(a, b SkillState) int { return strings.Compare(a.Current.Name, b.Current.Name) })
	for _, k := range sorted {
		cur := k.Current
		path := filepath.Join(dir, cur.Name)
		op := Op{Kind: KindSkill, Name: cur.Name, Path: path, Version: cur.Version}
		local, err := localSkill(path)
		if err != nil {
			op.Action, op.Before, op.Why = ChangedLocally, "unreadable", err.Error()
			ops = append(ops, op)
			continue
		}
		op.Before = local
		matched, known := digestIn(local, k.Versions)
		retired := cur.Status != "active"
		switch {
		case local == "" && retired:
			continue
		case local == "":
			op.Action = Create
		case !known:
			op.Action, op.Why = ChangedLocally, "the local copy is no version orbit holds"
		case retired:
			op.Action, op.Version = Remove, matched
		case local == cur.SHA256:
			op.Action = Unchanged
		default:
			op.Action, op.Why = Update, fmt.Sprintf("the local copy is version %d", matched)
		}
		if op.Action == Create || op.Action == Update {
			op.After = cur.SHA256
			op.files = map[string][]byte{"SKILL.md": []byte(cur.Content)}
			for _, f := range cur.Files {
				op.files[f.Path] = f.Content
			}
		}
		ops = append(ops, op)
	}
	return ops
}

// EntryState is one of orbit's memory entries: its current version and every version.
type EntryState struct {
	Current  store.Entry
	Versions []store.Entry
}

// FieldsSHA256 is a memory entry's content as a memory file carries it (type, title, description, body, why, how to apply), each normalized as claudeimport reads a file, so a file and an entry version compare equal when one is the other.
func FieldsSHA256(f store.EntryFields) string {
	norm := func(s string) string { return strings.TrimSpace(s) }
	one := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	b, _ := json.Marshal([]string{f.Type, one(f.Title), one(f.Description), norm(f.Body), norm(f.Why), norm(f.HowToApply)})
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// RenderMemory is an entry as a Claude Code memory file: frontmatter with its key as name, its description and its type, then its body and its Why and How to apply paragraphs, which claudeimport.ParseMemory reads back into the same fields.
func RenderMemory(e store.Entry) []byte {
	type meta struct {
		Type string `yaml:"type"`
	}
	fm, _ := yaml.Marshal(struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Metadata    meta   `yaml:"metadata"`
	}{e.Key, strings.Join(strings.Fields(e.Description), " "), meta{e.Type}})
	var b bytes.Buffer
	b.WriteString("---\n")
	b.Write(fm)
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(e.Body))
	b.WriteString("\n")
	if w := strings.TrimSpace(e.Why); w != "" {
		b.WriteString("\n**Why:** " + w + "\n")
	}
	if h := strings.TrimSpace(e.HowToApply); h != "" {
		b.WriteString("\n**How to apply:** " + h + "\n")
	}
	return b.Bytes()
}

func fileSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// indexLineRE finds a MEMORY.md line's file: "- [Title](file.md) ...".
var indexLineRE = regexp.MustCompile(`^\s*[-*]\s*\[[^\]]*\]\(([^)]+\.md)\)`)

// indexLine is an entry's MEMORY.md line.
func indexLine(e store.Entry, file string) string {
	line := "- [" + strings.Join(strings.Fields(e.Title), " ") + "](" + file + ")"
	if d := strings.Join(strings.Fields(e.Description), " "); d != "" {
		line += " - " + d
	}
	return line
}

// PlanMemory plans entries into dir as memory files, and dir's MEMORY.md: an active entry's file is created as <key>.md, or updated when the local file holding its key is an older version of it; a retired one's file is removed when it is a version of it. A local file that is no version orbit holds is changed locally and left alone. MEMORY.md gets a line per written entry (its old line for that file replaced) and loses the lines of removed ones; every other line stays as it is.
func PlanMemory(dir string, entries []EntryState) ([]Op, error) {
	local, _, err := claudeimport.ReadMemoryDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	byKey := map[string]claudeimport.Memory{}
	files := map[string]bool{}
	for _, m := range local {
		byKey[m.Key] = m
		files[m.File] = true
	}
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b EntryState) int { return strings.Compare(a.Current.Key, b.Current.Key) })
	var ops []Op
	lines := map[string]string{} // file -> its new index line, "" to drop it
	for _, s := range sorted {
		e := s.Current
		file := e.Key + ".md"
		m, have := byKey[e.Key]
		if have {
			file = m.File
		}
		path := filepath.Join(dir, file)
		op := Op{Kind: KindMemory, Name: e.Key, Path: path, Version: e.Version}
		raw, rerr := os.ReadFile(path) //nolint:gosec // the memory directory the person named
		switch {
		case rerr == nil:
			op.Before = fileSHA256(raw)
		case !errors.Is(rerr, os.ErrNotExist):
			return nil, rerr
		}
		retired := e.Status != "active"
		matched, known := 0, false
		if have {
			h := FieldsSHA256(m.EntryFields)
			for _, v := range s.Versions {
				if FieldsSHA256(v.EntryFields) == h {
					matched, known = v.Version, true
					break
				}
			}
		}
		switch {
		case op.Before == "" && retired:
			continue
		case op.Before == "":
			op.Action = Create
		case !have:
			op.Action, op.Why = ChangedLocally, "the file is not a memory of this entry's key"
		case !known:
			op.Action, op.Why = ChangedLocally, "the local copy is no version orbit holds"
		case retired:
			op.Action, op.Version = Remove, matched
			lines[file] = ""
		case FieldsSHA256(m.EntryFields) == FieldsSHA256(e.EntryFields):
			op.Action = Unchanged
		default:
			op.Action, op.Why = Update, fmt.Sprintf("the local copy is version %d", matched)
		}
		if op.Action == Create || op.Action == Update {
			body := RenderMemory(e)
			op.After, op.files = fileSHA256(body), map[string][]byte{"": body}
			lines[file] = indexLine(e, file)
		}
		ops = append(ops, op)
	}
	if idx, ok, err := planIndex(dir, lines); err != nil {
		return nil, err
	} else if ok {
		ops = append(ops, idx)
	}
	return ops, nil
}

// planIndex is MEMORY.md with lines set per file ("" drops that file's line), or ok false when nothing would change.
func planIndex(dir string, lines map[string]string) (Op, bool, error) {
	if len(lines) == 0 {
		return Op{}, false, nil
	}
	path := filepath.Join(dir, claudeimport.MemoryIndexFile)
	op := Op{Kind: KindIndex, Name: claudeimport.MemoryIndexFile, Path: path, Action: Update}
	old, err := os.ReadFile(path) //nolint:gosec // the memory directory the person named
	switch {
	case errors.Is(err, os.ErrNotExist):
		op.Action = Create
	case err != nil:
		return op, false, err
	default:
		op.Before = fileSHA256(old)
	}
	text := strings.TrimRight(string(old), "\n")
	var out []string
	done := map[string]bool{}
	if text != "" {
		for _, l := range strings.Split(text, "\n") {
			if m := indexLineRE.FindStringSubmatch(l); m != nil {
				if nl, ok := lines[filepath.Base(m[1])]; ok {
					done[filepath.Base(m[1])] = true
					if nl != "" {
						out = append(out, nl)
					}
					continue
				}
			}
			out = append(out, l)
		}
	}
	for _, f := range slices.Sorted(maps.Keys(lines)) {
		if !done[f] && lines[f] != "" {
			out = append(out, lines[f])
		}
	}
	next := []byte(strings.Join(out, "\n") + "\n")
	if op.Before != "" && fileSHA256(next) == op.Before {
		return op, false, nil
	}
	op.After, op.files = fileSHA256(next), map[string][]byte{"": next}
	return op, true, nil
}

// Result is what applying one operation did.
type Result struct {
	Op
	Done  bool   `json:"done"`
	Error string `json:"error,omitempty"`
}

// Apply carries out a plan's writes in order. Each checks first that the local copy is still what the plan saw (Before); one that moved is skipped and reported, and the rest go ahead. A skill is written beside its directory and swapped in, a file written beside itself and renamed over, so a reader never sees half of either.
func Apply(p Plan) []Result {
	var out []Result
	for _, o := range p.Ops {
		if !o.Writes() {
			continue
		}
		r := Result{Op: o}
		if err := applyOp(o); err != nil {
			r.Error = err.Error()
		} else {
			r.Done = true
		}
		out = append(out, r)
	}
	return out
}

func applyOp(o Op) error {
	now, err := currentHash(o)
	if err != nil {
		return err
	}
	if now != o.Before {
		return fmt.Errorf("%s changed since the plan was made; run the dry run again", o.Path)
	}
	switch {
	case o.Kind == KindSkill && o.Action == Remove:
		return swapDir(o.Path, nil)
	case o.Kind == KindSkill:
		return swapDir(o.Path, o.files)
	case o.Action == Remove:
		return os.Remove(o.Path)
	default:
		return writeFile(o.Path, o.files[""])
	}
}

func currentHash(o Op) (string, error) {
	if o.Kind == KindSkill {
		h, err := localSkill(o.Path)
		if err != nil {
			return "unreadable", nil
		}
		return h, nil
	}
	b, err := os.ReadFile(o.Path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return fileSHA256(b), nil
}

func suffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// swapDir replaces dir with files (nil: removes it), through a sibling written first and renamed in.
func swapDir(dir string, files map[string][]byte) error {
	parent, name := filepath.Dir(dir), filepath.Base(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil { //nolint:gosec // a skills directory Claude Code reads
		return err
	}
	var tmp string
	if files != nil {
		tmp = filepath.Join(parent, ".orbit-sync-"+name+"-"+suffix())
		for p, c := range files {
			dst := filepath.Join(tmp, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // as above
				_ = os.RemoveAll(tmp)
				return err
			}
			if err := os.WriteFile(dst, c, 0o644); err != nil { //nolint:gosec // a skill's files are read by Claude Code, not secret
				_ = os.RemoveAll(tmp)
				return err
			}
		}
	}
	old := ""
	if _, err := os.Stat(dir); err == nil {
		old = filepath.Join(parent, ".orbit-sync-old-"+name+"-"+suffix())
		if err := os.Rename(dir, old); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	if tmp != "" {
		if err := os.Rename(tmp, dir); err != nil {
			if old != "" {
				_ = os.Rename(old, dir)
			}
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	if old != "" {
		// What it held is a version orbit keeps (the plan checked), so nothing is lost.
		return os.RemoveAll(old)
	}
	return nil
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // a memory directory Claude Code reads
		return err
	}
	tmp := path + ".orbit-sync-" + suffix()
	if err := os.WriteFile(tmp, b, 0o644); err != nil { //nolint:gosec // as Claude Code writes its memory files
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
