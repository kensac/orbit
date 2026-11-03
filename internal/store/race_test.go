package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// racing files a race of n lanes with agent a and approves its plan.
func racing(t *testing.T, s *store.Store, n int, variants ...store.Variant) store.TicketCreated {
	t.Helper()
	nt := newRequest("")
	nt.Race, nt.Variants = n, variants
	r, _, err := s.CreateTicket(ctx, nt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProposePlan(ctx, r.TicketID, "", "the plan"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApprovePlan(ctx, r.TicketID, 1, "kanishk", ""); err != nil {
		t.Fatal(err)
	}
	return r
}

// deliverLane claims the next build run and finishes it as its lane's PR.
func deliverLane(t *testing.T, s *store.Store, pr, lines int) string {
	t.Helper()
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil {
		t.Fatalf("claim = %v %v", c, err)
	}
	b, err := s.BuildOfRun(ctx, c.ID)
	if err != nil || b.Lane == nil {
		t.Fatalf("build = %+v %v", b, err)
	}
	if err := s.FinishBuild(ctx, c.ID, store.RunResult{Attempt: c.Attempt, Summary: "built"}, store.TicketUpdate{
		Branch: "orbit/x-" + *b.Lane, BaseSHA: "base", HeadSHA: "head-" + *b.Lane, PRNumber: &pr,
		Diff: &store.BuildDiff{Added: lines, Files: []string{"a.go"}}}); err != nil {
		t.Fatal(err)
	}
	return *b.Lane
}

func TestARaceIsCheckedWhenFiled(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	for want, nt := range map[string]store.NewTicket{
		"race is 2 to 3":         {Race: 1},
		"3 variants for 2 lanes": {Race: 2, Variants: make([]store.Variant, 3)},
		"not a model name":       {Race: 2, Variants: []store.Variant{{Model: "--dangerously-skip-permissions"}}},
		"effort must be one of":  {Race: 3, Variants: []store.Variant{{}, {Effort: "max"}, {Effort: "ultra"}}},
		"cannot be stacked":      {Race: 2, StackOn: "00000000-0000-0000-0000-000000000000"},
		"variants need a race":   {Variants: []store.Variant{{Model: "opus"}}},
	} {
		r := newRequest("")
		r.Race, r.Variants, r.StackOn = nt.Race, nt.Variants, nt.StackOn
		if _, _, err := s.CreateTicket(ctx, r); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
	q := store.NewTicket{Kind: "question", Title: "t", Body: "b", Agent: "a", SpecSHA: "sha-a", Race: 2}
	if _, _, err := s.CreateTicket(ctx, q); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a racing question = %v", err)
	}
	// Nothing stacks on a race.
	r := racing(t, s, 2)
	st := newRequest("")
	st.StackOn = r.TicketID
	if _, _, err := s.CreateTicket(ctx, st); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "is a race") {
		t.Fatalf("stacked on a race = %v", err)
	}
}

func TestARaceBuildsEveryLaneAndAPersonPicksOne(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r := racing(t, s, 3, store.Variant{Model: "claude-opus-4-1[1m]"}, store.Variant{Effort: "high"})
	start := store.StartBuild{TicketID: r.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: store.BuildPurposeBuild}
	if _, err := s.StartBuild(ctx, start); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a single build of a race = %v", err)
	}
	runs, err := s.StartRace(ctx, start)
	if err != nil || len(runs) != 3 || status(t, s, r.TicketID) != store.TicketBuilding {
		t.Fatalf("start = %v %v", runs, err)
	}
	if _, err := s.StartRace(ctx, start); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second start = %v", err)
	}
	// Lane variants reach the builds.
	byLane := map[string]*store.Build{}
	for _, id := range runs {
		b, err := s.BuildOfRun(ctx, id)
		if err != nil || b.Lane == nil {
			t.Fatalf("build = %+v %v", b, err)
		}
		byLane[*b.Lane] = b
	}
	if byLane["a"].Variant.Model != "claude-opus-4-1[1m]" || byLane["b"].Variant.Effort != "high" || byLane["c"].Variant != (store.Variant{}) {
		t.Fatalf("variants = %+v %+v %+v", byLane["a"].Variant, byLane["b"].Variant, byLane["c"].Variant)
	}

	// Lanes end one at a time; the ticket moves only once all have.
	first := deliverLane(t, s, 7, 30)
	if status(t, s, r.TicketID) != store.TicketBuilding {
		t.Fatal("the race settled with lanes still to build")
	}
	c, _ := s.ClaimRun(ctx, "w")
	fb, _ := s.BuildOfRun(ctx, c.ID)
	failed := *fb.Lane
	if err := s.FailBuild(ctx, c.ID, store.RunResult{Attempt: c.Attempt, Error: "diff policy: deploy/x is protected", DiffPolicyFailed: true}); err != nil {
		t.Fatal(err)
	}
	if status(t, s, r.TicketID) != store.TicketBuilding {
		t.Fatal("the race settled with a lane still to build")
	}
	third := deliverLane(t, s, 9, 10)
	tk, _ := s.GetTicket(ctx, r.TicketID)
	if tk.Status != store.TicketReview || tk.PRNumber != nil || tk.Branch != nil || !tk.Racing() {
		t.Fatalf("settled = %+v", tk)
	}

	lanes, err := s.RaceLanes(ctx, r.TicketID)
	if err != nil || len(lanes) != 3 {
		t.Fatalf("lanes = %+v %v", lanes, err)
	}
	for _, l := range lanes {
		switch {
		case l.Lane == failed && (l.Delivered || l.Rank != 3 || l.DiffPolicy == nil || *l.DiffPolicy != "failed" || l.Why != "the diff policy refused its diff"):
			t.Errorf("failed lane = %+v", l)
		case l.Lane == third && (l.Rank != 1 || l.LinesChanged != 10):
			t.Errorf("smaller lane = %+v", l)
		case l.Lane == first && (l.Rank != 2 || l.LinesChanged != 30 || *l.Branch != "orbit/x-"+first):
			t.Errorf("larger lane = %+v", l)
		}
	}
	// Checks green on the larger lane put it first.
	if err := s.RecordLaneChecks(ctx, r.TicketID, first, "head-"+first, "green", "open"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordLaneChecks(ctx, r.TicketID, third, "head-"+third, "failed", "open"); err != nil {
		t.Fatal(err)
	}
	lanes, _ = s.RaceLanes(ctx, r.TicketID)
	for _, l := range lanes {
		if (l.Lane == first) != (l.Rank == 1) || (l.Lane == first && !l.ChecksGreen) {
			t.Errorf("after checks: %+v", l)
		}
	}

	if _, _, err := s.PickLane(ctx, store.Pick{TicketID: r.TicketID, Lane: failed, By: "kanishk"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("picking a lane with no PR = %v", err)
	}
	act, _, err := s.PickLane(ctx, store.Pick{TicketID: r.TicketID, Lane: first, By: "kanishk"})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.GetAction(ctx, act)
	if a.Type != store.PickType || a.Status != "executed" || a.Rule != "approve" || *a.DecidedBy != "kanishk" || a.RunID != byLane[first].RunID {
		t.Fatalf("action = %+v", a)
	}
	tk, _ = s.GetTicket(ctx, r.TicketID)
	if tk.Status != store.TicketReview || *tk.PickedLane != first || *tk.PRNumber != 7 || *tk.HeadSHA != "head-"+first ||
		*tk.Branch != "orbit/x-"+first || *tk.BaseSHA != "base" || tk.Racing() {
		t.Fatalf("picked = %+v", tk)
	}
	if b, _ := s.LatestBuild(ctx, r.TicketID); b == nil || b.RunID != byLane[first].RunID {
		t.Fatalf("latest build = %+v, want the picked lane's", b)
	}
	// From here it is that lane's ticket: a fix builds on its lane.
	fix, err := s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: store.BuildPurposeFix,
		Failure: []byte(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := s.BuildOfRun(ctx, fix); b.Lane == nil || *b.Lane != first || b.Variant != byLane[first].Variant {
		t.Fatalf("fix = %+v", b)
	}
	fc, _ := s.ClaimRun(ctx, "w")
	pr := 7
	if err := s.FinishBuild(ctx, fc.ID, store.RunResult{Attempt: fc.Attempt, Summary: "fixed"},
		store.TicketUpdate{Branch: "orbit/x-" + first, PRNumber: &pr, HeadSHA: "head2"}); err != nil {
		t.Fatal(err)
	}
	if tk, _ := s.GetTicket(ctx, r.TicketID); tk.Status != store.TicketReview || *tk.HeadSHA != "head2" {
		t.Fatalf("after the fix = %+v", tk)
	}
}

func TestARaceWithNoLaneDeliveredFails(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r := racing(t, s, 2)
	runs, err := s.StartRace(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: store.BuildPurposeBuild})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.ClaimRun(ctx, "w")
	if err := s.FailBuild(ctx, c.ID, store.RunResult{Attempt: c.Attempt, Error: "timed out"}); err != nil {
		t.Fatal(err)
	}
	// The other lane is cancelled by a person: no FailBuild, so the loop
	// settles it.
	other := runs[0]
	if other == c.ID {
		other = runs[1]
	}
	if _, err := s.CancelRun(ctx, other, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if status(t, s, r.TicketID) != store.TicketBuilding {
		t.Fatal("settled before the loop saw the cancelled lane")
	}
	if err := s.SettleRace(ctx, r.TicketID); err != nil {
		t.Fatal(err)
	}
	tk, _ := s.GetTicket(ctx, r.TicketID)
	if tk.Status != store.TicketFailed || !strings.Contains(*tk.Error, "every race build failed") ||
		!strings.Contains(*tk.Error, "timed out") || !strings.Contains(*tk.Error, "cancelled") {
		t.Fatalf("failed = %+v %v", tk, *tk.Error)
	}
	// Retrying races again, from every lane.
	if err := s.TransitionTicket(ctx, r.TicketID, store.TicketApproved, "kanishk", store.TicketUpdate{}); err != nil {
		t.Fatal(err)
	}
	again, err := s.StartRace(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: store.BuildPurposeBuild})
	if err != nil || len(again) != 2 {
		t.Fatalf("retry = %v %v", again, err)
	}
	lanes, _ := s.RaceLanes(ctx, r.TicketID)
	for _, l := range lanes {
		if *l.RunStatus != "queued" {
			t.Fatalf("after the retry a lane reads its old build: %+v", l)
		}
	}
}

func TestRankLanes(t *testing.T) {
	ok, open, head := "succeeded", "open", "h"
	n := func(i int) *int { return &i }
	f := func(x float64) *float64 { return &x }
	green, pending := "green", "pending"
	lanes := []store.Lane{
		{Lane: "a", RunStatus: &ok, PRNumber: n(1), HeadSHA: &head, Added: n(5), Files: []string{"x.go"}, CostUSD: f(1), Checks: &green, ChecksHead: &head, PRState: &open},
		{Lane: "b", RunStatus: &ok, PRNumber: n(2), HeadSHA: &head, Added: n(50), Files: []string{"x.go", "internal/x/x_test.go"}, CostUSD: f(3), Checks: &green, ChecksHead: &head, PRState: &open},
		{Lane: "c", RunStatus: &ok, PRNumber: n(3), HeadSHA: &head, Added: n(1), Files: []string{"web/a.spec.ts"}, CostUSD: f(1), Checks: &pending, ChecksHead: &head, PRState: &open},
	}
	store.RankLanes(lanes)
	// Green first; among green, the one that touched tests; then pending.
	if lanes[1].Rank != 1 || lanes[0].Rank != 2 || lanes[2].Rank != 3 || lanes[1].TestsTouched != 1 || lanes[2].TestsTouched != 1 {
		t.Fatalf("ranks = %d %d %d", lanes[0].Rank, lanes[1].Rank, lanes[2].Rank)
	}
	// Equal on everything else: fewer lines, then lower cost, then the letter.
	other := "x"
	lanes = []store.Lane{
		{Lane: "a", RunStatus: &ok, PRNumber: n(1), Added: n(9), CostUSD: f(1)},
		{Lane: "b", RunStatus: &ok, PRNumber: n(2), Added: n(3), Deleted: n(6), CostUSD: f(0.5)},
		{Lane: "c", RunStatus: &ok, PRNumber: n(3), Added: n(9), CostUSD: f(1), ChecksHead: &other},
	}
	store.RankLanes(lanes)
	if lanes[1].Rank != 1 || lanes[0].Rank != 2 || lanes[2].Rank != 3 {
		t.Fatalf("ties = %d %d %d", lanes[0].Rank, lanes[1].Rank, lanes[2].Rank)
	}
	for _, p := range []string{"a_test.go", "src/__tests__/x.js", "testdata/in.json", "pkg/tests/a.py", "b.test.ts"} {
		if !store.TestPath(p) {
			t.Errorf("TestPath(%q) = false", p)
		}
	}
	for _, p := range []string{"latest.go", "contest/x.go", "attest.md"} {
		if store.TestPath(p) {
			t.Errorf("TestPath(%q) = true", p)
		}
	}
}
