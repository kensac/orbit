package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

const prHead = "1111111111111111111111111111111111111111"

// inReview is an approved ticket whose build delivered PR 7 at prHead.
func inReview(t *testing.T, s *store.Store) store.TicketCreated {
	t.Helper()
	r := approved(t, s)
	run, err := s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: "build"})
	if err != nil {
		t.Fatal(err)
	}
	c := claimBuild(t, s, run)
	pr := 7
	if err := s.FinishBuild(ctx, run, store.RunResult{Attempt: c.Attempt, Summary: "built"},
		store.TicketUpdate{BaseSHA: "base", Branch: "orbit/x", PRNumber: &pr, HeadSHA: prHead}); err != nil {
		t.Fatal(err)
	}
	return r
}

func comment(t *testing.T, s *store.Store, ticket, path string, start, end int, body string) *store.ReviewComment {
	t.Helper()
	c, err := s.AddReviewComment(ctx, store.NewComment{TicketID: ticket, Path: path, StartLine: start, EndLine: end, Body: body, Author: "kanishk"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func statuses(t *testing.T, s *store.Store, ticket string) map[string]string {
	t.Helper()
	cs, err := s.ReviewComments(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, c := range cs {
		out[c.Body] = c.Status
	}
	return out
}

func TestReviewCommentsAreDeliveredOnceTheirBuildSucceeds(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r := approved(t, s)
	if _, err := s.AddReviewComment(ctx, store.NewComment{TicketID: r.TicketID, Path: "a.go", StartLine: 1, EndLine: 1, Body: "x"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a comment before there is a PR = %v", err)
	}
	r = inReview(t, s)
	for _, bad := range []store.NewComment{
		{Path: "../etc/passwd", StartLine: 1, EndLine: 1, Body: "x"},
		{Path: "/abs", StartLine: 1, EndLine: 1, Body: "x"},
		{Path: "a//b.go", StartLine: 1, EndLine: 1, Body: "x"},
		{Path: "a.go", StartLine: 3, EndLine: 2, Body: "x"},
		{Path: "a.go", StartLine: 0, EndLine: 2, Body: "x"},
		{Path: "a.go", StartLine: 1, EndLine: 1, Body: "  "},
		{Path: "a.go", StartLine: 1, EndLine: 1, Body: strings.Repeat("x", store.MaxCommentBytes+1)},
	} {
		bad.TicketID = r.TicketID
		if _, err := s.AddReviewComment(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%+v = %v", bad, err)
		}
	}
	if _, _, err := s.SubmitReview(ctx, r.TicketID, "kanishk", "", ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a review with no drafts = %v", err)
	}
	one := comment(t, s, r.TicketID, "internal/x.go", 10, 12, "rename this")
	gone := comment(t, s, r.TicketID, "README.md", 0, 0, "never mind")
	edited := comment(t, s, r.TicketID, "README.md", 3, 3, "first wording")
	if one.Status != store.CommentDraft || one.HeadSHA != prHead || *one.Author != "kanishk" {
		t.Fatalf("draft = %+v", one)
	}
	if err := s.WithdrawReviewComment(ctx, r.TicketID, gone.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.WithdrawReviewComment(ctx, r.TicketID, gone.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("withdraw twice = %v", err)
	}
	v2, err := s.EditReviewComment(ctx, r.TicketID, edited.ID, 3, 4, "second wording\nwith a `> 9.` line", "kanishk")
	if err != nil || v2.Supersedes == nil || *v2.Supersedes != edited.ID || v2.Path != "README.md" {
		t.Fatalf("edit = %+v %v", v2, err)
	}
	if _, err := s.EditReviewComment(ctx, r.TicketID, edited.ID, 3, 3, "a fork", "kanishk"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second edit of the same version = %v", err)
	}

	res, replayed, err := s.SubmitReview(ctx, r.TicketID, "kanishk", "Two things.", "rv1")
	if err != nil || replayed || res.Comments != 2 || res.MessageID == "" {
		t.Fatalf("submit = %+v %v %v", res, replayed, err)
	}
	if again, replayed, _ := s.SubmitReview(ctx, r.TicketID, "kanishk", "Two things.", "rv1"); !replayed || again != res {
		t.Fatalf("retry = %+v %v", again, replayed)
	}
	want := map[string]string{"rename this": "submitted", "never mind": "withdrawn", "first wording": "superseded",
		"second wording\nwith a `> 9.` line": "submitted"}
	if got := statuses(t, s, r.TicketID); !mapsEqual(got, want) {
		t.Fatalf("after submit: %v", got)
	}
	if err := s.WithdrawReviewComment(ctx, r.TicketID, one.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("withdraw a submitted comment = %v", err)
	}
	msgs, _ := s.TicketMessages(ctx, r.TicketID)
	last := msgs[len(msgs)-1].Body
	if !strings.HasPrefix(last, "Review of PR #7 at 111111111111: 2 comments.\n\nTwo things.\n\n1. `internal/x.go`, lines 10-12\n   > rename this\n") ||
		!strings.Contains(last, "2. `README.md`, lines 3-4\n   > second wording\n   > with a `> 9.` line\n") {
		t.Fatalf("message = %q", last)
	}

	// The loop's revise is given both; a failed run delivers neither, and
	// the next build is given them again.
	if n, _ := s.PendingReviewComments(ctx, r.TicketID); n != 2 {
		t.Fatalf("pending = %d", n)
	}
	run, err := s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: store.BuildPurposeRevise})
	if err != nil {
		t.Fatal(err)
	}
	if given, _ := s.BuildComments(ctx, run); len(given) != 2 {
		t.Fatalf("given = %+v", given)
	}
	c := claimBuild(t, s, run)
	if err := s.FailBuild(ctx, run, store.RunResult{Attempt: c.Attempt, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	if got := statuses(t, s, r.TicketID)["rename this"]; got != "submitted" {
		t.Fatalf("after a failed build: %s", got)
	}
	// Submitting on a failed ticket with nothing new resends what waits:
	// back to review, where the loop revises.
	res, _, err = s.SubmitReview(ctx, r.TicketID, "kanishk", "", "")
	if err != nil || res.Comments != 0 || status(t, s, r.TicketID) != store.TicketReview {
		t.Fatalf("resubmit = %+v %v, %s", res, err, status(t, s, r.TicketID))
	}
	run, err = s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: store.BuildPurposeRevise})
	if err != nil {
		t.Fatal(err)
	}
	c = claimBuild(t, s, run)
	pr := 7
	if err := s.FinishBuild(ctx, run, store.RunResult{Attempt: c.Attempt, Summary: "revised"},
		store.TicketUpdate{PRNumber: &pr, HeadSHA: "2222222222222222222222222222222222222222"}); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.ReviewComments(ctx, r.TicketID)
	for _, c := range cs {
		if c.Body == "rename this" && (c.Status != "delivered" || c.DeliveredBy == nil || *c.DeliveredBy != run) {
			t.Fatalf("after the revise: %+v", c)
		}
	}
	if n, _ := s.PendingReviewComments(ctx, r.TicketID); n != 0 {
		t.Fatalf("pending after delivery = %d", n)
	}
	if _, err := s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: store.BuildPurposeRevise}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a revise with nothing to apply = %v", err)
	}
	// A delivered comment edited is a new draft at the new head; the
	// delivered version stays delivered.
	v3, err := s.EditReviewComment(ctx, r.TicketID, one.ID, 10, 10, "rename it again", "kanishk")
	if err != nil || v3.HeadSHA != "2222222222222222222222222222222222222222" {
		t.Fatalf("edit after delivery = %+v %v", v3, err)
	}
	if got := statuses(t, s, r.TicketID); got["rename this"] != "delivered" || got["rename it again"] != "draft" {
		t.Fatalf("%v", got)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
