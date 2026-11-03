package engine

import (
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

func TestLevelFor(t *testing.T) {
	a := &spec.Agent{Chat: true, Sandbox: spec.SandboxWorkstation, Permissions: &spec.Permissions{Level: spec.LevelAsk, Ceiling: spec.LevelFull}}
	now := time.Now()
	past, later := now.Add(-time.Minute), now.Add(time.Hour)
	row := func(level string, exp *time.Time) *store.ConversationPermission {
		return &store.ConversationPermission{Version: 3, Level: level, SetBy: "kanishk", ExpiresAt: exp}
	}
	cases := []struct {
		name     string
		attended bool
		cp       *store.ConversationPermission
		level    string
		source   string
	}{
		{"no row", true, nil, spec.LevelAsk, "agent"},
		{"raised, attended", true, row(spec.LevelFull, &later), spec.LevelFull, "conversation"},
		{"raised, unattended", false, row(spec.LevelFull, nil), spec.LevelAsk, "agent"},
		{"lowered, unattended", false, row(spec.LevelLocked, nil), spec.LevelLocked, "conversation"},
		{"expired", true, row(spec.LevelFull, &past), spec.LevelAsk, "agent"},
		{"the agent's own", true, row("", nil), spec.LevelAsk, "agent"},
		{"unknown resolves down", true, row("root", nil), spec.LevelLocked, "conversation"},
	}
	for _, c := range cases {
		got := levelFor(a, c.attended, c.cp, now)
		if got.Level != c.level || got.Choice.Source != c.source {
			t.Errorf("%s: %s from %s, want %s from %s", c.name, got.Level, got.Choice.Source, c.level, c.source)
		}
		if c.source == "conversation" && (got.Choice.Version != 3 || got.Choice.SetBy != "kanishk") {
			t.Errorf("%s: choice %+v", c.name, got.Choice)
		}
	}
}
