package engine_test

import (
	"testing"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

func TestARaisedConversationLevelIsItsPersonsOnly(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, "chat: true\npermissions: {level: locked, ceiling: edits}\n")
	a := e.Current().Config.Agents["finance"]
	var conv string
	if err := s.Pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE agent = 'finance' AND is_default`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetConversationPermission(ctx, conv, spec.LevelEdits, "kanishk", nil); err != nil {
		t.Fatal(err)
	}
	run := func(enqueue func()) {
		t.Helper()
		enqueue()
		c, err := s.ClaimRun(ctx, "test")
		if err != nil || c == nil {
			t.Fatalf("claim: %v %v", c, err)
		}
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	run(func() {
		if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: a.SHA, Body: "go", Wake: true}); err != nil {
			t.Fatal(err)
		}
	})
	if c := fr.got.Chat; c == nil || c.Level != spec.LevelEdits || c.Ceiling != spec.LevelEdits || c.Choice.Source != "conversation" || c.Choice.SetBy != "kanishk" || c.Changes == nil {
		t.Fatalf("the person's run got %+v", fr.got.Chat)
	}
	run(func() {
		if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "finance", SpecSHA: a.SHA, Trigger: "manual"}); err != nil {
			t.Fatal(err)
		}
	})
	if c := fr.got.Chat; c.Level != spec.LevelLocked || c.Choice.Source != "agent" {
		t.Fatalf("an unattended run took the raised level: %+v", c)
	}
}
