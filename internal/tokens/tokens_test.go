package tokens_test

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/tokens"
)

// claude-live-results.jsonl is the two result lines of one live Claude Code 2.1.316 process (stream-json in, two user turns, haiku): modelUsage is the process's running total, usage and model_usage_since_last_result the turn's own.
func TestClaudeResultLinesCountEachTurn(t *testing.T) {
	f, err := os.Open("testdata/claude-live-results.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var got []*tokens.Usage
	for sc.Scan() {
		r, err := claudeproto.ParseResult(append([]byte{}, sc.Bytes()...))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, r.Tokens())
	}
	const model = "claude-haiku-5-5"
	first := tokens.Count{Input: 2, Output: 4, CacheRead: 649, CacheCreation: 2960}
	second := tokens.Count{Input: 2, Output: 4, CacheRead: 0, CacheCreation: 503315}
	want := []*tokens.Usage{
		{Count: first, Model: model, NumTurns: 1, ByModel: map[string]tokens.Count{model: first}},
		{Count: second, Model: model, NumTurns: 1, ByModel: map[string]tokens.Count{model: second}},
	}
	if !reflect.DeepEqual(got, want) {
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		t.Fatalf("tokens = %s\nwant     %s", a, b)
	}
	if r := tokens.HitRatio(first.CacheRead, first.Prompt()); r == nil || *r < 0.1797 || *r > 0.1798 {
		t.Fatalf("hit ratio = %v, want 649/3611", r)
	}
}

func TestClaudeWithoutPerTurnSplitUsesUsage(t *testing.T) {
	u := tokens.FromClaude(json.RawMessage(`{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":90,"cache_creation_input_tokens":0}`),
		nil, json.RawMessage(`{"claude-a":{"costUSD":0.1},"claude-b":{"costUSD":0.5}}`), 2)
	want := &tokens.Usage{Count: tokens.Count{Input: 10, Output: 3, CacheRead: 90}, Model: "claude-b", NumTurns: 2,
		ByModel: map[string]tokens.Count{"claude-b": {Input: 10, Output: 3, CacheRead: 90}}}
	if !reflect.DeepEqual(u, want) {
		t.Fatalf("got %+v, want %+v", u, want)
	}
	if tokens.FromClaude(nil, nil, nil, 0) != nil || tokens.FromClaude(json.RawMessage(`null`), json.RawMessage(`{}`), nil, 0) != nil {
		t.Fatal("a line without counts gave counts")
	}
}

func TestSubagentsCountInTheTurn(t *testing.T) {
	u := tokens.FromClaude(json.RawMessage(`{"input_tokens":1,"output_tokens":1}`),
		json.RawMessage(`{"claude-big":{"inputTokens":1,"outputTokens":1,"cacheReadInputTokens":100,"costUSD":0.2},
		                  "claude-small":{"inputTokens":5,"outputTokens":2,"cacheCreationInputTokens":50,"costUSD":0.01}}`), nil, 3)
	if u.Model != "claude-big" || u.Count != (tokens.Count{Input: 6, Output: 3, CacheRead: 100, CacheCreation: 50}) || len(u.ByModel) != 2 {
		t.Fatalf("got %+v", u)
	}
}

func TestCodexAndAnthropic(t *testing.T) {
	c := tokens.FromCodex(json.RawMessage(`{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":50,"reasoning_output_tokens":20}`), "gpt-x")
	if c.Count != (tokens.Count{Input: 200, Output: 50, CacheRead: 800}) || c.Model != "gpt-x" || c.NumTurns != 1 {
		t.Fatalf("codex = %+v", c)
	}
	if c := tokens.FromCodex(json.RawMessage(`{"input_tokens":5}`), ""); c.Input != 5 || c.ByModel != nil {
		t.Fatalf("codex without a model = %+v", c)
	}
	a := tokens.FromAnthropic(json.RawMessage(`{"input_tokens":3,"output_tokens":9,"cache_read_input_tokens":40,"cache_creation_input_tokens":7}`), "claude-m")
	if a.Count != (tokens.Count{Input: 3, Output: 9, CacheRead: 40, CacheCreation: 7}) || a.Model != "claude-m" {
		t.Fatalf("anthropic = %+v", a)
	}
	if tokens.FromCodex(nil, "m") != nil || tokens.FromAnthropic(nil, "m") != nil {
		t.Fatal("no usage gave counts")
	}
}

func TestPrefixChanges(t *testing.T) {
	base := map[string]string{"model": "claude-code/opus", "system": "contract", "charter": "watch spend"}
	a := tokens.NewPrefix(base)
	charter := map[string]string{"model": "claude-code/opus", "system": "contract", "charter": "watch spend weekly"}
	b := tokens.NewPrefix(charter)
	if a.SHA != b.SHA {
		t.Fatal("the charter changed the prefix digest; a resumed session sends it after the transcript")
	}
	if got := tokens.ChangedParts(a.Parts, b.Parts); !reflect.DeepEqual(got, []string{"charter"}) {
		t.Fatalf("changed = %v", got)
	}
	sys := tokens.NewPrefix(map[string]string{"model": "claude-code/opus", "system": "contract + today's date", "charter": "watch spend"})
	if sys.SHA == a.SHA || !reflect.DeepEqual(tokens.ChangedParts(a.Parts, sys.Parts), []string{"system"}) {
		t.Fatalf("a system change: %v", tokens.ChangedParts(a.Parts, sys.Parts))
	}
	if got := tokens.ChangedParts(a.Parts, a.Parts); len(got) != 0 {
		t.Fatalf("unchanged = %v", got)
	}
}
