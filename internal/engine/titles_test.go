package engine_test

import (
	"testing"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

func TestAConversationIsTitledOnceFromItsFirstMessage(t *testing.T) {
	fr := &fakeRunner{output: `{"title":"Uber Eats spending in October"}`}
	e, s, _ := harness(t, fr)
	open := func(title, body string) string {
		t.Helper()
		res, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "finance", SpecSHA: "x", Title: title, Body: body, Author: "kanishk"})
		if err != nil {
			t.Fatal(err)
		}
		return res.ConversationID
	}
	first := open("", "How much did I spend on Uber Eats in October, and was it more than September?")
	named := open("Mine", "Groceries?")

	// Without titles in orbit.yaml nothing is taken.
	if err := e.TitleConversations(ctx); err != nil || fr.calls != 0 {
		t.Fatalf("titles off: %v, %d calls", err, fr.calls)
	}
	cfg := *e.Current().Config
	cfg.Deployment.Titles = &spec.Titles{Runner: "fake"}
	if err := e.SetConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := e.TitleConversations(ctx); err != nil {
		t.Fatal(err)
	}
	c, _ := s.GetConversation(ctx, first)
	if c.Title != "Uber Eats spending in October" || c.TitleSource == nil || *c.TitleSource != "model" {
		t.Fatalf("titled = %q %v", c.Title, c.TitleSource)
	}
	if fr.calls != 1 || fr.got.Session != nil || fr.got.Prompt != "How much did I spend on Uber Eats in October, and was it more than September?" {
		t.Fatalf("calls %d, request %+v", fr.calls, fr.got)
	}
	if c, _ := s.GetConversation(ctx, named); c.Title != "Mine" || *c.TitleSource != "person" {
		t.Fatalf("a person's title = %q %v", c.Title, c.TitleSource)
	}
	// Once each, ever.
	if err := e.TitleConversations(ctx); err != nil || fr.calls != 1 {
		t.Fatalf("second pass: %v, %d calls", err, fr.calls)
	}
}
