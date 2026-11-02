package status

import "time"

// Status is an agent's derived status, as the API shows it.
type Status struct {
	State  State  `json:"state"`
	Kind   Kind   `json:"kind,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Since is when the state was entered, as far as orbit knows.
	Since *time.Time `json:"since,omitempty"`
	// Source is which producer decided it: "hooks" (the run's Claude Code
	// hook events) or "run" (the run's row).
	Source string `json:"source"`
	// RunID is the run it describes; empty for an agent or conversation
	// with no run.
	RunID string `json:"run_id,omitempty"`
}

// Sources.
const (
	SourceHooks = "hooks"
	SourceRun   = "run"
)

// Run is what Derive reads of a run.
type Run struct {
	ID         string
	Status     string
	Attempt    int
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	Error      *string
	// OpenQuestion and PendingAction: the finished run waits on its person.
	OpenQuestion  bool
	PendingAction bool
	// Hook is the stored hook snapshot, nil when no hook reported.
	Hook *Snapshot
}

// maxDetail bounds an error shown as Detail; the full error is the run's.
const maxDetail = 300

// Derive is a run's status. See the package comment for the precedence.
func Derive(r Run) Status {
	st := Status{Source: SourceRun, RunID: r.ID}
	switch r.Status {
	case "queued":
		st.State, st.Since = Queued, ptr(r.CreatedAt)
	case "running":
		if h := r.Hook; h != nil && h.Attempt == r.Attempt {
			st.Source, st.Kind, st.Detail, st.Since = SourceHooks, h.Kind, h.Detail, ptr(h.At)
			st.State = h.State
			// The turn ended but orbit is still checking and recording its
			// output: the reply is not there yet.
			if st.State == Done {
				st.State = Working
			}
			return st
		}
		st.State, st.Since = Working, r.StartedAt
	case "succeeded", "skipped":
		st.Since = r.FinishedAt
		switch {
		case r.PendingAction:
			st.State, st.Kind = Waiting, Approval
		case r.OpenQuestion:
			st.State, st.Kind = Waiting, Question
		default:
			st.State = Done
		}
	case "failed":
		st.State, st.Since = Errored, r.FinishedAt
		if r.Error != nil {
			st.Detail = clip(*r.Error, maxDetail)
		}
	default: // cancelled: it never ran
		st.State, st.Since = Idle, r.FinishedAt
	}
	return st
}

// None is the status of an agent or conversation with no run.
func None() Status { return Status{State: Idle, Source: SourceRun} }

func ptr(t time.Time) *time.Time { return &t }
