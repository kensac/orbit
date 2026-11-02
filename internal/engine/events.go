package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/kensac/orbit/internal/journal"
	"github.com/kensac/orbit/internal/store"
)

// eventEmitter serializes sequence allocation, transcript capture and the full event/journal write.
// Its persistence function must not call this emitter.
type eventEmitter struct {
	mu         sync.Mutex
	seq        int
	persist    func(int, string, []byte) error
	warn       func(error)
	capture    bool
	transcript [][]byte
	// said are the lines that can hold the agent's prose, for fullReport.
	said []journal.Event
}

func (e *Engine) events(ctx context.Context, c *store.ClaimedRun, log *slog.Logger, capture bool) *eventEmitter {
	return &eventEmitter{
		persist: func(seq int, kind string, payload []byte) error {
			return e.Store.AppendEvent(context.WithoutCancel(ctx), c.ID, c.Attempt, seq, kind, payload)
		},
		warn:    func(err error) { log.Warn("append event", "err", err) },
		capture: capture,
	}
}

func (e *eventEmitter) emit(kind string, payload []byte) {
	err := e.write(kind, payload)
	// Logging happens after the write lock is released, so a log handler can inspect the emitter without reentering its persistence critical section.
	if err != nil && e.warn != nil {
		e.warn(err)
	}
}

func (e *eventEmitter) write(kind string, payload []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	if e.capture && (kind == "assistant" || kind == "user") {
		e.transcript = append(e.transcript, bytes.Clone(payload))
	}
	if kind == "assistant" || kind == "response" || strings.HasPrefix(kind, "item.") {
		e.said = append(e.said, journal.Event{Seq: e.seq, Kind: kind, Payload: bytes.Clone(payload)})
	}
	return e.persist(e.seq, kind, payload)
}

// prose is the attempt's final prose so far (journal.FinalProse).
func (e *eventEmitter) prose() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return journal.FinalProse(e.said)
}

// messages takes an owned snapshot so figure checks cannot race new callbacks.
func (e *eventEmitter) messages() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]byte, len(e.transcript))
	for i, b := range e.transcript {
		out[i] = bytes.Clone(b)
	}
	return out
}
