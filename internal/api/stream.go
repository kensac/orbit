package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/kensac/orbit/internal/journal"
	"github.com/kensac/orbit/internal/status"
	"github.com/kensac/orbit/internal/store"
)

// streamPoll is how often a stream reads the run's row and new events.
// Polling rather than LISTEN/NOTIFY: a stream then depends on nothing but
// the rows themselves, so every writer (the engine's events, the reaper's
// requeue, a cancel, another replica) is followed without each having to
// remember to notify, and no stream holds a dedicated connection. The cost
// is two indexed reads per open stream per interval, which for the handful
// of people watching a run is nothing; half a second keeps prose arriving
// as it is written.
const streamPoll = 500 * time.Millisecond

// streamPing is the heartbeat comment interval (docs/api.md), well inside
// the idle timeouts of the proxies between orbit and a client (Cloudflare
// closes an idle origin response at 100 s).
const streamPing = 15 * time.Second

// agentStatus is an agent_status event's data; the name keeps the status
// package reachable inside streamRun, whose run status is a local status.
type agentStatus = status.Status

// sameStatus reports whether a client showing a would show b the same.
func sameStatus(a, b agentStatus) bool {
	return a.State == b.State && a.Kind == b.Kind && a.Detail == b.Detail && a.Source == b.Source
}

