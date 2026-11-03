package api_test

import (
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
)

// TestAThreadFoldsIdenticalSkips: consecutive skipped runs of one trigger with one gate are one item, the newest, with the streak's count, first and last time and every run id; another gate, another trigger or a message between them starts a new streak, and fold=false lists every run.
func TestAThreadFoldsIdenticalSkips(t *testing.T) {
	v := newEnv(t, nil)
	sha := v.e.Current().Config.Agents["a"].SHA
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	n := 0
	skip := func(index int, gate string) string {
		t.Helper()
		n++
		at := base.Add(time.Duration(n) * time.Minute)
		id, err := v.s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: sha, Trigger: "schedule", TriggerIndex: &index, ScheduledFor: &at})
		if err != nil {
			t.Fatal(err)
		}
		c, err := v.s.ClaimRun(ctx, "test")
		if err != nil || c == nil || c.ID != id {
			t.Fatalf("claim %v %v", c, err)
		}
		if err := v.s.FinishRun(ctx, id, store.RunResult{Attempt: c.Attempt, Status: store.StatusSkipped, Gate: gate, Summary: "Skipped: " + id}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := []string{skip(0, "unchanged"), skip(0, "unchanged"), skip(0, "unchanged")}
	skip(0, "items_above")
	if _, err := v.s.SendToAgent(ctx, "a", "anything new?", "kanishk", nil); err != nil {
		t.Fatal(err)
	}
	second := []string{skip(0, "unchanged"), skip(0, "unchanged")}
	skip(1, "unchanged")

	url := v.internal.URL + "/api/v1/agents/a/thread"
	code, page := call(t, "GET", url, token, "")
	if code != 200 {
		t.Fatalf("thread = %d %v", code, page)
	}
	items := page["items"].([]any)
	if len(items) != 5 {
		t.Fatalf("folded thread has %d items, want 5: %v", len(items), items)
	}
	check := func(i int, ids []string) {
		t.Helper()
		it := items[i].(map[string]any)
		f, _ := it["folded"].(map[string]any)
		if ids == nil {
			if f != nil {
				t.Errorf("item %d folded %v, want a plain item", i, f)
			}
			return
		}
		if f == nil || f["count"] != float64(len(ids)) || it["id"] != ids[len(ids)-1] || len(f["run_ids"].([]any)) != len(ids) ||
			f["run_ids"].([]any)[0] != ids[0] || f["last_at"] != it["at"] {
			t.Errorf("item %d = %v, want the newest of %v folded", i, it, ids)
		}
	}
	check(0, first)
	check(1, nil)
	if items[2].(map[string]any)["kind"] != "message" {
		t.Errorf("item 2 = %v, want the message", items[2])
	}
	check(3, second)
	check(4, nil)

	code, page = call(t, "GET", url+"?fold=false", token, "")
	if code != 200 || len(page["items"].([]any)) != 8 {
		t.Fatalf("unfolded = %d, %d items", code, len(page["items"].([]any)))
	}
	if code, _ := call(t, "GET", url+"?fold=maybe", token, ""); code != 400 {
		t.Fatalf("fold=maybe = %d", code)
	}
}
