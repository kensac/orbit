package spec

import (
	"fmt"
	"slices"
)

// Permission levels say how a chat agent's Claude Code session asks before a tool runs (docs/agents/configuration.md, Permissions). They do not add tools: which tools exist is still `tools` and the sandbox profile; a level decides which of those run without a person. Ordered from the narrowest.
const (
	// LevelLocked runs only what the spec allows by name, and denies the rest without asking (Claude Code's dontAsk). Every unattended run is at most this wide in what it asks: nobody is there to answer.
	LevelLocked = "locked"
	// LevelAsk asks the person for every call the CLI's rules leave as "ask" (default mode, through the stdio permission prompt).
	LevelAsk = "ask"
	// LevelEdits accepts file edits in the workspace without asking, and asks for the rest (acceptEdits).
	LevelEdits = "edits"
	// LevelAuto lets the CLI's classifier decide, asking the person when it cannot (auto), where the CLI and model offer it; elsewhere it is ask.
	LevelAuto = "auto"
	// LevelFull runs every call without asking (bypassPermissions). Only in a sandbox, which is then the whole boundary.
	LevelFull = "full"
)

// Levels lists the permission levels from the narrowest to the widest.
var Levels = []string{LevelLocked, LevelAsk, LevelEdits, LevelAuto, LevelFull}

// Permissions is an agent's permission level and the widest a conversation may raise it to.
type Permissions struct {
	// Level is what every run of the agent starts at: a conversation's own level replaces it for runs its person's messages start.
	Level string `yaml:"level,omitempty" json:"level,omitempty"`
	// Ceiling is the widest level a conversation may set; at least Level.
	Ceiling string `yaml:"ceiling,omitempty" json:"ceiling,omitempty"`
}

// LevelRank is a level's place in Levels, -1 for a name that is not a level.
func LevelRank(level string) int { return slices.Index(Levels, level) }

// lowerLevel is the narrower of two levels; a name that is not a level is locked.
func lowerLevel(a, b string) string {
	ra, rb := LevelRank(a), LevelRank(b)
	if ra < 0 || rb < 0 {
		return LevelLocked
	}
	if rb < ra {
		return b
	}
	return a
}

// maxUnsandboxed is the widest level a run on orbit's host may have: auto and full let a call run without a person beside orbit's own secrets.
const maxUnsandboxed = LevelEdits

// PermissionDefaults are an agent's level and ceiling when its spec names none: a sandboxed chat runs full (the container is the boundary, as it always has been), every other agent locked.
func (a *Agent) PermissionDefaults() (level, ceiling string) {
	if a.Chat && a.Sandbox != "" {
		return LevelFull, LevelFull
	}
	return LevelLocked, LevelLocked
}

// PermissionLevel is the agent's own level, and PermissionCeiling the widest a conversation may set, each resolved down to what the agent can run (ResolveLevel).
func (a *Agent) PermissionLevel() string {
	level, _ := a.permissionSpec()
	return a.ResolveLevel(level)
}

// PermissionCeiling is the widest level a conversation of the agent may set.
func (a *Agent) PermissionCeiling() string {
	_, ceiling := a.permissionSpec()
	return a.capLevel(ceiling)
}

func (a *Agent) permissionSpec() (level, ceiling string) {
	level, ceiling = a.PermissionDefaults()
	if p := a.Permissions; p != nil {
		if p.Level != "" {
			level = p.Level
			if p.Ceiling == "" && LevelRank(level) > LevelRank(ceiling) {
				ceiling = level
			}
		}
		if p.Ceiling != "" {
			ceiling = p.Ceiling
		}
	}
	return level, ceiling
}

// capLevel resolves a level down to what the agent can ever run: a name that is not a level is locked, a report agent is locked, and a run on orbit's host is at most edits.
func (a *Agent) capLevel(level string) string {
	if LevelRank(level) < 0 || !a.Chat {
		return LevelLocked
	}
	if a.Sandbox == "" {
		return lowerLevel(level, maxUnsandboxed)
	}
	return level
}

// ResolveLevel is the level a run of the agent gets for want (a conversation's level, or the agent's own): never wider than the ceiling or than the agent can run, and locked for a name that is not a level. Every unknown or unsupported value resolves down, never up.
func (a *Agent) ResolveLevel(want string) string {
	return lowerLevel(a.capLevel(want), a.PermissionCeiling())
}

// UnattendedLevel is the level of a run nobody is watching: the agent's own, or the conversation's when that is narrower. A conversation's raised level is its person's, for the runs they start.
func (a *Agent) UnattendedLevel(conversation string) string {
	own := a.PermissionLevel()
	if conversation == "" {
		return own
	}
	return lowerLevel(own, a.ResolveLevel(conversation))
}

// validatePermissions checks an agent's permissions: known levels, a level within its ceiling, nothing above locked for an agent that is not a chat, and nothing above edits outside a sandbox.
func (a *Agent) validatePermissions() []error {
	p := a.Permissions
	if p == nil {
		return nil
	}
	var errs []error
	at := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agent %q: permissions: "+format, append([]any{a.Name}, args...)...))
	}
	for field, v := range map[string]string{"level": p.Level, "ceiling": p.Ceiling} {
		if v != "" && LevelRank(v) < 0 {
			at("%s %q is not a level (want one of %v)", field, v, Levels)
		}
	}
	if len(errs) > 0 {
		return errs
	}
	level, ceiling := a.permissionSpec()
	if LevelRank(level) > LevelRank(ceiling) {
		at("level %s is above the ceiling %s", level, ceiling)
	}
	widest := ceiling
	if LevelRank(level) > LevelRank(widest) {
		widest = level
	}
	switch {
	case !a.Chat && widest != LevelLocked:
		at("%s needs chat: true (an agent that is not a chat runs unattended, so it is locked)", widest)
	case a.Sandbox == "" && LevelRank(widest) > LevelRank(maxUnsandboxed):
		at("%s needs a sandbox profile: without one the session runs on orbit's host, where a call that runs without a person reaches orbit's own secrets", widest)
	}
	return errs
}
