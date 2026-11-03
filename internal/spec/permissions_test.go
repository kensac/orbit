package spec

import (
	"reflect"
	"strings"
	"testing"
)

func TestPermissionDefaultsAndResolution(t *testing.T) {
	report := &Agent{Name: "r"}
	local := &Agent{Name: "l", Chat: true}
	box := &Agent{Name: "b", Chat: true, Sandbox: SandboxWorkstation}
	cases := []struct {
		a              *Agent
		level, ceiling string
	}{
		{report, LevelLocked, LevelLocked},
		{local, LevelLocked, LevelLocked},
		{box, LevelFull, LevelFull},
		{&Agent{Chat: true, Permissions: &Permissions{Level: LevelAsk}}, LevelAsk, LevelAsk},
		{&Agent{Chat: true, Permissions: &Permissions{Level: LevelLocked, Ceiling: LevelEdits}}, LevelLocked, LevelEdits},
		{&Agent{Chat: true, Sandbox: SandboxBuild, Permissions: &Permissions{Level: LevelAsk}}, LevelAsk, LevelFull},
		// Values a load would refuse still resolve down at run time: a stored spec, a ceiling lowered since.
		{&Agent{Chat: true, Permissions: &Permissions{Level: LevelFull, Ceiling: LevelFull}}, LevelEdits, LevelEdits},
		{&Agent{Chat: true, Sandbox: SandboxBuild, Permissions: &Permissions{Level: "sudo", Ceiling: "root"}}, LevelLocked, LevelLocked},
		{&Agent{Permissions: &Permissions{Level: LevelFull, Ceiling: LevelFull}}, LevelLocked, LevelLocked},
	}
	for i, c := range cases {
		if l, ce := c.a.PermissionLevel(), c.a.PermissionCeiling(); l != c.level || ce != c.ceiling {
			t.Errorf("case %d: level %s ceiling %s, want %s %s", i, l, ce, c.level, c.ceiling)
		}
	}
	edits := &Agent{Chat: true, Sandbox: SandboxWorkstation, Permissions: &Permissions{Level: LevelAsk, Ceiling: LevelEdits}}
	for want, got := range map[string]string{
		LevelEdits: edits.ResolveLevel(LevelFull), LevelAsk: edits.ResolveLevel(LevelAsk),
		LevelLocked: edits.ResolveLevel("bogus"),
	} {
		if got != want {
			t.Errorf("ResolveLevel resolved to %s, want %s", got, want)
		}
	}
	// An unattended run takes a conversation's level only when it is narrower.
	if l := box.UnattendedLevel(LevelAsk); l != LevelAsk {
		t.Errorf("a lowered conversation: %s", l)
	}
	if l := edits.UnattendedLevel(LevelEdits); l != LevelAsk {
		t.Errorf("a raised conversation reached an unattended run: %s", l)
	}
	if l := edits.UnattendedLevel(""); l != LevelAsk {
		t.Errorf("no conversation level: %s", l)
	}
}

func TestPermissionsValidation(t *testing.T) {
	cfg := base + "sandbox: {sandboxd: \"sandboxd:8080\"}\n"
	load := func(agent string) error {
		_, err := Load(write(t, map[string]string{"orbit.yaml": cfg, "agents/a.yaml": "name: a\ncharter: x\n" + agent}))
		return err
	}
	for agent, want := range map[string]string{
		"chat: true\npermissions: {level: sudo}\n":                                     `level "sudo" is not a level`,
		"chat: true\nsandbox: build\npermissions: {level: full, ceiling: ask}\n":       "level full is above the ceiling ask",
		"permissions: {level: ask}\n":                                                  "ask needs chat: true",
		"chat: true\npermissions: {ceiling: full}\n":                                   "full needs a sandbox profile",
		"chat: true\npermissions: {level: auto}\n":                                     "auto needs a sandbox profile",
		"chat: true\nsandbox: workstation\npermissions: {level: locked, ceiling: x}\n": `ceiling "x" is not a level`,
	} {
		if err := load(agent); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", agent, err, want)
		}
	}
	for _, agent := range []string{
		"chat: true\npermissions: {level: ask, ceiling: edits}\n",
		"chat: true\nsandbox: workstation\npermissions: {level: ask, ceiling: full}\n",
		"chat: true\nsandbox: build\npermissions: {level: auto}\n",
		"permissions: {level: locked}\n",
	} {
		if err := load(agent); err != nil {
			t.Errorf("%q: %v", agent, err)
		}
	}
}

func TestRaisingPermissionsWidens(t *testing.T) {
	cur := &Agent{Chat: true, Sandbox: SandboxWorkstation, Permissions: &Permissions{Level: LevelAsk, Ceiling: LevelEdits}}
	next := &Agent{Chat: true, Sandbox: SandboxWorkstation, Permissions: &Permissions{Level: LevelEdits, Ceiling: LevelFull}}
	want := []string{"permission ceiling full, was edits", "permission level edits, was ask"}
	if w := Widening(cur, next); !reflect.DeepEqual(w, want) {
		t.Fatalf("widening = %q, want %q", w, want)
	}
	if w := Widening(next, cur); len(w) != 0 {
		t.Fatalf("lowering widens: %q", w)
	}
}
