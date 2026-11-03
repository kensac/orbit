package runner

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/kensac/orbit/internal/claudeproto"
)

// Steering a chat turn (docs/ARCHITECTURE.md, Steering a running reply): a message the person sends while the turn runs is written into it with its message id as the CLI's uuid. The CLI folds a message written during a tool call into the same turn, when the call's result goes back; one written after the turn's last tool call starts a turn of its own after the result (claudeproto.TestRealCLISteer). So a message waits until a tool call is in flight, and one still unwritten when the turn ends is the next run's. A message written that the turn did not take is withdrawn when the result arrives; one whose withdrawal is not confirmed may yet run, so the process is closed rather than kept live.

// SteerMessage is a person's message for the turn in progress.
type SteerMessage struct {
	// ID is the message's id, the uuid the CLI names it by.
	ID   string
	Text string
}

// steerProc is what steering needs of a chat process.
type steerProc interface {
	SendUUID(text, uuid string) error
	CancelQueued(ctx context.Context, uuid string) (bool, error)
}

// steering feeds one turn its steer messages.
type steering struct {
	proc    steerProc
	steered func(id string)
	emit    func(Event)

	mu      sync.Mutex
	open    map[string]bool // top-level tool calls in flight
	waiting []SteerMessage  // received, not written
	written map[string]bool // written, neither started nor ended
	closed  bool
	wake    chan struct{}
	// wmu is held while writing, so finish never races a write.
	wmu sync.Mutex
}

func newSteering(proc steerProc, c *Chat, emit func(Event)) *steering {
	return &steering{proc: proc, steered: c.Steered, emit: emit, open: map[string]bool{}, written: map[string]bool{}, wake: make(chan struct{}, 1)}
}

// feed takes messages from in until ctx ends, writing them while a tool call is in flight.
func (s *steering) feed(ctx context.Context, in <-chan SteerMessage) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-in:
			s.mu.Lock()
			s.waiting = append(s.waiting, m)
			s.mu.Unlock()
		case <-s.wake:
		}
		s.flush()
	}
}

func (s *steering) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// flush writes every waiting message when a tool call is in flight.
func (s *steering) flush() {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	if s.closed || len(s.open) == 0 || len(s.waiting) == 0 {
		s.mu.Unlock()
		return
	}
	batch := s.waiting
	s.waiting = nil
	for _, m := range batch {
		s.written[m.ID] = true
	}
	s.mu.Unlock()
	for _, m := range batch {
		if err := s.proc.SendUUID(m.Text, m.ID); err != nil {
			// Never reached the CLI: the next run's, like an unwritten one.
			s.mu.Lock()
			delete(s.written, m.ID)
			s.mu.Unlock()
		}
	}
}

// observe follows the turn's lines: the tool calls in flight, and the lifecycle of each message written.
func (s *steering) observe(l claudeproto.Line) {
	if lc, ok := l.Lifecycle(); ok {
		s.mu.Lock()
		was := s.written[lc.CommandUUID]
		if lc.State == claudeproto.LifecycleStarted || lc.Terminal() {
			delete(s.written, lc.CommandUUID)
		}
		s.mu.Unlock()
		switch {
		case was && lc.State == claudeproto.LifecycleStarted && s.steered != nil:
			s.steered(lc.CommandUUID)
		case was && lc.Terminal():
			// Ended without running (an interrupt withdrew it, or the CLI refused it): the next run's.
			s.withdrawn(lc.CommandUUID, true)
		}
		return
	}
	uses, results := toolBlocks(l)
	if len(uses) == 0 && len(results) == 0 {
		return
	}
	s.mu.Lock()
	for _, id := range uses {
		s.open[id] = true
	}
	for _, id := range results {
		delete(s.open, id)
	}
	s.mu.Unlock()
	if len(uses) > 0 {
		s.signal()
	}
}

// finish ends steering when the turn has ended: nothing more is written, and every message written that the turn did not take is withdrawn, unless the interrupt's receipt already did. It reports whether every withdrawal was confirmed; when not, a message may yet run in this process.
func (s *steering) finish(ctx context.Context, receipt *claudeproto.InterruptReceipt) bool {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	s.closed = true
	var left []string
	for id := range s.written {
		left = append(left, id)
	}
	s.written = map[string]bool{}
	s.mu.Unlock()
	cancelled := map[string]bool{}
	if receipt != nil {
		for _, id := range receipt.Cancelled {
			cancelled[id] = true
		}
	}
	clean := true
	for _, id := range left {
		ok := cancelled[id]
		if !ok {
			var err error
			ok, err = s.proc.CancelQueued(ctx, id)
			ok = ok && err == nil
		}
		clean = clean && ok
		s.withdrawn(id, ok)
	}
	return clean
}

// withdrawn records a message written into the turn that the turn did not take.
func (s *steering) withdrawn(id string, confirmed bool) {
	p, _ := json.Marshal(map[string]any{"message_id": id, "confirmed": confirmed})
	s.emit(Event{Kind: "orbit.steer_withdrawn", Payload: p})
}

// toolBlocks are the ids of the tool calls a top-level assistant line makes and of the results a top-level user line carries; a subagent's (parent_tool_use_id set) are its own.
func toolBlocks(l claudeproto.Line) (uses, results []string) {
	if l.Type != "assistant" && l.Type != "user" {
		return nil, nil
	}
	var m struct {
		Parent  *string `json:"parent_tool_use_id"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(l.Raw, &m) != nil || m.Parent != nil {
		return nil, nil
	}
	var blocks []struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		ToolUseID string `json:"tool_use_id"`
	}
	_ = json.Unmarshal(m.Message.Content, &blocks)
	for _, b := range blocks {
		switch {
		case l.Type == "assistant" && b.Type == "tool_use" && b.ID != "":
			uses = append(uses, b.ID)
		case l.Type == "user" && b.Type == "tool_result" && b.ToolUseID != "":
			results = append(results, b.ToolUseID)
		}
	}
	return uses, results
}
