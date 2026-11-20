package store_test

import (
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestAPersonsTitleWinsOverTheModels(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	res, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Body: "## Rent in *October*", Author: "kanishk"})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ClaimTitles(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ConversationID != res.ConversationID || jobs[0].Body != "## Rent in *October*" {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	if again, err := s.ClaimTitles(ctx, 10); err != nil || len(again) != 0 {
		t.Fatalf("claimed twice: %+v %v", again, err)
	}
	title := "Rent paid late"
	if err := s.UpdateConversation(ctx, res.ConversationID, store.ConversationUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	cost := 0.001
	if set, err := s.SetModelTitle(ctx, res.ConversationID, "October rent", &cost); err != nil || set {
		t.Fatalf("the model's title replaced a person's: %v %v", set, err)
	}
	if c, _ := s.GetConversation(ctx, res.ConversationID); c.Title != "Rent paid late" || *c.TitleSource != "person" {
		t.Fatalf("conversation = %+v", c)
	}
}
