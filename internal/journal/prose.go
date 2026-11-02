package journal

import (
	"encoding/json"
	"strings"
)

// FinalProse is the prose a timeline ends with: its last run of top-level assistant messages (a tool call ends a run), joined by blank lines. A subagent's messages are not part of it, and neither is the call that submits a structured answer, which is not a tool call. It projects the events in memory exactly as the store does (Project), so it is what the run's journal shows as the agent's last words.
func FinalProse(events []Event) string {
	var (
		state     AttemptState
		latest    = map[string]*Item{}
		cur, last []string
	)
	look := func(id string) (*Item, error) { return latest[id], nil }
	for _, ev := range events {
		ws, err := Project(&state, ev, look)
		if err != nil {
			continue
		}
		for _, w := range ws {
			b, _ := json.Marshal(w.Body)
			latest[w.ID] = &Item{ID: w.ID, Kind: w.Kind, Body: b}
			if w.Parent != "" {
				continue
			}
			switch w.Kind {
			case KindMessage:
				if m, ok := w.Body.(Message); ok && m.Role == "assistant" && strings.TrimSpace(m.Text) != "" {
					cur = append(cur, strings.TrimSpace(m.Text))
					last = cur
				}
			case KindToolCall:
				cur = nil
			}
		}
	}
	return strings.Join(last, "\n\n")
}
