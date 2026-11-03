package api_test

import (
	"bufio"
	"os"
	"reflect"
	"testing"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/tokens"
)

// recordedTokens are the token counts of the recorded Claude Code result lines (internal/tokens/testdata), one per turn.
func recordedTokens(t *testing.T) []*tokens.Usage {
	t.Helper()
	f, err := os.Open("../tokens/testdata/claude-live-results.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var out []*tokens.Usage
	for sc.Scan() {
		r, err := claudeproto.ParseResult(append([]byte{}, sc.Bytes()...))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r.Tokens())
	}
	return out
}

func TestRunTokensPrefixAndUsage(t *testing.T) {
	v := newEnv(t, nil)
	withTrigger(t, v, "America/New_York")
	sha := v.e.Current().Config.Agents["a"].SHA
	turns := recordedTokens(t)
	parts := func(system string) tokens.Prefix {
		return tokens.NewPrefix(map[string]string{"model": "claude-code/haiku", "system": system, "charter": "x"})
	}
	run := func(p tokens.Prefix, u *tokens.Usage, events ...string) string {
		t.Helper()
		id, err := v.s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: sha, Trigger: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.s.ClaimRun(ctx, "w"); err != nil {
			t.Fatal(err)
		}
		if err := v.s.RecordPrefix(ctx, id, 1, p); err != nil {
			t.Fatal(err)
		}
		if err := v.s.PutArtifact(ctx, id, 1, "in", "skills.json", "application/json", []byte(`[{"name":"tidy","version":3,"sha256":"x"}]`)); err != nil {
			t.Fatal(err)
		}
		for i, e := range events {
			if err := v.s.AppendEvent(ctx, id, 1, i+1, "assistant", []byte(e)); err != nil {
				t.Fatal(err)
			}
		}
		if err := v.s.FinishRun(ctx, id, store.RunResult{Attempt: 1, Status: "succeeded", Tokens: u}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	skillCall := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"` + harness.ToolSkill + `","input":{"name":"tidy"}}]}}`
	first := run(parts("contract"), turns[0], skillCall)
	second := run(parts("contract, changed"), turns[1])
	third := run(parts("contract, changed"), nil)

	code, r := call(t, "GET", v.srv.URL+"/api/v1/runs/"+second, v.person("kanishk"), "")
	if code != 200 {
		t.Fatalf("run = %d %v", code, r)
	}
	got := r["run"].(map[string]any)
	tk := got["tokens"].(map[string]any)
	if tk["input"] != 2.0 || tk["output"] != 4.0 || tk["cache_read"] != 0.0 || tk["cache_creation"] != 503315.0 ||
		tk["cache_hit_ratio"] != 0.0 || tk["num_turns"] != 1.0 || tk["model"] != "claude-haiku-5-5" {
		t.Fatalf("run tokens = %v", tk)
	}
	px := got["prefix"].(map[string]any)
	if px["previous_run"] != first || !reflect.DeepEqual(px["changed"], []any{"system"}) || px["sha"] == px["previous_sha"] {
		t.Fatalf("prefix = %v", px)
	}
	at := r["attempts"].([]any)[0].(map[string]any)
	if bm := at["tokens"].(map[string]any)["by_model"].(map[string]any); bm["claude-haiku-5-5"].(map[string]any)["cache_creation"] != 503315.0 {
		t.Fatalf("attempt tokens = %v", at["tokens"])
	}
	if at["prefix"].(map[string]any)["sha"] != px["sha"] {
		t.Fatalf("attempt prefix = %v", at["prefix"])
	}

	_, r = call(t, "GET", v.srv.URL+"/api/v1/runs/"+third, v.person("kanishk"), "")
	got = r["run"].(map[string]any)
	if _, ok := got["tokens"]; ok {
		t.Fatalf("a run without counts has tokens: %v", got["tokens"])
	}
	if px := got["prefix"].(map[string]any); px["previous_run"] != second || px["sha"] != px["previous_sha"] || len(px["changed"].([]any)) != 0 {
		t.Fatalf("an unchanged prefix = %v", px)
	}
	_, r = call(t, "GET", v.srv.URL+"/api/v1/runs/"+first, v.person("kanishk"), "")
	if px := r["run"].(map[string]any)["prefix"].(map[string]any); px["previous_run"] != nil || px["changed"] != nil {
		t.Fatalf("the first run's prefix = %v", px)
	}

	_, th := call(t, "GET", v.srv.URL+"/api/v1/agents/a/thread", v.person("kanishk"), "")
	seen := 0
	for _, it := range th["items"].([]any) {
		item := it.(map[string]any)
		if item["kind"] != "run" {
			continue
		}
		seen++
		switch item["id"] {
		case first:
			if item["tokens"].(map[string]any)["cache_read"] != 649.0 {
				t.Fatalf("thread run tokens = %v", item["tokens"])
			}
		case third:
			if item["tokens"] != nil {
				t.Fatalf("thread run without counts = %v", item["tokens"])
			}
		}
	}
	if seen != 3 {
		t.Fatalf("thread = %v", th)
	}

	_, u := call(t, "GET", v.srv.URL+"/api/v1/usage", v.person("kanishk"), "")
	tu := u["tokens"].(map[string]any)
	total := tu["total"].(map[string]any)
	// 649 read of 2+649+2960 + 2+0+503315 prompt tokens.
	if total["input"] != 4.0 || total["output"] != 8.0 || total["cache_read"] != 649.0 || total["cache_creation"] != 506275.0 ||
		total["attempts"] != 2.0 || total["runs"] != 2.0 || total["cache_hit_ratio"] != 0.001280 {
		t.Fatalf("token total = %v", total)
	}
	if m := tu["by_model"].([]any); len(m) != 1 || m[0].(map[string]any)["model"] != "claude-haiku-5-5" || m[0].(map[string]any)["cache_creation"] != 506275.0 {
		t.Fatalf("by_model = %v", m)
	}
	if a := tu["by_agent"].([]any); len(a) != 1 || a[0].(map[string]any)["agent"] != "a" || a[0].(map[string]any)["runs"] != 2.0 {
		t.Fatalf("by_agent = %v", a)
	}
	days := tu["by_day"].([]any)
	if len(days) != 30 || days[29].(map[string]any)["attempts"] != 2.0 || days[0].(map[string]any)["cache_hit_ratio"] != nil {
		t.Fatalf("by_day = %v", days)
	}
	sk := u["skills"].([]any)
	if len(sk) != 1 {
		t.Fatalf("skills = %v", sk)
	}
	if s := sk[0].(map[string]any); s["skill"] != "tidy" || s["runs"] != 1.0 || !reflect.DeepEqual(s["versions"], []any{3.0}) || s["last_used_at"] == nil {
		t.Fatalf("skill use = %v", s)
	}
}
