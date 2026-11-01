package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/kensac/orbit/internal/runner"
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

// eventPage bounds one events read; a stream loops until it is caught up.
const eventPage = 1000

// streamRun serves GET /runs/{id}/stream: Server-Sent Events replaying the
// run's latest attempt from its start, then following it live until the
// run is final.
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

	// send writes one event. id is set on text and tool events only, so a
	// client's Last-Event-ID is always the last narration it received.
	send := func(event, id string, data any) error {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if id != "" {
			if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		return rc.Flush()
	}
	sendStatus := func(p *store.RunProgress) error {
		return send("status", "", map[string]any{"status": p.Status, "attempt": p.Attempt})
	}
	// A reconnecting client names the last narration it has; within the
	// same attempt, everything up to it is skipped. Skipped narration is
	// still read, so chunk separators come out exactly as in a full replay.
	resume, resuming := parseEventID(r.Header.Get("Last-Event-ID"))

	attempt, seq, status := p.Attempt, 0, p.Status
	// wrote is whether this attempt has sent text yet: each text event is a
	// chunk to append, and a new block of prose after an earlier one starts
	// with a blank line, so the concatenation reads as paragraphs.
	wrote := false
	// drain sends every event of the current attempt after seq.
	drain := func(kind string) error {
		if attempt == 0 {
			return nil
		}
		for {
			evs, err := s.Store.ListEvents(r.Context(), id, attempt, seq, eventPage)
			if err != nil {
				return err
			}
			for _, ev := range evs {
				seq = ev.Seq
				for i, n := range runner.Narrate(kind, ev.Kind, ev.Payload) {
					at := eventID{attempt, ev.Seq, i}
					skip := resuming && resume.attempt == attempt && !resume.less(at)
					if n.Tool != nil {
						if !skip {
							err = send("tool", at.String(), n.Tool)
						}
					} else {
						chunk := n.Text
						if wrote {
							chunk = "\n\n" + chunk
						}
						wrote = true
						if !skip {
							err = send("text", at.String(), map[string]string{"text": chunk})
						}
					}
					if err != nil {
						return err
					}
				}
			}
			if len(evs) < eventPage {
				return nil
			}
		}
	}
	finish := func() {
		run, err := s.Store.ThreadRunByID(r.Context(), id)
		if err != nil {
			s.Log.Warn("stream done", "run", id, "err", err)
			return
		}
		_ = send("done", "", run)
	}

	if sendStatus(p) != nil || drain(p.RunnerKind) != nil {
		return
	}
	if store.Terminal(status) {
		finish()
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
			if p.Attempt != attempt {
				// A new claim: the earlier attempt's narration is
				// abandoned, and this one is streamed from its start. The
				// status event's attempt tells the client to start over.
				attempt, seq, status, wrote = p.Attempt, 0, p.Status, false
				if sendStatus(p) != nil {
					return
				}
			}
			// Events before status: a run's events are all written before
			// its final state, so once the status read says final, this
			// drain has everything.
			if drain(p.RunnerKind) != nil {
				return
			}
			if p.Status != status {
				status = p.Status
				if sendStatus(p) != nil {
					return
				}
			}
			if store.Terminal(status) {
				finish()
				return
			}
		}
	}
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// eventID names one narration: the attempt, the transcript event's seq,
// and its index among that event's narrations. Sent as "attempt.seq.index".
type eventID struct{ attempt, seq, index int }

func (e eventID) String() string { return fmt.Sprintf("%d.%d.%d", e.attempt, e.seq, e.index) }

func (e eventID) less(o eventID) bool {
	if e.seq != o.seq {
		return e.seq < o.seq
	}
	return e.index < o.index
}

// parseEventID reads a Last-Event-ID; anything else means a full replay.
func parseEventID(s string) (eventID, bool) {
	var e eventID
	if _, err := fmt.Sscanf(s, "%d.%d.%d", &e.attempt, &e.seq, &e.index); err != nil || e.attempt < 1 {
		return eventID{}, false
	}
	return e, true
}