// streamRun serves GET /runs/{id}/stream: Server-Sent Events replaying the
// journal of the run's latest timeline (epoch) from its start, then
// following it live until the run is final.
func (s *Server) streamRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := s.Store.GetRunProgress(r.Context(), id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	rc := http.NewResponseController(w)
	// A stream lasts as long as the run, longer than the server's write
	// timeout is meant for ordinary responses.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported: "+err.Error())
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var buf bytes.Buffer
	// add queues one event. id is set on the events of a journal row only,
	// so a client's Last-Event-ID is always the last row it received.
	add := func(event, id string, data any) {
		b, _ := json.Marshal(data)
		if id != "" {
			fmt.Fprintf(&buf, "id: %s\n", id)
		}
		fmt.Fprintf(&buf, "event: %s\ndata: %s\n\n", event, b)
	}
	// flush writes what is queued in one write, so a row's events reach a
	// client together or not at all.
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		_, err := w.Write(buf.Bytes())
		buf.Reset()
		if err != nil {
			return err
		}
		return rc.Flush()
	}

	// A reconnecting client names the last row it has; rows up to it in
	// its epoch are read but not sent, so chunk separators come out
	// exactly as in a full replay.
	resume, resuming := journal.ParseCursor(r.Header.Get("Last-Event-ID"))

	// sent is the last agent_status sent: one follows every status, and
	// one is sent on its own whenever it changes.
	var sent *agentStatus
	sendAgentStatus := func(p *store.RunProgress, force bool) {
		if !force && sent != nil && sameStatus(*sent, p.AgentStatus) {
			return
		}
		st := p.AgentStatus
		sent = &st
		add("agent_status", "", st)
	}

	var (
		started   bool
		epoch     int
		attempt   int
		continues int // the earlier attempt the current one continues, 0 for none
		seq       int // the last row read
		after     int // rows up to this one are not sent
		status    string
		// wrote is whether this timeline has sent prose yet: each text
		// event is a chunk to append, and a block after an earlier one
		// starts with a blank line, so the concatenation reads as
		// paragraphs.
		wrote bool
	)
	sendStatus := func() {
		m := map[string]any{"status": status, "attempt": attempt}
		if epoch > 0 {
			m["epoch"] = epoch
		}
		if continues > 0 {
			m["continues"] = continues
		}
		add("status", "", m)
	}
	// drain queues every row of the epoch after seq.
	drain := func() error {
		if epoch == 0 {
			return nil
		}
		for {
			items, more, err := s.Store.ReadJournal(r.Context(), id, epoch, seq, store.MaxJournalLimit)
			if err != nil {
				return err
			}
			for _, it := range items {
				seq = it.Seq
				ev, data := narration(it)
				if ev == "text" {
					if wrote {
						data = map[string]string{"text": "\n\n" + data.(map[string]string)["text"]}
					}
					wrote = true
				}
				if it.Seq <= after {
					continue
				}
				if ev != "" {
					add(ev, it.Cursor, data)
				}
				add("item", it.Cursor, it)
			}
			if err := flush(); err != nil || !more {
				return err
			}
		}
	}
	// follow moves to the run's latest attempt. One in the timeline the
	// stream is on continues it, and the client keeps what it has; one in
	// another starts that timeline from its first row, and the client
	// clears. It waits (ok false) while a new attempt has not yet decided
	// whether it continues the last one.
	follow := func(p *store.RunProgress) (ok bool, err error) {
		chain, settled, err := s.Store.AttemptChain(r.Context(), id, p.Attempt)
		if err != nil || !settled {
			return false, err
		}
		attempt, status, continues = p.Attempt, p.Status, 0
		if len(chain) > 1 {
			continues = chain[len(chain)-2]
		}
		e := 0
		if len(chain) > 0 {
			e = chain[0]
		}
		if started && e == epoch {
			sendStatus()
			sendAgentStatus(p, true)
			return true, nil
		}
		after = 0
		if !started && resuming && e > 0 {
			jh, err := s.Store.GetJournalHead(r.Context(), id, e)
			if err != nil {
				return false, err
			}
			if a, reset := journal.Resume(resume, e, jh.Last); reset == "" {
				after = a
			}
		}
		started, epoch, seq, wrote = true, e, 0, false
		sendStatus()
		sendAgentStatus(p, true)
		return true, nil
	}
	finish := func() {
		run, err := s.Store.ThreadRunByID(r.Context(), id)
		if err == nil {
			err = s.withRunChanges(r.Context(), []store.ThreadItem{{Run: run}})
		}
		if err != nil {
			s.Log.Warn("stream done", "run", id, "err", err)
			return
		}
		add("done", "", run)
		_ = flush()
	}
	// step reads the run once and sends what changed; end is true when the
	// stream is over.
	step := func(p *store.RunProgress) (end bool) {
		if !started || p.Attempt != attempt {
			ok, err := follow(p)
			if err != nil {
				return true
			}
			if !ok {
				return flush() != nil
			}
		}
		// The journal before status: a run's rows are all written before
		// its final state (an attempt's closing turn in the same
		// transaction), so once the status read says final, this drain
		// has everything. A sync first catches up rows an older instance
		// left unprojected.
		if err := s.Store.SyncJournal(r.Context(), id); err != nil {
			if r.Context().Err() == nil {
				s.Log.Warn("stream journal sync", "run", id, "err", err)
			}
			return true
		}
		if drain() != nil {
			return true
		}
		if p.Status != status {
			status = p.Status
			sendStatus()
			sendAgentStatus(p, true)
		} else {
			sendAgentStatus(p, false)
		}
		if flush() != nil {
			return true
		}
		if store.Terminal(status) {
			finish()
			return true
		}
		return false
	}

	if step(p) {
		return
	}
	poll := time.NewTicker(orDefault(s.StreamPoll, streamPoll))
	defer poll.Stop()
	ping := time.NewTicker(orDefault(s.StreamPing, streamPing))
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.Stop:
			// orbit is shutting down; the client reconnects to the next
			// process and the replay loses nothing.
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-poll.C:
			p, err := s.Store.GetRunProgress(r.Context(), id)
			if err != nil {
				if r.Context().Err() == nil {
					s.Log.Warn("stream poll", "run", id, "err", err)
				}
				return
			}
			if step(p) {
				return
			}
		}
	}
}

// narration is the prose-only view of a journal row, for clients that show
// a run as text and tool lines: a revision 1 of the agent's prose is a
// text event, of a tool call a tool event; anything else is none.
func narration(it journal.Item) (event string, data any) {
	if it.Revision != 1 {
		return "", nil
	}
	switch it.Kind {
	case journal.KindMessage:
		var m journal.Message
		if json.Unmarshal(it.Body, &m) == nil && m.Role == "assistant" {
			return "text", map[string]string{"text": m.Text}
		}
	case journal.KindToolCall:
		var c journal.ToolCall
		if json.Unmarshal(it.Body, &c) == nil {
			return "tool", map[string]string{"name": c.Name, "summary": c.Summary}
		}
	}
	return "", nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
