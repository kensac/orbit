package tickets

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/store"
)

const laneBHead = "8888888888888888888888888888888888888888"

// racedTicket files a race of two lanes for kensac/orbit and approves it.
func (e *env) racedTicket() string {
	e.t.Helper()
	a := e.cfg.Agents["orbit"]
	r, _, err := e.s.CreateTicket(ctx, store.NewTicket{Kind: "request", Title: "Add a flag", Body: "please", Agent: "orbit",
		SpecSHA: a.SHA, Repo: "kensac/orbit", Author: "kanishk", Race: 2, Variants: []store.Variant{{Effort: "high"}}})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.ProposePlan(ctx, r.TicketID, "", "the plan"); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.ApprovePlan(ctx, r.TicketID, 1, "kanishk", ""); err != nil {
		e.t.Fatal(err)
	}
	return r.TicketID
}

// usageAt records a poll of the subscription's five-hour window at pct.
func (e *env) usageAt(pct int) {
	e.t.Helper()
	resets := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{"five_hour": map[string]any{"utilization": pct, "resets_at": resets}})
	if err := e.s.RecordUsageSample(ctx, "test", 200, body, nil); err != nil {
		e.t.Fatal(err)
	}
}

// finishLanes plays both lane builds: lane a delivers PR 7 at
// githubtest.Head, lane b PR 8 at laneBHead, each with its diff.
func (e *env) finishLanes() map[string]string {
	e.t.Helper()
	runs := map[string]string{}
	for range 2 {
		c, err := e.s.ClaimRun(ctx, "w")
		if err != nil || c == nil {
			e.t.Fatalf("claim = %+v %v", c, err)
		}
		b, _ := e.s.BuildOfRun(ctx, c.ID)
		lane := *b.Lane
		pr, head := map[string]int{"a": 7, "b": 8}[lane], map[string]string{"a": githubtest.Head, "b": laneBHead}[lane]
		if err := e.s.FinishBuild(ctx, c.ID, store.RunResult{Attempt: c.Attempt, Summary: "built"}, store.TicketUpdate{
			BaseSHA: "base", Branch: "orbit/3f2a9c1e-" + lane, PRNumber: &pr, HeadSHA: head,
			Diff: &store.BuildDiff{Added: map[string]int{"a": 20, "b": 5}[lane], Files: []string{"x.go"}}}); err != nil {
			e.t.Fatal(err)
		}
		runs[lane] = c.ID
	}
	return runs
}

