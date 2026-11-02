package engine

import (
	"context"
	"strings"
	"sync"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

// Steering a running reply (docs/ARCHITECTURE.md): a message posted with delivery steer while the conversation's chat run works is stored unread (store.steerTarget). The attempt running that turn finds it with the poll that watches for a stop (watchCancel, every cancelPoll), from the database, so it works whichever instance or rollout took the message; the runner writes it into the turn (runner/steer.go) and, when the CLI starts it, the run takes it (steered). One the turn did not take stays unread, and the run's end wakes a run for it.

// steerFeed is one chat turn's steer messages: those handed to the runner, by id.
type steerFeed struct {
	ch   chan runner.SteerMessage
	mu   sync.Mutex
	sent map[string]store.Message
}

// steerBuffer bounds the messages handed to a turn and not yet taken by the runner; a message past it waits for the next poll.
const steerBuffer = 32

func newSteerFeed() *steerFeed {
	return &steerFeed{ch: make(chan runner.SteerMessage, steerBuffer), sent: map[string]store.Message{}}
}

// pollSteers hands the turn the conversation's steer messages it has not seen.
func (e *Engine) pollSteers(ctx context.Context, c *store.ClaimedRun, f *steerFeed) {
	msgs, err := e.Store.PendingSteers(ctx, c.ID)
	if err != nil {
		e.Log.Warn("read steer messages", "run", c.ID, "err", err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range msgs {
		if _, seen := f.sent[m.ID]; seen {
			continue
		}
		select {
		case f.ch <- runner.SteerMessage{ID: m.ID, Text: strings.TrimSpace(m.Body)}:
			f.sent[m.ID] = m
		default:
			return
		}
	}
}

// steered records that the turn took a steer message: read by the run, and a steer item in its journal.
func (e *Engine) steered(ctx context.Context, c *store.ClaimedRun, f *steerFeed, id string, emit func(string, any)) {
	f.mu.Lock()
	m, ok := f.sent[id]
	f.mu.Unlock()
	if !ok {
		return
	}
	if err := e.Store.MarkSteered(context.WithoutCancel(ctx), c.ID, c.Attempt, id); err != nil {
		e.Log.Warn("record a steer", "run", c.ID, "message", id, "err", err)
		return
	}
	author := ""
	if m.Author != nil {
		author = *m.Author
	}
	emit("orbit.steer", map[string]any{"message_id": id, "text": strings.TrimSpace(m.Body), "author": author})
}
