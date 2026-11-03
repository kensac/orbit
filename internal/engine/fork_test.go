package engine_test

import (
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
)

func TestAForksFirstRunContinuesTheSessionAtItsPoint(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, "session: {resume: true}\n")
	sha := e.Current().Config.Agents["finance"].SHA
	send := func(conv, body string) string {
		t.Helper()
		res, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: sha, Body: body, Author: "kanishk",
			Wake: true, ConversationID: conv})
		if err != nil {
			t.Fatal(err)
		}
		c, err := s.ClaimRun(ctx, "test")
		if err != nil || c == nil {
			t.Fatal(c, err)
		}
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		return res.MessageID
	}
	first := send("", "October?")
	send("", "And November?")
	if fr.got.Session.Resume != "session-1" {
		t.Fatalf("the second run resumed %q", fr.got.Session.Resume)
	}
	var def string
	if err := s.Pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE agent = 'finance' AND is_default`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	f, _, err := s.ForkConversation(ctx, store.ForkRequest{ConversationID: def, MessageID: first})
	if err != nil {
		t.Fatal(err)
	}
	send(f.ID, "What about December?")
	got := fr.got
	if got.Session.Resume != "session-1" || got.Session.ResumeAt != "msg-session-1" {
		t.Fatalf("the fork's run resumed %q at %q, want session-1 at its last message", got.Session.Resume, got.Session.ResumeAt)
	}
	if strings.Contains(got.Prompt, "And November?") {
		t.Fatalf("the fork was shown what came after its point:\n%s", got.Prompt)
	}
	// The fork's next run continues its own session, not the parent's.
	send(f.ID, "Thanks")
	if fr.got.Session.Resume != "session-3" || fr.got.Session.ResumeAt != "" {
		t.Fatalf("the fork's second run resumed %q at %q", fr.got.Session.Resume, fr.got.Session.ResumeAt)
	}
}
