package journal

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SummaryMax bounds a tool call's summary: one line on a phone.
const SummaryMax = 160

// Summary is the one input that says what a tool call was about: the
// file, the pattern, the command, the URL.
func Summary(name string, in map[string]any) string {
	str := func(k string) string { s, _ := in[k].(string); return s }
	var s string
	switch name {
	case "Read", "Write", "Edit", "MultiEdit":
		s = RelPath(str("file_path"))
	case "NotebookEdit":
		s = RelPath(str("notebook_path"))
	case "Glob", "Grep":
		s = str("pattern")
		if p := RelPath(str("path")); p != "" {
			s += " in " + p
		}
	case "Bash":
		s = str("command")
	case "WebFetch":
		s = str("url")
	case "WebSearch":
		s = str("query")
	case "Task", "Agent":
		s = str("description")
	case "TodoWrite":
		if todos, ok := in["todos"].([]any); ok {
			s = plural(len(todos), "item")
		}
	default:
		for _, k := range []string{"file_path", "path", "pattern", "query", "url", "command", "description"} {
			if s = str(k); s != "" {
				break
			}
		}
	}
	return Clip(s)
}

// runDir matches a run's workdir prefix (<data>/runs/<id>-<attempt>/), so a
// summary or a diff names inputs/x.json rather than a scratch path that
// differs per attempt.
var runDir = regexp.MustCompile(`^.*/runs/[^/]+/`)

// RelPath is p relative to its run's directory.
func RelPath(p string) string { return runDir.ReplaceAllString(p, "") }

// Clip makes s one line of at most SummaryMax runes.
func Clip(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " ..."
	}
	if utf8.RuneCountInString(s) > SummaryMax {
		r := []rune(s)
		s = string(r[:SummaryMax-3]) + "..."
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
