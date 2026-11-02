package workspace

import (
	"regexp"
	"strings"
)

// ignoreRule is one pattern of a .gitignore, compiled.
type ignoreRule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

// Ignored reports whether .gitignore files in the snapshot exclude p: the
// .gitignore of every directory from the root down to p's, later and
// deeper patterns overriding earlier ones, and anything under an excluded
// directory excluded, as git decides (gitignore(5)). Only the snapshot's
// own files are read; there is no global or info/exclude file.
func (s *Snapshot) Ignored(e *Entry) bool {
	if e.Path == "" {
		return false
	}
	parts := strings.Split(e.Path, "/")
	for i := 1; i <= len(parts); i++ {
		p := strings.Join(parts[:i], "/")
		isDir := i < len(parts) || e.Kind == KindDir
		if s.ignoredHere(p, isDir) {
			return true
		}
	}
	return false
}

// ignoredHere applies the patterns that reach p, without looking at its
// parents.
func (s *Snapshot) ignoredHere(p string, isDir bool) bool {
	parts := strings.Split(p, "/")
	ignored := false
	for depth := 0; depth < len(parts); depth++ {
		dir := strings.Join(parts[:depth], "/")
		rel := strings.Join(parts[depth:], "/")
		for _, r := range s.rulesOf(dir) {
			if r.dirOnly && !isDir {
				continue
			}
			if r.re.MatchString(rel) {
				ignored = !r.negate
			}
		}
	}
	return ignored
}

// rulesOf is the compiled .gitignore of a directory, read once.
func (s *Snapshot) rulesOf(dir string) []ignoreRule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.ignores[dir]; ok {
		return r
	}
	name := ".gitignore"
	if dir != "" {
		name = dir + "/.gitignore"
	}
	var rules []ignoreRule
	if e, ok := s.entries[name]; ok && e.Kind == KindFile {
		rules = parseGitignore(string(e.data))
	}
	s.ignores[dir] = rules
	return rules
}

// parseGitignore compiles a .gitignore's patterns.
func parseGitignore(text string) []ignoreRule {
	var out []ignoreRule
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		// Trailing spaces are ignored unless escaped.
		for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
			line = line[:len(line)-1]
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var r ignoreRule
		if strings.HasPrefix(line, "!") {
			r.negate, line = true, line[1:]
		} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly, line = true, strings.TrimRight(line, "/")
		}
		if line == "" {
			continue
		}
		// A slash at the start or in the middle anchors the pattern to the
		// .gitignore's directory; otherwise it matches at any depth.
		anchored := strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		expr := globRegexp(line)
		if !anchored {
			expr = "(?:.*/)?" + expr
		}
		re, err := regexp.Compile("^" + expr + "$")
		if err != nil {
			continue
		}
		r.re = re
		out = append(out, r)
	}
	return out
}

// globRegexp translates a gitignore glob: "*" and "?" within one path
// component, "**" across them, character classes as written.
func globRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '\\' && i+1 < len(g):
			i++
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		case c == '[':
			end := strings.IndexByte(g[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}
