package store_test

import (
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestTheThreadMetersContextAndCost(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	conv, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Body: "hi", Author: "kanishk", Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ConversationThread(ctx, conv.ConversationID, nil, 0)
	if err != nil || page.Meter == nil || page.Meter.RunID != nil || page.Meter.Runs != 0 || page.Meter.ContextTokens != nil {
		t.Fatalf("before any run: %+v %v", page.Meter, err)
	}
	finish := func(events []string, usage string, cost, session float64) string {
		t.Helper()
		c, err := s.ClaimRun(ctx, "w")
		if err != nil || c == nil {
			t.Fatal(c, err)
		}
		for i, ev := range events {
			if err := s.AppendEvent(ctx, c.ID, c.Attempt, i+1, "assistant", []byte(ev)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: c.Attempt, Status: "succeeded", Report: "r",
			SessionID: "s1", CostUSD: &cost, SessionCostUSD: &session, Usage: []byte(usage)}, store.Outcome{}); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	finish([]string{`{"type":"assistant","message":{"usage":{"input_tokens":3,"cache_read_input_tokens":1000,"output_tokens":20}}}`},
		`{"usage":{"input_tokens":10,"cache_read_input_tokens":2000,"cache_creation_input_tokens":500,"output_tokens":40},"model_usage":{"claude-opus":{"contextWindow":200000}}}`,
		0.05, 0.05)
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: "more", Author: "kanishk", Wake: true,
		ConversationID: conv.ConversationID}); err != nil {
		t.Fatal(err)
	}
	last := finish([]string{
		`{"type":"assistant","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":3000,"output_tokens":10}}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":2,"cache_read_input_tokens":3100,"cache_creation_input_tokens":50,"output_tokens":30}}}`,
		`{"type":"assistant","message":{"content":[]}}`,
	}, `{"usage":{"input_tokens":7,"cache_read_input_tokens":6100,"output_tokens":40},"model_usage":{"claude-opus":{"contextWindow":200000},"claude-haiku":{"contextWindow":100000}}}`,
		0.02, 0.07)
	page, err = s.ConversationThread(ctx, conv.ConversationID, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	m := page.Meter
	if m.RunID == nil || *m.RunID != last || *m.SessionID != "s1" || *m.SessionCostUSD != 0.07 || *m.ContextWindow != 200000 {
		t.Fatalf("meter = %+v", m)
	}
	// The last message that reported usage: 2 + 3100 + 50 + 30.
	if m.ContextTokens == nil || *m.ContextTokens != 3182 {
		t.Fatalf("context = %v", m.ContextTokens)
	}
	if m.Runs != 2 || m.CostUSD < 0.0699 || m.CostUSD > 0.0701 ||
		m.Tokens != (store.MeterTokens{Input: 17, CacheRead: 8100, CacheCreation: 500, Output: 80}) {
		t.Fatalf("totals = %+v", m)
	}
}
