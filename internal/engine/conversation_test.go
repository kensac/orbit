package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
)

func TestConversationFitsTheBudgetNewestKept(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var items []store.ConversationItem // newest first, as the store returns them
	for i := 40; i >= 1; i-- {
		items = append(items, store.ConversationItem{At: t0.Add(time.Duration(i) * time.Hour), Kind: "report",
			Summary: fmt.Sprintf("run %d", i), Body: strings.Repeat("x", 2000)})
	}
	// The newest is huge: it is clipped, not allowed to push out the rest.
	items[0].Body = strings.Repeat("é", 20000)
	text, chosen := conversationText(items, time.UTC)
	if len(text) > conversationBudget {
		t.Fatalf("%d bytes over the %d budget", len(text), conversationBudget)
	}
	if len(chosen) < 4 || chosen[len(chosen)-1].Summary != "run 40" {
		t.Fatalf("kept %d items, last %q: the newest must be kept and shown last", len(chosen), chosen[len(chosen)-1].Summary)
	}
	if !strings.Contains(text, "[truncated: ") || !strings.HasSuffix(strings.TrimSpace(text), "bytes shown]") {
		t.Fatal("a clipped item must say so")
	}
	for i := 1; i < len(chosen); i++ {
		if !chosen[i-1].At.Before(chosen[i].At) {
			t.Fatal("items must be oldest first")
		}
	}
	again, _ := conversationText(items, time.UTC)
	if again != text {
		t.Fatal("the same thread must give the same bytes")
	}
}
