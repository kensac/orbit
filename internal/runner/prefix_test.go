package runner_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/tokens"
)

func TestPrefixOfNamesWhatChanged(t *testing.T) {
	base := runner.Request{Workdir: "/data/agents/a/work", System: "contract", Prompt: "today's prompt", Tools: []string{"WebFetch"},
		OutputSchema: json.RawMessage(`{"type":"object"}`), Harness: &runner.Harness{Bin: "orbit"}, SkillsPlugin: "/home/.claude/orbit-skills-abc"}
	a := runner.PrefixOf("claude-code", "opus", "charter", base)
	same := base
	same.Prompt, same.RunID = "another prompt", "another run"
	if b := runner.PrefixOf("claude-code", "opus", "charter", same); b.SHA != a.SHA {
		t.Fatal("the prompt or run id changed the prefix; both come after it")
	}
	cases := map[string]func(r *runner.Request) (string, string){
		"workdir": func(r *runner.Request) (string, string) { r.Workdir = "/data/runs/r2"; return "opus", "charter" },
		"skills": func(r *runner.Request) (string, string) {
			r.SkillsPlugin = "/home/.claude/orbit-skills-def"
			return "opus", "charter"
		},
		"schema": func(r *runner.Request) (string, string) {
			r.OutputSchema = json.RawMessage(`{}`)
			return "opus", "charter"
		},
		"mcp": func(r *runner.Request) (string, string) {
			r.Harness = &runner.Harness{Bin: "orbit", Search: "/s"}
			return "opus", "charter"
		},
		"tools":   func(r *runner.Request) (string, string) { r.Tools = nil; return "opus", "charter" },
		"model":   func(r *runner.Request) (string, string) { return "sonnet", "charter" },
		"charter": func(r *runner.Request) (string, string) { return "opus", "new charter" },
	}
	for part, change := range cases {
		r := base
		model, charter := change(&r)
		b := runner.PrefixOf("claude-code", model, charter, r)
		if got := tokens.ChangedParts(a.Parts, b.Parts); !reflect.DeepEqual(got, []string{part}) {
			t.Errorf("%s: changed = %v", part, got)
		}
		if (b.SHA == a.SHA) != (part == "charter") {
			t.Errorf("%s: digest changed = %v", part, b.SHA != a.SHA)
		}
	}
	moved := base
	moved.Workdir = "/elsewhere"
	if runner.PrefixOf("codex", "m", "", base).SHA != runner.PrefixOf("codex", "m", "", moved).SHA {
		t.Fatal("a backend that does not name its directory changed with it")
	}
}
