package engine_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

// A synthetic imported session; nothing here is from a real transcript.
const seededSession = "88888888-8888-4888-8888-888888888888"

func TestAConversationFromAnImportedSessionResumesIt(t *testing.T) {
	var transcript string
	fr := &fakeRunner{output: goodOutput}
	fr.seen = func(req runner.Request) {
		if req.Session != nil && req.Session.Resume != "" {
			p, _ := runner.SessionFile(req.Session.Home, req.Workdir, req.Session.Resume)
			b, _ := os.ReadFile(p)
			transcript = string(b)
		}
	}
	e, s, _ := harnessWith(t, fr, sessionSpec)
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	line := []byte(`{"message":{"content":"Why is the sprocket slow?","role":"user"},"type":"user","uuid":"u1"}`)
	res, err := s.ImportSessions(ctx, "claude-code", []store.SessionChunk{{SessionID: seededSession, ToOffset: 90, ToPrefix: strings.Repeat("a", 64),
		Lines: []store.SessionLine{{Content: line, Type: "user", At: &at, Prompt: "Why is the sprocket slow?"}}}}, false)
	if err != nil {
		t.Fatal(err)
	}

	// By the source's session id or orbit's: the same seed.
	seed, x, err := e.SessionSeed(ctx, seededSession)
	if err != nil || x.ID != res[0].ID {
		t.Fatalf("seed = %+v %+v %v", seed, x, err)
	}
	again, _, _ := e.SessionSeed(ctx, res[0].ID)
	if again.SessionID != seed.SessionID || seed.SessionID == seededSession {
		t.Fatalf("seed ids %s %s (the person's own is %s)", seed.SessionID, again.SessionID, seededSession)
	}

	a := e.Current().Config.Agents["finance"]
	cr, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "finance", SpecSHA: a.SHA, Body: "Continue this on the server.",
		Author: "kanishk", Wake: true, Seed: seed})
	if err != nil || cr.RunID == "" {
		t.Fatalf("create = %+v %v", cr, err)
	}
	conv, _ := s.GetConversation(ctx, cr.ConversationID)
	if conv.FromSession == nil || *conv.FromSession != x.ID {
		t.Fatalf("conversation = %+v", conv)
	}
	first := claimOn(t, s, store.Claim{Worker: "hetzner/0", Any: true})
	if err := e.Execute(ctx, first); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session == nil || fr.got.Session.Resume != seed.SessionID {
		t.Fatalf("the first run resumed %+v, want the seed %s", fr.got.Session, seed.SessionID)
	}
	if !strings.Contains(transcript, `"sessionId":"`+seed.SessionID+`"`) || !strings.Contains(transcript, "sprocket") {
		t.Fatalf("the seed's transcript on disk = %q", transcript)
	}
	run, _ := s.GetRun(ctx, first.ID)
	if run.Status != "succeeded" || *run.SessionMode != "resumed" || *run.SessionParent != seed.SessionID {
		t.Fatalf("run = %s %v %v", run.Status, *run.SessionMode, *run.SessionParent)
	}

	// Once a run of its own succeeded, the conversation continues its own session.
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{ConversationID: cr.ConversationID, Agent: "finance", SpecSHA: a.SHA, Body: "And the gear?", Author: "kanishk", Wake: true}); err != nil {
		t.Fatal(err)
	}
	second := claimOn(t, s, store.Claim{Worker: "hetzner/0", Any: true})
	if err := e.Execute(ctx, second); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session.Resume != "session-1" {
		t.Fatalf("the second run resumed %q, want the first run's session-1", fr.got.Session.Resume)
	}
}
