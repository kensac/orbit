package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kensac/orbit/internal/claudeproto"
)

// ErrStalled is a turn the stall watchdog ended: the CLI wrote nothing, not
// even a keep_alive, for Stall.After while no tool call was running. The
// engine hands the run back, and the next attempt continues the session.
var ErrStalled = errors.New("the turn stalled")

// Stall is the watchdog for one turn in a streaming CLI process (a chat
// turn, a live process's turn).
type Stall struct {
	// After is how long without a line from the CLI is a stall
	// (spec session.stall).
	After time.Duration
	// ToolOpen reports whether a tool call of the attempt is still running
	// (the run's journal); an error is no stall, as the answer is unknown.
	ToolOpen func(ctx context.Context) (bool, error)
	// Grace is how long the interrupted turn gets to end before its process
	// is killed; claudeproto.CloseGrace when zero.
	Grace time.Duration

	fired atomic.Bool
	idle  atomic.Int64
}

// Fired says whether the watchdog ended the turn.
func (s *Stall) Fired() bool { return s != nil && s.fired.Load() }

// Idle is how long the turn had been silent when the watchdog fired.
func (s *Stall) Idle() time.Duration { return time.Duration(s.idle.Load()) }

// stallProc is what the watchdog needs of a process.
type stallProc interface {
	LastLine() time.Time
	Kill()
}

// watch guards one turn started now. interrupt asks the CLI to end the turn;
// a turn still going Grace after the interrupt was sent is ended by killing p. The returned
// stop ends the watch once the turn is over and waits for it.
func (s *Stall) watch(ctx context.Context, p stallProc, interrupt func(context.Context) error) (stop func()) {
	if s == nil || s.After <= 0 {
		return func() {}
	}
	start := time.Now()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// A tenth of the limit, so a stall is caught within 10% of it, and
		// at most every 30 s, the cost being one indexed read.
		t := time.NewTicker(min(max(s.After/10, 10*time.Millisecond), 30*time.Second))
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			last := p.LastLine()
			if last.Before(start) {
				last = start
			}
			idle := time.Since(last)
			if idle < s.After {
				continue
			}
			if s.ToolOpen != nil {
				if open, err := s.ToolOpen(ctx); err != nil || open {
					continue
				}
			}
			s.idle.Store(int64(idle))
			s.fired.Store(true)
			grace := s.Grace
			if grace <= 0 {
				grace = claudeproto.CloseGrace
			}
			// The grace runs from the interrupt's sending: a CLI that
			// never answers it is as stuck as one that never ends the
			// turn.
			ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
			go func() { _ = interrupt(ictx) }()
			select {
			case <-done:
			case <-ictx.Done():
				p.Kill()
			}
			cancel()
			return
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
}

// event is the one that ends a stalled turn, written after its last line:
// the journal marks that turn interrupted with reason stalled
// (journal.ReasonStalled).
func (s *Stall) event() Event {
	return Event{Kind: "orbit.stalled", Payload: fmt.Appendf(nil, `{"idle_ms":%d,"stall_ms":%d}`, s.Idle().Milliseconds(), s.After.Milliseconds())}
}

// stallError is what a stalled turn returns, naming how long it was silent.
func stallError(s *Stall) error {
	return fmt.Errorf("%w: no line from the CLI for %s and no tool call running", ErrStalled, s.Idle().Round(time.Second))
}
