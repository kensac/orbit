package journal

import (
	"encoding/json"
	"testing"
)

func events(lines ...string) []Event {
	var evs []Event
	for i, l := range lines {
		var h struct{ Type string }
		_ = json.Unmarshal([]byte(l), &h)
		evs = append(evs, Event{Attempt: 1, Seq: i + 1, At: t0, Kind: h.Type, Payload: []byte(l)})
	}
	return evs
}

func TestFinalProseIsTheLastRunOfTopLevelAssistantText(t *testing.T) {
	evs := events(
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Let me look."},{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"## Issues\n\n1. One\n2. Two"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"That is all.\n"}]}}`,
		// A subagent's prose, a later plan update and the submit call are not part of it.
		`{"type":"assistant","parent_tool_use_id":"t9","message":{"content":[{"type":"text","text":"subagent words"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"TodoWrite","input":{"todos":[]}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"StructuredOutput","input":{"summary":"s","report":"See above."}}]}}`,
	)
	if got, want := FinalProse(evs), "## Issues\n\n1. One\n2. Two\n\nThat is all."; got != want {
		t.Fatalf("FinalProse = %q, want %q", got, want)
	}
	if got := FinalProse(events(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`)); got != "" {
		t.Fatalf("no prose = %q", got)
	}
}
