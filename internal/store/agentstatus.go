package store

import (
	"time"

	"github.com/kensac/orbit/internal/status"
)

// RunFacts is what status.Derive reads of one run, scanned from the JSON
// runFacts builds.
type RunFacts struct {
	ID            string           `json:"id"`
	Status        string           `json:"status"`
	Attempt       int              `json:"attempt"`
	CreatedAt     time.Time        `json:"created_at"`
	StartedAt     *time.Time       `json:"started_at"`
	FinishedAt    *time.Time       `json:"finished_at"`
	Error         *string          `json:"error"`
	Hook          *status.Snapshot `json:"hook"`
	OpenQuestion  bool             `json:"open_question"`
	PendingAction bool             `json:"pending_action"`
}

func (f *RunFacts) run() *status.Run {
	if f == nil {
		return nil
	}
	return &status.Run{ID: f.ID, Status: f.Status, Attempt: f.Attempt, CreatedAt: f.CreatedAt,
		StartedAt: f.StartedAt, FinishedAt: f.FinishedAt, Error: f.Error,
		OpenQuestion: f.OpenQuestion, PendingAction: f.PendingAction, Hook: f.Hook}
}

// StatusFacts is what an agent's or a conversation's status reads: its
// current run, and whether anything of it waits on its person.
type StatusFacts struct {
	Run           *RunFacts `json:"run"`
	OpenQuestion  bool      `json:"open_question"`
	PendingAction bool      `json:"pending_action"`
}

// Status derives the status (status.Aggregate).
func (f StatusFacts) Status() status.Status {
	return status.Aggregate(f.Run.run(), f.OpenQuestion, f.PendingAction)
}

// runFacts is a run (alias x) as RunFacts' JSON. Its questions and actions
// are read through messages_run and actions_run.
func runFacts(x string) string {
	return `json_build_object('id', ` + x + `.id, 'status', ` + x + `.status, 'attempt', ` + x + `.attempt,
	    'created_at', ` + x + `.created_at, 'started_at', ` + x + `.started_at, 'finished_at', ` + x + `.finished_at,
	    'error', ` + x + `.error, 'hook', ` + x + `.hook_status,
	    'open_question', EXISTS (SELECT 1 FROM messages m WHERE m.run_id = ` + x + `.id AND ` + openQuestion + `),
	    'pending_action', EXISTS (SELECT 1 FROM actions pa WHERE pa.run_id = ` + x + `.id AND pa.status = 'pending'))`
}

// currentRun is the facts of the current run among those where matches
// (over runs sr): the running one (runs_one_running, so at most one per
// agent), else the newest. byAgent names the agent, which keeps the
// running lookup on that index.
func currentRun(byAgent, where string) string {
	return `COALESCE(
	    (SELECT ` + runFacts("sr") + ` FROM runs sr WHERE sr.agent = ` + byAgent + ` AND sr.status = 'running' AND ` + where + ` LIMIT 1),
	    (SELECT ` + runFacts("sr") + ` FROM runs sr WHERE ` + where + ` ORDER BY sr.created_at DESC LIMIT 1))`
}
