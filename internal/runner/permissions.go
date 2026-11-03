package runner

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

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

// modeForLevel selects the CLI mode for a level while preserving explicit connector prompts at locked.
func modeForLevel(level string, connectorPrompts bool) string {
	if level == spec.LevelLocked && connectorPrompts {
		return PermissionMode(spec.LevelAsk)
	}
	return PermissionMode(level)
}

func startModeForLevel(level string, connectorPrompts bool) string {
	if level == spec.LevelAuto {
		return PermissionMode(spec.LevelAsk)
	}
	return modeForLevel(level, connectorPrompts)
}

// Prompt decides one permission request the CLI sends (a call its mode and rules leave as "ask"). It is called on its own goroutine while the turn waits; ctx ends when the CLI withdraws the request. Whatever it returns must already be recorded.
type Prompt func(ctx context.Context, req claudeproto.PermissionRequest) claudeproto.Permission

// noPrompt answers a request in a run with no Prompt: denied, with the reason the model reads.
func noPrompt(_ context.Context, _ claudeproto.PermissionRequest) claudeproto.Permission {
	return claudeproto.Permission{Message: "orbit has no one to ask in this run, so the call is denied"}
}

// LevelChoice is why a run is at its level, recorded with every application.
type LevelChoice struct {
	Source    string     `json:"source"`
	Version   int        `json:"version,omitempty"`
	SetBy     string     `json:"set_by,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// LevelChange is a new level for the turn in progress.
type LevelChange struct {
	Level  string
	Choice LevelChoice
}

// modeSetter is a process whose permission mode can change while it runs.
type modeSetter interface {
	SetPermissionMode(ctx context.Context, mode string) error
}

func setLevel(ctx context.Context, p modeSetter, level, mode string) (applied, appliedMode, reason string, err error) {
	err = p.SetPermissionMode(ctx, mode)
	var refused *claudeproto.ControlError
	if err != nil && level == spec.LevelAuto && errors.As(err, &refused) {
		fallbackMode := PermissionMode(spec.LevelAsk)
		if err2 := p.SetPermissionMode(ctx, fallbackMode); err2 != nil {
			return "", "", "", err2
		}
		return spec.LevelAsk, fallbackMode, err.Error(), nil
	}
	if err != nil {
		return "", "", "", err
	}
	return level, mode, "", nil
}

func levelEvent(level, applied, mode, when, reason string, choice LevelChoice) Event {
	m := map[string]any{"level": level, "applied": applied, "mode": mode, "when": when, "source": choice.Source}
	if choice.Version > 0 {
		m["version"] = choice.Version
	}
	if choice.SetBy != "" {
		m["set_by"] = choice.SetBy
	}
	if choice.ExpiresAt != nil {
		m["expires_at"] = choice.ExpiresAt
	}
	if reason != "" {
		m["reason"] = reason
	}
	b, _ := json.Marshal(m)
	return Event{Kind: "orbit.permissions", Payload: b}
}

type procLevel struct {
	mu                sync.Mutex
	want, level, mode string
	exitPlans         map[string]*planExitState
}

type planExitState struct {
	result  chan struct{}
	done    chan struct{}
	seen    bool
	pending bool
}

func (l *procLevel) set(want, level, mode string) {
	l.mu.Lock()
	l.want, l.level, l.mode = want, level, mode
	l.mu.Unlock()
}

func (l *procLevel) request(level string) {
	l.mu.Lock()
	l.want = level
	l.mu.Unlock()
}

func (l *procLevel) needs(level, mode string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.want != level || l.mode != mode
}

func (l *procLevel) applied() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

func (l *procLevel) safeLevel() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if spec.LevelRank(l.want) < spec.LevelRank(l.level) {
		return l.want
	}
	return l.level
}

func (l *procLevel) reportedMode() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mode
}

func (l *procLevel) beginPlanExit(toolUseID string) {
	if toolUseID == "" {
		return
	}
	l.mu.Lock()
	if l.exitPlans == nil {
		l.exitPlans = make(map[string]*planExitState)
	}
	if _, exists := l.exitPlans[toolUseID]; !exists {
		l.exitPlans[toolUseID] = &planExitState{result: make(chan struct{}), done: make(chan struct{})}
	}
	l.mu.Unlock()
}

func (l *procLevel) finishPlanExit(toolUseID string) {
	l.mu.Lock()
	if state := l.exitPlans[toolUseID]; state != nil && !state.seen {
		state.seen = true
		close(state.result)
		if !state.pending {
			delete(l.exitPlans, toolUseID)
			close(state.done)
		}
	}
	l.mu.Unlock()
}

func (l *procLevel) beginChange() {
	l.mu.Lock()
	for _, state := range l.exitPlans {
		state.pending = true
	}
	l.mu.Unlock()
}

func (l *procLevel) completeChange() {
	l.mu.Lock()
	for id, state := range l.exitPlans {
		state.pending = false
		if state.seen {
			delete(l.exitPlans, id)
			close(state.done)
		}
	}
	l.mu.Unlock()
}

func (l *procLevel) waitPlanExit(ctx context.Context) bool {
	for {
		l.mu.Lock()
		var waits []chan struct{}
		for _, state := range l.exitPlans {
			if !state.seen {
				waits = append(waits, state.result)
			}
		}
		l.mu.Unlock()
		if len(waits) == 0 {
			return true
		}
		for _, ch := range waits {
			select {
			case <-ctx.Done():
				return false
			case <-ch:
			}
		}
	}
}

func (l *procLevel) waitPlanExitDone(ctx context.Context) bool {
	for {
		l.mu.Lock()
		var waits []chan struct{}
		for _, state := range l.exitPlans {
			waits = append(waits, state.done)
		}
		l.mu.Unlock()
		if len(waits) == 0 {
			return true
		}
		for _, ch := range waits {
			select {
			case <-ctx.Done():
				return false
			case <-ch:
			}
		}
	}
}

func (l *procLevel) observe(line claudeproto.Line) {
	if line.Kind == "user" {
		var ev struct {
			Message struct {
				Content []struct {
					Type      string `json:"type"`
					ToolUseID string `json:"tool_use_id"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line.Raw, &ev) == nil {
			for _, block := range ev.Message.Content {
				if block.Type == "tool_result" {
					l.finishPlanExit(block.ToolUseID)
				}
			}
		}
		return
	}
	if line.Kind != "system.init" && line.Kind != "system.status" {
		return
	}
	var v struct {
		Mode string `json:"permissionMode"`
	}
	if json.Unmarshal(line.Raw, &v) == nil && v.Mode != "" {
		l.mu.Lock()
		l.mode = v.Mode
		l.mu.Unlock()
	}
}

