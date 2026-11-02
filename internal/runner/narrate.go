package runner

import (
	"encoding/json"
	"strings"

	"github.com/kensac/orbit/internal/journal"
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
				out = append(out, Narration{Tool: &ToolUse{Name: b.Name, Summary: journal.Summary(b.Name, b.Input)}})
			}
		}
	}
	return out
}

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
		return []Narration{{Tool: &ToolUse{Name: "shell", Summary: journal.Clip(it.Command)}}}
	case kind == "item.started" && it.Type == "web_search":
		return []Narration{{Tool: &ToolUse{Name: "web_search", Summary: journal.Clip(it.Query)}}}
	case kind == "item.started" && it.Type == "mcp_tool_call":
		return []Narration{{Tool: &ToolUse{Name: it.Server + "." + it.Tool}}}
	case kind == "item.completed" && it.Type == "file_change":
		var paths []string
		for _, c := range it.Changes {
			paths = append(paths, journal.RelPath(c.Path))
		}
		return []Narration{{Tool: &ToolUse{Name: "edit", Summary: journal.Clip(strings.Join(paths, ", "))}}}
	}
	return nil
}
