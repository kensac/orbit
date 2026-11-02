package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/kensac/orbit/internal/store"
)

// The board (docs/api.md, The board): every open conversation's state at a
// glance, the person's read points, and one change feed for a client that
// shows them all.

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	idle := s.Engine.Current().Config.Deployment.BoardIdle()
	if idle <= 0 {
		idle = store.DefaultBoardIdle
	}
	b, err := s.Store.ReadBoard(r.Context(), idle, queryFlag(r, "idle"), limit)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"rows": b.Rows, "as_of": b.AsOf, "idle_after_seconds": int64(idle / time.Second)}, nil)
}

// queryFlag is a boolean query parameter: 1 or true.
func queryFlag(r *http.Request, name string) bool {
	v := r.URL.Query().Get(name)
	return v == "1" || v == "true"
}

func (s *Server) markRead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !store.IsUUID(id) {
		writeErr(w, http.StatusBadRequest, "not a valid id")
		return
	}
	var body struct {
		Through *time.Time `json:"through"`
	}
	if !decode(w, r, 4<<10, &body, bodyOptional) {
		return
	}
	at, err := s.Store.MarkRead(r.Context(), id, body.Through)
	respond(w, map[string]any{"conversation_id": id, "read_through": at}, err)
}

// serveThread serves a conversation's thread page with the read point it
// had before this request (read_through), and with mark_read=1 moves the
// read point to when the page was read.
func (s *Server) serveThread(w http.ResponseWriter, r *http.Request, id string) {
	if !store.IsUUID(id) {
		writeErr(w, http.StatusBadRequest, "not a valid id")
		return
	}
	before, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	prev, now, err := s.Store.ReadPoint(r.Context(), id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	page, err := s.Store.ConversationThread(r.Context(), id, before, limit)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if queryFlag(r, "mark_read") {
		if _, err := s.Store.MarkRead(r.Context(), id, &now); err != nil {
			respond(w, nil, err)
			return
		}
	}
	resp := map[string]any{"items": orEmpty(page.Items), "has_more": page.HasMore, "next_before": nil, "read_through": prev}
	if page.HasMore {
		resp["next_before"] = page.NextBefore
	}
	respond(w, resp, nil)
}

// eventsPoll is how often the change feed reads new records: a sidebar
// that moves within a second reads as live, and each open feed costs one
// indexed read per interval.
const eventsPoll = time.Second

// eventsBatch is the most records one read of the feed returns; a backlog
// is read in several.
const eventsBatch = 500

// eventsReplayMax is the most records a resume replays. A client further
// behind is sent ready with resumed false and starts from the head: it
// reads the board again rather than every change it missed.
const eventsReplayMax = 1000

// events serves GET /events: Server-Sent Events of the change feed. A ready
// event first, then a change event per record, each with its cursor as the
// id, and a ping every 15 s. It reads only rows, as a run's stream does,
// so a reconnect to another instance resumes from its Last-Event-ID.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	from := r.Header.Get("Last-Event-ID")
	if from == "" {
		from = r.URL.Query().Get("cursor")
	}
	var cur store.ChangeCursor
	resumed := false
	if from != "" {
		if c, err := store.ParseChangeCursor(from); err == nil {
			n, err := s.Store.ChangesBehind(ctx, c, eventsReplayMax)
			if err != nil {
				respond(w, nil, err)
				return
			}
			cur, resumed = c, n <= eventsReplayMax
		}
	}
	if !resumed {
		head, err := s.Store.ChangesHead(ctx)
		if err != nil {
			respond(w, nil, err)
			return
		}
		cur = head
	}
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported: "+err.Error())
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event, id string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "event: %s\nid: %s\ndata: %s\n\n", event, id, b)
		_, err = w.Write(buf.Bytes())
		return err
	}
	if err := send("ready", cur.String(), map[string]any{"cursor": cur.String(), "resumed": resumed}); err != nil || rc.Flush() != nil {
		return
	}
	drain := func() error {
		for {
			list, err := s.Store.ChangesAfter(ctx, cur, eventsBatch)
			if err != nil {
				return err
			}
			for _, c := range list {
				if err := send("change", c.Cursor, c); err != nil {
					return err
				}
				if cur, err = store.ParseChangeCursor(c.Cursor); err != nil {
					return err
				}
			}
			if len(list) > 0 {
				if err := rc.Flush(); err != nil {
					return err
				}
			}
			if len(list) < eventsBatch {
				return nil
			}
		}
	}
	if drain() != nil {
		return
	}
	poll := time.NewTicker(orDefault(s.StreamPoll, eventsPoll))
	defer poll.Stop()
	ping := time.NewTicker(orDefault(s.StreamPing, streamPing))
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Stop:
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-poll.C:
			if drain() != nil {
				return
			}
		}
	}
}
