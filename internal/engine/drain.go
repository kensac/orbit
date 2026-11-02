package engine

import (
	"sync"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// Drain is an instance's switch from serving to leaving. A draining
// instance claims no run and no approved action, reports not ready
// (/readyz), and keeps serving the API and open streams while the runs it
// holds finish; then it exits. A deploy starts the new instance, waits for
// it to be ready, and drains the old one, so nobody sees the switch.
type Drain struct {
	once sync.Once
	ch   chan struct{}
	mu   sync.Mutex
	at   time.Time
}

// NewDrain returns a Drain that has not started.
func NewDrain() *Drain { return &Drain{ch: make(chan struct{})} }

// Start begins draining; later calls do nothing. It reports whether this
// call started it.
func (d *Drain) Start() bool {
	started := false
	d.once.Do(func() {
		d.mu.Lock()
		d.at = time.Now()
		d.mu.Unlock()
		close(d.ch)
		started = true
	})
	return started
}

// Draining reports whether Start was called.
func (d *Drain) Draining() bool {
	select {
	case <-d.ch:
		return true
	default:
		return false
	}
}

// Done is closed when draining starts.
func (d *Drain) Done() <-chan struct{} { return d.ch }

// Since is when draining started, zero before.
func (d *Drain) Since() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.at
}

// DrainMargin is what a run may take beyond its agent's timeout, which
// bounds the backend call only: fetching its sources before (each request
// bounded by the HTTP client's two minutes) and recording and delivering
// its outcome after (a minute per channel).
const DrainMargin = 5 * time.Minute

// DrainMax is how long a drain waits for the runs it holds before handing
// the rest back: the longest agent timeout plus DrainMargin, so a run that
// can still finish within its own timeout does.
func DrainMax(cfg *spec.Config) time.Duration {
	longest := time.Duration(cfg.Deployment.Defaults.Timeout)
	for _, a := range cfg.Agents {
		longest = max(longest, time.Duration(a.Timeout))
	}
	return longest + DrainMargin
}

// ActionTimeout bounds one action's execution (ExecuteActions).
const ActionTimeout = 2 * time.Minute

// OrphanAfter is how long an action may stay executing before it is taken
// for orphaned by a dead process: twice ActionTimeout, so an action a live
// instance is executing is never failed under it.
const OrphanAfter = 2 * ActionTimeout