func TestARaceIsBuiltComparedPickedAndMerged(t *testing.T) {
	e := newEnv(t, "auto")
	e.gh.Mu.Lock()
	e.gh.PR["head"].(map[string]any)["ref"] = "orbit/3f2a9c1e-a"
	e.gh.Others = map[int]map[string]any{8: {
		"number": 8, "state": "open", "draft": false, "merged": false, "title": "Add a flag (b)",
		"head": map[string]any{"ref": "orbit/3f2a9c1e-b", "sha": laneBHead, "repo": map[string]any{"full_name": "kensac/orbit"}},
		"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "kensac/orbit", "default_branch": "main"}},
	}}
	e.gh.CommitChecks = map[string][]map[string]any{laneBHead: {
		{"name": "check", "status": "completed", "conclusion": "failure", "app": map[string]any{"id": 15368}}}}
	e.gh.Mu.Unlock()
	id := e.racedTicket()

	// Nearly spent: the race waits in approved.
	e.usageAt(91)
	e.tick()
	if st := e.ticket(id).Status; st != store.TicketApproved {
		t.Fatalf("with the subscription nearly spent: %s", st)
	}
	e.usageAt(30)
	e.tick()
	if st := e.ticket(id).Status; st != store.TicketBuilding {
		t.Fatalf("after the window dropped: %s", st)
	}
	runs := e.finishLanes()
	if st := e.ticket(id).Status; st != store.TicketReview {
		t.Fatalf("after both lanes: %s", st)
	}

	// In review before the pick: each lane's checks are read, nothing is
	// merged, rerun or fixed.
	e.tick()
	lanes, err := e.s.RaceLanes(ctx, id)
	if err != nil || len(lanes) != 2 {
		t.Fatalf("lanes = %+v %v", lanes, err)
	}
	a, b := lanes[0], lanes[1]
	if *a.Checks != "green" || *a.ChecksHead != githubtest.Head || *b.Checks != "failed" || *b.PRState != "open" || a.Rank != 1 || b.Rank != 2 {
		t.Fatalf("a = %+v\nb = %+v", a, b)
	}
	if b.Variant.Effort != "" || a.Variant.Effort != "high" {
		t.Fatalf("variants a %+v b %+v", a.Variant, b.Variant)
	}
	e.gh.Mu.Lock()
	reruns, patched := len(e.gh.Reruns), len(e.gh.Patched)
	e.gh.Mu.Unlock()
	if tk := e.ticket(id); tk.Status != store.TicketReview || tk.MergeActionID != nil || reruns != 0 || patched != 0 {
		t.Fatalf("before the pick: %+v, reruns %d, patched %d", tk, reruns, patched)
	}

	// The pick: lane b's PR is closed, its branch kept, and lane a's PR
	// goes through the merge gate like any ticket's.
	if _, _, err := e.s.PickLane(ctx, store.Pick{TicketID: id, Lane: "a", By: "kanishk"}); err != nil {
		t.Fatal(err)
	}
	e.tick()
	e.gh.Mu.Lock()
	closed := e.gh.Others[8]["state"]
	var deletes []string
	for _, c := range e.gh.Calls {
		if strings.HasPrefix(c, "DELETE /repos/") {
			deletes = append(deletes, c)
		}
	}
	e.gh.Mu.Unlock()
	if closed != "closed" || len(deletes) != 0 {
		t.Fatalf("lane b's PR is %v; deletes %v", closed, deletes)
	}
	lanes, _ = e.s.RaceLanes(ctx, id)
	if lanes[1].PRClosedAt == nil || *lanes[1].PRState != "closed" || lanes[0].PRClosedAt != nil || !lanes[0].Picked {
		t.Fatalf("after the pick: a %+v\nb %+v", lanes[0], lanes[1])
	}
	tk := e.ticket(id)
	if tk.Status != store.TicketMerging || tk.MergeActionID == nil {
		t.Fatalf("after the pick: %+v", tk)
	}
	act, _ := e.s.GetAction(ctx, *tk.MergeActionID)
	if act.RunID != runs["a"] || act.Status != "pending" || !strings.Contains(string(act.Params), `"pull_number": 7`) {
		t.Fatalf("merge = %+v", act)
	}
}

func TestAnAbandonedRaceClosesEveryLanePR(t *testing.T) {
	e := newEnv(t, "auto")
	e.gh.Mu.Lock()
	e.gh.Others = map[int]map[string]any{8: {"number": 8, "state": "open", "merged": false,
		"head": map[string]any{"ref": "orbit/3f2a9c1e-b", "sha": laneBHead, "repo": map[string]any{"full_name": "kensac/orbit"}},
		"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "kensac/orbit", "default_branch": "main"}}}}
	e.gh.Mu.Unlock()
	id := e.racedTicket()
	e.tick()
	e.finishLanes()
	if err := e.s.TransitionTicket(ctx, id, store.TicketAbandoned, "kanishk", store.TicketUpdate{Reason: "not needed"}); err != nil {
		t.Fatal(err)
	}
	e.tick()
	e.gh.Mu.Lock()
	a, b := e.gh.PR["state"], e.gh.Others[8]["state"]
	e.gh.Mu.Unlock()
	if a != "closed" || b != "closed" {
		t.Fatalf("lane PRs after abandoning: %v %v", a, b)
	}
}
