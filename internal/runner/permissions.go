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

// LevelChoice is why a run is at its level, recorded with every application of it (orbit.permissions).
type LevelChoice struct {
	// Source is "conversation" when the conversation's own version set the level, else "agent".
	Source    string     `json:"source"`
	Version   int        `json:"version,omitempty"`
	SetBy     string     `json:"set_by,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// LevelChange is a new level for the turn in progress: its person changed the conversation's, or it expired.
type LevelChange struct {
	Level  string
	Choice LevelChoice
}

// modeSetter is a process whose permission mode can change while it runs.
type modeSetter interface {
	SetPermissionMode(ctx context.Context, mode string) error
}

// setLevel puts a running process at level, and returns the level it is at: auto that the CLI refuses (a model without it) is ask, with the refusal as reason. An error means the process is at neither.
func setLevel(ctx context.Context, p modeSetter, level string) (applied, reason string, err error) {
	err = p.SetPermissionMode(ctx, PermissionMode(level))
	var refused *claudeproto.ControlError
	if err != nil && level == spec.LevelAuto && errors.As(err, &refused) {
		if err2 := p.SetPermissionMode(ctx, PermissionMode(spec.LevelAsk)); err2 != nil {
			return "", "", err2
		}
		return spec.LevelAsk, err.Error(), nil
	}
	if err != nil {
		return "", "", err
	}
	return level, "", nil
}

// levelEvent is orbit.permissions: the level a run asked for, the one its process is at, and when it was applied: "start" (a new process), "turn" (a live process taking the run) or "live" (during the turn).
func levelEvent(level, applied, when, reason string, choice LevelChoice) Event {
	m := map[string]any{"level": level, "applied": applied, "mode": PermissionMode(applied), "when": when, "source": choice.Source}
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

// procLevel is what a chat process's permissions are: the level last asked for, the level it is at (auto refused is ask), and the CLI mode its stream last reported (the model's EnterPlanMode moves it, an approved ExitPlanMode moves it back to default).
type procLevel struct {
	mu                sync.Mutex
	want, level, mode string
	// left is signalled when the CLI leaves plan mode for a mode other than its level's (an approved ExitPlanMode goes to default), so the level is put back.
	left chan struct{}
}

func (l *procLevel) leftPlan() chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.left == nil {
		l.left = make(chan struct{}, 1)
	}
	return l.left
}

// planning reports whether the CLI is in plan mode: a level change waits until it leaves, so it never cuts a plan short.
func (l *procLevel) planning() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mode == "plan"
}

// defer records a level to apply once the plan is done.
func (l *procLevel) deferTo(level string) {
	l.mu.Lock()
	l.want = level
	l.mu.Unlock()
}

func (l *procLevel) wanted() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.want
}

func (l *procLevel) set(want, level string) {
	l.mu.Lock()
	l.want, l.level, l.mode = want, level, PermissionMode(level)
	l.mu.Unlock()
}

// needs reports whether the process must be moved to take a run at level.
func (l *procLevel) needs(level string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.want != level || l.mode != PermissionMode(l.level)
}

func (l *procLevel) applied() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

// observe reads the mode the CLI reports on its init and status lines.
func (l *procLevel) observe(line claudeproto.Line) {
	if line.Kind != "system.init" && line.Kind != "system.status" {
		return
	}
	var v struct {
		Mode string `json:"permissionMode"`
	}
	if json.Unmarshal(line.Raw, &v) == nil && v.Mode != "" {
		l.mu.Lock()
		was := l.mode
		l.mode = v.Mode
		left := was == "plan" && v.Mode != "plan" && (v.Mode != PermissionMode(l.level) || l.want != l.level)
		l.mu.Unlock()
		if left {
			select {
			case l.leftPlan() <- struct{}{}:
			default:
			}
		}
	}
}

// apply moves the process to level and records it.
func (l *procLevel) apply(ctx context.Context, p modeSetter, level, when string, choice LevelChoice, emit func(Event)) error {
	applied, reason, err := setLevel(ctx, p, level)
	if err != nil {
		return err
	}
	l.set(level, applied)
	emit(levelEvent(level, applied, when, reason, choice))
	return nil
}

// withoutMode drops --permission-mode and its value: a live process is kept across level changes (set_permission_mode moves it), so the level is not part of what it runs.
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

// watchLevel applies the level changes the engine sends during the turn, and puts the level back when the CLI leaves plan mode, until turnCtx ends; the returned channel closes once it has stopped, so nothing is recorded after the run. A change while the CLI is planning waits for the plan to end. A change the process refuses is recorded with its reason; a lowering it refuses interrupts the turn, so nothing more runs at the wider level.
func watchLevel(ctx, turnCtx context.Context, p *liveProc, chat *Chat, emit func(Event)) <-chan struct{} {
	done := make(chan struct{})
	choice := chat.Choice
	apply := func(level, when string) {
		was := p.lv.applied()
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeproto.RequestTimeout)
		defer cancel()
		if err := p.lv.apply(actx, p.proc, level, when, choice, emit); err != nil {
			emit(levelEvent(level, was, when, "not applied: "+err.Error(), choice))
			if spec.LevelRank(level) < spec.LevelRank(was) {
				_ = p.proc.Interrupt(actx)
			}
		}
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-turnCtx.Done():
				return
			case ch := <-chat.Changes:
				choice = ch.Choice
				if p.lv.planning() {
					p.lv.deferTo(ch.Level)
					emit(levelEvent(ch.Level, p.lv.applied(), "live", "the session is planning: the level applies when the plan ends", choice))
					continue
				}
				apply(ch.Level, "live")
			case <-p.lv.leftPlan():
				apply(p.lv.wanted(), "plan")
			}
		}
	}()
	return done
}
