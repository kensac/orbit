package runner

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Narration is one thing a person watching a run sees: prose the agent
// wrote, or a tool it used. It is derived from the stored transcript by
// code, deterministically, so a replay shows exactly what the live stream
// showed.
type Narration struct {
	Text string
	Tool *ToolUse
}

// ToolUse is a tool call, summarised to one line.
type ToolUse struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

// summaryMax bounds a tool summary: one line on a phone.
const summaryMax = 160

// submitTools are the calls through which a backend returns its final
// answer (Claude Code's --json-schema tool, the anthropic runner's forced
// tool). The answer is the run's report, not a step worth narrating.
var submitTools = map[string]bool{"StructuredOutput": true, "submit": true}

// Narrate turns one stored event into what a stream shows. runnerKind is
// the run's backend; events orbit itself recorded (orbit.*) and lines a
// backend emits that carry no prose or tool call narrate to nothing.
func Narrate(runnerKind, eventKind string, payload []byte) []Narration {
	switch runnerKind {
	case "claude-code":
		if eventKind == "assistant" {
			return narrateClaude(payload)
		}
	case "codex":
		if eventKind == "item.started" || eventKind == "item.completed" {
			return narrateCodex(eventKind, payload)
		}
	case "anthropic":
		if eventKind == "response" {
			return narrateAnthropic(payload)
		}
	}
	return nil
}

type contentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// narrateClaude reads a stream-json assistant line: text blocks are prose,
// tool_use blocks are tool calls. Thinking blocks are not shown.
func narrateClaude(payload []byte) []Narration {
	var ev struct {
		Message struct {
			Content []contentBlock `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return nil
	}
	return narrateBlocks(ev.Message.Content)
}

// narrateAnthropic reads a Messages API response, whose only tool call is
// the forced submit.
func narrateAnthropic(payload []byte) []Narration {
	var m struct {
		Content []contentBlock `json:"content"`
	}
	if json.Unmarshal(payload, &m) != nil {
		return nil
	}
	return narrateBlocks(m.Content)
}

func narrateBlocks(blocks []contentBlock) []Narration {
	var out []Narration
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				out = append(out, Narration{Text: b.Text})
			}
		case "tool_use":
			if !submitTools[b.Name] {
				out = append(out, Narration{Tool: &ToolUse{Name: b.Name, Summary: toolSummary(b.Name, b.Input)}})
			}
		}
	}
	return out
}

// toolSummary is the one input that says what a call was about: the file,
// the pattern, the command, the URL.
func toolSummary(name string, in map[string]any) string {
	str := func(k string) string { s, _ := in[k].(string); return s }
	var s string
	switch name {
	case "Read", "Write", "Edit", "MultiEdit":
		s = relPath(str("file_path"))
	case "NotebookEdit":
		s = relPath(str("notebook_path"))
	case "Glob", "Grep":
		s = str("pattern")
		if p := relPath(str("path")); p != "" {
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
	return clip(s)
}

// runDir matches a run's workdir prefix (<data>/runs/<id>-<attempt>/), so a
// summary names inputs/x.json rather than a scratch path that differs per
// attempt.
var runDir = regexp.MustCompile(`^.*/runs/[^/]+/`)

func relPath(p string) string { return runDir.ReplaceAllString(p, "") }

// narrateCodex reads `codex exec --json` items: reasoning summaries and
// messages are prose, commands, file changes, searches and MCP calls are
// tools. A tool is shown when it starts, prose when it completes.
func narrateCodex(kind string, payload []byte) []Narration {
	var ev struct {
		Item struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Command string `json:"command"`
			Query   string `json:"query"`
			Server  string `json:"server"`
			Tool    string `json:"tool"`
			Changes []struct {
				Path string `json:"path"`
			} `json:"changes"`
		} `json:"item"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return nil
	}
	it := ev.Item
	switch {
	case kind == "item.completed" && (it.Type == "reasoning" || it.Type == "agent_message"):
		// The final agent_message is the structured answer, which the
		// run's report already shows.
		if t := strings.TrimSpace(it.Text); t != "" && !json.Valid([]byte(t)) {
			return []Narration{{Text: it.Text}}
		}
	case kind == "item.started" && it.Type == "command_execution":
		return []Narration{{Tool: &ToolUse{Name: "shell", Summary: clip(it.Command)}}}
	case kind == "item.started" && it.Type == "web_search":
		return []Narration{{Tool: &ToolUse{Name: "web_search", Summary: clip(it.Query)}}}
	case kind == "item.started" && it.Type == "mcp_tool_call":
		return []Narration{{Tool: &ToolUse{Name: it.Server + "." + it.Tool}}}
	case kind == "item.completed" && it.Type == "file_change":
		var paths []string
		for _, c := range it.Changes {
			paths = append(paths, relPath(c.Path))
		}
		return []Narration{{Tool: &ToolUse{Name: "edit", Summary: clip(strings.Join(paths, ", "))}}}
	}
	return nil
}

// clip makes s one line of at most summaryMax runes.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " ..."
	}
	if utf8.RuneCountInString(s) > summaryMax {
		r := []rune(s)
		s = string(r[:summaryMax-3]) + "..."
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
