package status

import "time"

// Snapshot is what the hooks last said about a run's turn. It is stored on
// the run (runs.hook_status) by the attempt that received it.
type Snapshot struct {
	State  State  `json:"state"`
	Kind   Kind   `json:"kind,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Event is the hook event that set State, and At when its hook ran.
	Event   string    `json:"event"`
	At      time.Time `json:"at"`
	Attempt int       `json:"attempt"`
	// PromptID is the turn the snapshot is about.
	PromptID string `json:"prompt_id,omitempty"`
}

// Tracker folds one run attempt's hook events into a Snapshot. It is not
// safe for concurrent use; the listener calls it from one goroutine.
//
// Which events are the run's own: a live process serves many runs, one
// turn each, and a hook that could not reach the listener spools its
// event to be read later, out of order. So an event stamped before the
// attempt started is an earlier run's, and the first event after it that
// names a turn (prompt_id) adopts that turn; events of any other turn are
// ignored. Within the turn, an event older than the snapshot does not
// overwrite it.
type Tracker struct {
	attempt int
	since   time.Time
	prompt  string
	snap    *Snapshot
	// waiter is whose wait the snapshot reports: "" for the main agent,
	// a subagent's id for one of its own. Only the same agent's next event
	// (or the turn's end) clears a wait.
	waiter string
}

// NewTracker starts a fold for an attempt that began at since.
func NewTracker(attempt int, since time.Time) *Tracker {
	return &Tracker{attempt: attempt, since: since}
}

// Snapshot is the current snapshot, nil before any event changed it.
func (t *Tracker) Snapshot() *Snapshot {
	if t.snap == nil {
		return nil
	}
	s := *t.snap
	return &s
}

// Own reports whether ev belongs to this attempt's turn, adopting the turn
// on its first event that names one.
func (t *Tracker) Own(ev Event) bool {
	if ev.At.Before(t.since) {
		return false
	}
	if ev.PromptID == "" {
		return true
	}
	if t.prompt == "" {
		t.prompt = ev.PromptID
	}
	return ev.PromptID == t.prompt
}

// Apply folds ev in, and reports whether the snapshot changed in a way
// worth storing: its state, its kind, or what a wait or an error names.
// Events Own rejects change nothing.
func (t *Tracker) Apply(ev Event) bool {
	if !t.Own(ev) {
		return false
	}
	if t.snap != nil && ev.At.Before(t.snap.At) {
		return false
	}
	next, waiter, ok := t.next(ev)
	if !ok {
		return false
	}
	next.Event, next.At, next.Attempt, next.PromptID = ev.Name, ev.At, t.attempt, t.prompt
	changed := t.snap == nil || t.snap.State != next.State || t.snap.Kind != next.Kind || t.snap.Detail != next.Detail
	t.snap, t.waiter = &next, waiter
	return changed
}

// next is the snapshot ev leads to, and whose wait it is; ok is false when
// ev says nothing about the turn's state.
func (t *Tracker) next(ev Event) (Snapshot, string, bool) {
	waiting := t.snap != nil && t.snap.State == Waiting
	finished := t.snap != nil && (t.snap.State == Done || t.snap.State == Errored)
	sub := ev.AgentID != ""

	// What blocks the person blocks it whoever asked: a subagent's
	// permission request holds the turn as much as the main agent's.
	if k, detail, ok := waitOn(ev); ok {
		return Snapshot{State: Waiting, Kind: k, Detail: detail}, ev.AgentID, true
	}
	switch ev.Name {
	case "UserPromptSubmit":
		return Snapshot{State: Working}, "", true
	case "Stop":
		if sub {
			return Snapshot{}, "", false
		}
		return Snapshot{State: Done}, "", true
	case "StopFailure":
		// A subagent's failure ends its own work, not the turn.
		if sub {
			return Snapshot{}, "", false
		}
		return Snapshot{State: Errored, Detail: orDefault(ev.Error, "the turn failed")}, "", true
	case "SessionEnd":
		// After Stop this is the process closing; before it, the turn
		// never finished.
		if finished || t.snap == nil {
			return Snapshot{}, "", false
		}
		return Snapshot{State: Errored, Detail: "the session ended before the turn finished (" + orDefault(ev.Reason, "no reason given") + ")"}, "", true
	case "PreToolUse", "PostToolUse", "PostToolUseFailure", "ElicitationResult", "SubagentStart":
		// Work resumed. A wait clears only by the agent that waits; a
		// finished turn is not reopened by a straggling event.
		if finished || (waiting && ev.AgentID != t.waiter) {
			return Snapshot{}, "", false
		}
		return Snapshot{State: Working}, "", true
	}
	// SessionStart (the process, before any turn), SubagentStop (the
	// turn goes on), PreCompact (fires on an aborted compaction too),
	// PostCompact (an automatic compaction is mid-turn; the turn's end is
	// still Stop), and notifications that block nothing.
	return Snapshot{}, "", false
}

// waitOn is what ev makes the turn wait for, if anything.
func waitOn(ev Event) (Kind, string, bool) {
	switch ev.Name {
	case "PermissionRequest":
		return Permission, ev.Tool, true
	case "Elicitation":
		return Elicitation, ev.Message, true
	case "PreToolUse":
		if ev.Tool == "AskUserQuestion" {
			return Question, "", true
		}
	case "Notification":
		switch ev.Notification {
		case "permission_prompt":
			return Permission, ev.Message, true
		case "elicitation_dialog":
			return Elicitation, ev.Message, true
		}
	}
	return "", "", false
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
