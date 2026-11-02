package runner

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/spec"
)

// A chat run's permission level (spec.Levels) is a Claude Code permission mode. Seen against the real CLI 2.1.316 (claudeproto's TestRealCLIPermissionModes): set_permission_mode takes acceptEdits, dontAsk, plan and default on any process; bypassPermissions only on one launched with --allow-dangerously-skip-permissions ("the session was not launched with --dangerously-skip-permissions" otherwise); auto only where the model offers it ("auto mode unavailable for this model" on claude-haiku-4-5, taken on sonnet).

// PermissionMode is the CLI's mode for a level; a name that is not a level is dontAsk.
func PermissionMode(level string) string {
	switch level {
	case spec.LevelAsk:
		return "default"
	case spec.LevelEdits:
		return "acceptEdits"
	case spec.LevelAuto:
		return "auto"
	case spec.LevelFull:
		return "bypassPermissions"
	}
	return "dontAsk"
}

// startMode is the mode a process is launched in for level: auto is asked for once the process runs, since a model without it would refuse the flag, so it starts as ask.
func startMode(level string) string {
	if level == spec.LevelAuto {
		return PermissionMode(spec.LevelAsk)
	}
	return PermissionMode(level)
}

// Prompt decides one permission request the CLI sends (a call its mode and rules leave as "ask"). It is called on its own goroutine while the turn waits; ctx ends when the CLI withdraws the request. Whatever it returns must already be recorded.
type Prompt func(ctx context.Context, req claudeproto.PermissionRequest) claudeproto.Permission

// noPrompt answers a request in a run with no Prompt: denied, with the reason the model reads.
func noPrompt(_ context.Context, _ claudeproto.PermissionRequest) claudeproto.Permission {
	return claudeproto.Permission{Message: "orbit has no one to ask in this run, so the call is denied"}
}

// permissionEvent is what orbit.permission records for a chat run's request: the decision the journal's approval item ends with.
func permissionEvent(req claudeproto.PermissionRequest, p claudeproto.Permission) []byte {
	b, _ := json.Marshal(map[string]any{"request_id": req.RequestID, "tool": req.Tool, "allowed": p.Allow, "message": p.Message})
	return b
}

// modeSetter is a process whose permission mode can change while it runs.
type modeSetter interface {
	SetPermissionMode(ctx context.Context, mode string) error
}

// applyLevel puts a running process at level, and returns the level it is at: auto that the CLI refuses (a model without it) is ask, recorded as an orbit.permission_mode event either way. An error means the process is at neither and must not take the turn.
func applyLevel(ctx context.Context, p modeSetter, level string, emit func(Event)) (string, error) {
	err := p.SetPermissionMode(ctx, PermissionMode(level))
	applied := level
	var refused *claudeproto.ControlError
	if err != nil && level == spec.LevelAuto && errors.As(err, &refused) {
		applied = spec.LevelAsk
		if err2 := p.SetPermissionMode(ctx, PermissionMode(applied)); err2 != nil {
			return "", err2
		}
	} else if err != nil {
		return "", err
	}
	ev := map[string]any{"level": level, "applied": applied, "mode": PermissionMode(applied)}
	if err != nil {
		ev["reason"] = err.Error()
	}
	b, _ := json.Marshal(ev)
	emit(Event{Kind: "orbit.permission_mode", Payload: b})
	return applied, nil
}
