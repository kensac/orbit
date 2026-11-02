package store_test

import (
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
	"github.com/kensac/orbit/internal/tokens"
)

const tokensMigration = 20261102045049

// A run finished before attempts recorded tokens had its usage on runs.usage only; the migration gives those counts to its latest attempt.
func TestTokensMigrationBackfillsFromRunUsage(t *testing.T) {
	s := testdb.Empty(t)
	if err := s.MigrateTo(ctx, tokensMigration-1); err != nil {
		t.Fatal(err)
	}
	conv := oldAgent(t, s, "a")
	finished := func(kind, model, usage string) string {
		t.Helper()
		return oldRun(t, s, "a", conv, `runner_kind = $2, model = NULLIF($3, ''), usage = $4::jsonb`, kind, model, usage)
	}
	// As runner.parseClaudeResult wrapped the recorded line's fields.
	claude := finished("claude-code", "haiku", `{"usage":{"input_tokens":2,"cache_creation_input_tokens":2960,"cache_read_input_tokens":649,"output_tokens":4},
		"model_usage":{"claude-haiku-5-5":{"inputTokens":2,"costUSD":0.0006}},"num_turns":1}`)
	codex := finished("codex", "gpt-x", `{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":50}`)
	none := finished("claude-code", "", `null`)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	check := func(id string, want *store.Tokens) {
		t.Helper()
		as, err := s.ListAttempts(ctx, id)
		if err != nil || len(as) != 1 {
			t.Fatalf("attempts = %v %v", as, err)
		}
		got := as[0].Tokens
		if want == nil {
			if got != nil {
				t.Fatalf("tokens = %+v, want none", got)
			}
			return
		}
		if got == nil || got.Input != want.Input || got.Output != want.Output || got.CacheRead != want.CacheRead ||
			got.CacheCreation != want.CacheCreation || got.Model != want.Model || got.NumTurns != want.NumTurns ||
			got.ByModel[want.Model] != (tokens.Count{Input: want.Input, Output: want.Output, CacheRead: want.CacheRead, CacheCreation: want.CacheCreation}) {
			t.Fatalf("tokens = %+v, want %+v", got, want)
		}
	}
	check(claude, &store.Tokens{Input: 2, Output: 4, CacheRead: 649, CacheCreation: 2960, Model: "claude-haiku-5-5", NumTurns: 1})
	check(codex, &store.Tokens{Input: 200, Output: 50, CacheRead: 800, Model: "gpt-x", NumTurns: 1})
	check(none, nil)
}