func (l *procLevel) apply(ctx context.Context, p modeSetter, level, mode, when string, choice LevelChoice, emit func(Event)) error {
	applied, appliedMode, reason, err := setLevel(ctx, p, level, mode)
	if err != nil {
		l.request(level)
		return err
	}
	l.set(level, applied, appliedMode)
	emit(levelEvent(level, applied, appliedMode, when, reason, choice))
	return nil
}

func withoutMode(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--permission-mode" {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func watchLevel(ctx, turnCtx context.Context, p *liveProc, chat *Chat, emit func(Event)) <-chan struct{} {
	done := make(chan struct{})
	if chat.Changes == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-turnCtx.Done():
				return
			case ch := <-chat.Changes:
				p.lv.beginChange()
				if !p.lv.waitPlanExit(turnCtx) {
					return
				}
				for {
					select {
					case next := <-chat.Changes:
						ch = next
					default:
						goto applyChange
					}
				}
			applyChange:
				was := p.lv.applied()
				wasMode := p.lv.reportedMode()
				actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeproto.RequestTimeout)
				mode := modeForLevel(ch.Level, chat.ConnectorPrompts)
				// Make a lowering visible to permission prompts before the CLI starts
				// applying it. A raise stays inactive because safeLevel reports the
				// applied level until setLevel succeeds.
				p.lv.request(ch.Level)
				if err := p.lv.apply(actx, p.proc, ch.Level, mode, "live", ch.Choice, emit); err != nil {
					reason := "not applied: " + err.Error()
					if spec.LevelRank(ch.Level) < spec.LevelRank(was) {
						_, interruptErr := p.proc.Interrupt(actx, p.proc.Capable(claudeproto.CapInterruptCancelQueued))
						if interruptErr != nil {
							reason += "; interrupt failed: " + interruptErr.Error()
							p.proc.Kill()
						}
					}
					emit(levelEvent(ch.Level, was, wasMode, "live", reason, ch.Choice))
				}
				p.lv.completeChange()
				cancel()
			}
		}
	}()
	return done
}
