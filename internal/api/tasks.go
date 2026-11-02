package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Delegated tasks (docs/api.md, Delegation): a conversation's tasks, one
// task, a person's cancel, and a live stream of a conversation's tasks.

func (s *Server) conversationTasks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusBadRequest, "not a valid id")
		return
	}
	if _, err := s.Store.GetConversation(r.Context(), id); err != nil {
		respond(w, nil, err)
		return
	}
	tasks, err := s.Store.ConversationTasks(r.Context(), id, "")
	respond(w, map[string]any{"tasks": orEmpty(tasks)}, err)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusBadRequest, "not a valid id")
		return
	}
	t, err := s.Store.GetTask(r.Context(), id)
	respond(w, map[string]any{"task": t}, err)
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusBadRequest, "not a valid id")
		return
	}
	var body struct {
		Reason string `json:"reason"`
		By     string `json:"by"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := s.Store.CancelTask(r.Context(), id, "", strings.TrimSpace(body.Reason), decider(r, body.By)); err != nil {
		respond(w, nil, err)
		return
	}
	if err := s.Engine.ReconcileTasks(r.Context()); err != nil {
		s.Log.Warn("delegation: reconcile after cancel", "err", err)
	}
	if s.Wake != nil {
		s.Wake()
	}
	respond(w, map[string]any{"cancelled": true}, nil)
}

// taskStreamPoll is how often a task stream reads its conversation's
// tasks: they change at run boundaries, so a second is live enough.
const taskStreamPoll = time.Second

// streamTasks serves GET /conversations/{id}/tasks/stream: a tasks event
// with every task the conversation delegated, sent on connect and again
// whenever any of them changes, until the client leaves. It reads only
// rows, as a run's stream does.
func (s *Server) streamTasks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusBadRequest, "not a valid id")
		return
	}
	if _, err := s.Store.GetConversation(r.Context(), id); err != nil {
		respond(w, nil, err)
		return
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

	var last []byte
	sendIfChanged := func() error {
		tasks, err := s.Store.ConversationTasks(r.Context(), id, "")
		if err != nil {
			return err
		}
		b, err := json.Marshal(map[string]any{"tasks": orEmpty(tasks)})
		if err != nil || bytes.Equal(b, last) {
			return err
		}
		last = b
		if _, err := fmt.Fprintf(w, "event: tasks\ndata: %s\n\n", b); err != nil {
			return err
		}
		return rc.Flush()
	}
	if err := sendIfChanged(); err != nil {
		return
	}
	poll := time.NewTicker(orDefault(s.StreamPoll, taskStreamPoll))
	defer poll.Stop()
	ping := time.NewTicker(orDefault(s.StreamPing, streamPing))
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.Stop:
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-poll.C:
			if err := sendIfChanged(); err != nil {
				return
			}
		}
	}
}
