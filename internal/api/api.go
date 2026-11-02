// Package api is orbit's HTTP surface: a JSON API under /api/v1 that a UI
// can be built on, the two pages notifications link to (a run, an
// approval), /healthz and /metrics.
//
// It serves two listeners. The public one (Handler) is what traefik routes
// to: /api/v1 there accepts only verified authentik tokens, and /ui relies
// on authentik forward-auth in front of it. The internal one
// (InternalHandler) is never routed by traefik: /api/v1 there also accepts
// the operator token, for scripts on the host. The approval page accepts a
// per-action signature, so a link opened from a phone works without a login.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kensac/orbit/internal/auth"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/scheduler"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// Server serves the API.
type Server struct {
	Store  *store.Store
	Engine *engine.Engine
	// Token is the operator token, accepted on the internal listener only.
	Token string
	// Verifier checks authentik tokens; nil refuses them all.
	Verifier *auth.Verifier
	Log      *slog.Logger
	// Wake nudges the executor after a manual run or message is enqueued.
	Wake func()
	// Stop is closed when the server shuts down, ending open streams.
	Stop <-chan struct{}
	// StreamPoll and StreamPing override the stream's poll and heartbeat
	// intervals (streamPoll, streamPing) when set; tests shorten them.
	StreamPoll, StreamPing time.Duration
	// ConfigState is the configuration's load status, for GET /health.
	ConfigState *engine.ConfigState
	// Drain is this instance's drain switch: /readyz reports it, and the
	// internal listener's POST /api/v1/drain starts it. Nil is never
	// draining.
	Drain *engine.Drain
}

// Handler returns the public listener's routes, the ones traefik serves.
func (s *Server) Handler() http.Handler { return s.routes(false) }

// InternalHandler returns the internal listener's routes: the API, which
// there also accepts the operator token, /healthz and /metrics. It has no
// /ui pages, which rely on forward-auth.
func (s *Server) InternalHandler() http.Handler { return s.routes(true) }

func (s *Server) routes(internal bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /metrics", promhttp.Handler())

	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.authenticate(internal, h)) }
	api("GET /api/v1/agents", s.listAgents)
	api("GET /api/v1/agents/{name}", s.getAgent)
	api("GET /api/v1/agents/{name}/thread", s.thread)
	api("GET /api/v1/agents/{name}/conversations", s.listConversations)
	api("POST /api/v1/agents/{name}/conversations", s.createConversation)
	api("GET /api/v1/conversations/{id}", s.getConversation)
	api("PATCH /api/v1/conversations/{id}", s.updateConversation)
	api("GET /api/v1/conversations/{id}/thread", s.conversationThread)
	api("POST /api/v1/conversations/{id}/messages", s.conversationMessage)
	api("GET /api/v1/agents/{name}/memory", s.memoryHistory)
	api("POST /api/v1/agents/{name}/runs", s.startRun)
	api("GET /api/v1/agents/{name}/messages", s.listMessages)
	api("POST /api/v1/agents/{name}/messages", s.sendMessage)
	api("GET /api/v1/runs", s.listRuns)
	api("GET /api/v1/runs/{id}", s.getRun)
	api("POST /api/v1/runs/{id}/cancel", s.cancelRun)
	api("GET /api/v1/runs/{id}/events", s.listEvents)
	api("GET /api/v1/runs/{id}/stream", s.streamRun)
	api("GET /api/v1/runs/{id}/artifacts", s.listArtifacts)
	api("GET /api/v1/runs/{id}/deliveries", s.listDeliveries)
	api("GET /api/v1/artifacts/{id}", s.getArtifact)
	api("GET /api/v1/usage", s.usage)
	api("GET /api/v1/health", s.health)
	api("GET /api/v1/actions", s.listActions)
	api("GET /api/v1/actions/{id}", s.getAction)
	api("POST /api/v1/actions/{id}/approve", s.decide(true))
	api("POST /api/v1/actions/{id}/reject", s.decide(false))
	api("POST /api/v1/tickets", s.createTicket)
	api("GET /api/v1/tickets", s.listTickets)
	api("GET /api/v1/tickets/{id}", s.getTicket)
	api("POST /api/v1/tickets/{id}/plans/{version}/approve", s.approvePlan)
	api("POST /api/v1/tickets/{id}/abandon", s.ticketMove(store.TicketAbandoned))
	api("POST /api/v1/tickets/{id}/retry", s.ticketMove(store.TicketApproved))
	api("POST /api/v1/tickets/{id}/reopen", s.ticketMove(store.TicketOpen))

	if internal {
		// Only the host's rollout reaches this listener; a person cannot
		// take an instance out of service from the public one.
		api("POST /api/v1/drain", s.drain)
	} else {
		mux.HandleFunc("GET /ui/actions/{id}", s.approvalPage)
		mux.HandleFunc("POST /ui/actions/{id}", s.approvalSubmit)
		mux.HandleFunc("GET /ui/runs/{id}", s.runPage)
	}
	return s.logRequests(mux)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Pool.Ping(r.Context()); err != nil {
		http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// readyz is readiness, not liveness: 200 only while this instance takes
// new work, so the proxy routes to it; 503 while it drains (it still serves
// whoever is connected) or cannot reach the database. traefik health-checks
// this; the container's own HEALTHCHECK stays on /healthz, because a
// draining instance is alive and must not be restarted.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.Drain != nil && s.Drain.Draining() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	if err := s.Store.Pool.Ping(r.Context()); err != nil {
		http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}

// drain serves POST /api/v1/drain (internal listener): start draining, as
// SIGTERM does. The rollout calls it on the old instance once the new one
// is ready, then waits for the old one to exit.
func (s *Server) drain(w http.ResponseWriter, r *http.Request) {
	if s.Drain == nil {
		writeErr(w, http.StatusNotImplemented, "this server has no drain")
		return
	}
	started := s.Drain.Start()
	if started {
		s.Log.Info("draining: requested over the internal API")
	}
	respondStatus(w, http.StatusAccepted, map[string]any{"draining": true, "started": started,
		"since": s.Drain.Since()}, nil)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && r.URL.Path != "/metrics" {
			s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the connection (a stream
// flushes and clears its write deadline through it).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ---------------------------------------------------------------- agents

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.Store.ListAgents(r.Context())
	respond(w, map[string]any{"agents": orEmpty(agents)}, err)
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agents, err := s.Store.ListAgents(r.Context())
	if err != nil {
		respond(w, nil, err)
		return
	}
	for _, a := range agents {
		if a.Name == name {
			mem, err := s.Store.Memory(r.Context(), name)
			next := scheduler.NextFires(s.Engine.Current().Config.Agents[name], time.Now())
			respond(w, map[string]any{"agent": a, "memory": mem, "next_fires": next}, err)
			return
		}
	}
	respond(w, nil, store.ErrNotFound)
}

// thread serves an agent's default conversation, a page at a time
// (store.Thread).
func (s *Server) thread(w http.ResponseWriter, r *http.Request) {
	servePage(w, r, func(before *store.ThreadCursor, limit int) (*store.ThreadPage, error) {
		return s.Store.Thread(r.Context(), r.PathValue("name"), before, limit)
	})
}

// servePage is the one way a thread page is served, whichever conversation:
// the before cursor and limit in, items, has_more and next_before out.
func servePage(w http.ResponseWriter, r *http.Request, fetch func(before *store.ThreadCursor, limit int) (*store.ThreadPage, error)) {
	var before *store.ThreadCursor
	if b := r.URL.Query().Get("before"); b != "" {
		c, err := store.ParseThreadCursor(b)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "before: "+err.Error())
			return
		}
		before = c
	}
	page, err := fetch(before, intParam(r, "limit"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	resp := map[string]any{"items": orEmpty(page.Items), "has_more": page.HasMore, "next_before": nil}
	if page.HasMore {
		resp["next_before"] = page.NextBefore
	}
	respond(w, resp, nil)
}

func (s *Server) memoryHistory(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.MemoryHistory(r.Context(), r.PathValue("name"), intParam(r, "limit"))
	respond(w, map[string]any{"versions": orEmpty(v)}, err)
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt string `json:"prompt"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	a, ok := s.Engine.Current().Config.Agents[r.PathValue("name")]
	if !ok {
		respond(w, nil, store.ErrNotFound)
		return
	}
	id, err := s.Store.EnqueueRun(r.Context(), store.NewRun{Agent: a.Name, SpecSHA: a.SHA, Trigger: "manual", Prompt: body.Prompt})
	if err == nil {
		s.Wake()
	}
	respondStatus(w, http.StatusAccepted, map[string]any{"run_id": id}, err)
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.ListMessages(r.Context(), r.PathValue("name"), intParam(r, "limit"))
	respond(w, map[string]any{"messages": orEmpty(m)}, err)
}

// sendMessage posts to an agent's default conversation.
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	a, ok := s.Engine.Current().Config.Agents[r.PathValue("name")]
	if !ok {
		respond(w, nil, store.ErrNotFound)
		return
	}
	s.postTo(w, r, a, "")
}

// postTo posts a person's message into one of agent a's conversations
// ("" is the default) and wakes a run there when wake says so.
func (s *Server) postTo(w http.ResponseWriter, r *http.Request, a *spec.Agent, conversationID string) {
	var body struct {
		Body    string  `json:"body"`
		ReplyTo *string `json:"reply_to"`
		// Author is used with the operator token only; a person's message
		// is always authored by the token's person.
		Author string `json:"author"`
		// Wake overrides the agent's wake_on_message for this message.
		Wake *bool `json:"wake"`
		// ClientMessageID makes a retry return the first result.
		ClientMessageID string `json:"client_message_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		writeErr(w, http.StatusBadRequest, "body is required")
		return
	}
	wake := a.MessagesTrigger
	if body.Wake != nil {
		wake = *body.Wake
	}
	res, replayed, err := s.Store.PostMessageOnce(r.Context(), store.PostMessage{
		Agent: a.Name, SpecSHA: a.SHA, Body: body.Body, Author: actor(r, body.Author), ReplyTo: body.ReplyTo,
		Wake: wake, ClientID: body.ClientMessageID, ConversationID: conversationID,
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	if res.RunID != "" && !replayed {
		s.Wake()
	}
	resp := map[string]any{"message_id": res.MessageID}
	if res.RunID != "" {
		resp["run_id"] = res.RunID
	}
	respondStatus(w, http.StatusCreated, resp, nil)
}

// actor is who a write is for: the token's person; with the operator token,
// whoever the body names (possibly no one).
func actor(r *http.Request, named string) string {
	if p := principalOf(r); p.Person != "" {
		return p.Person
	}
	return named
}

// ---------------------------------------------------------------- runs

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	f := store.RunFilter{Agent: r.URL.Query().Get("agent"), Status: r.URL.Query().Get("status"), Limit: intParam(r, "limit")}
	if b := r.URL.Query().Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339Nano, b)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "before: "+err.Error())
			return
		}
		f.Before = &t
	}
	runs, err := s.Store.ListRuns(r.Context(), f)
	respond(w, map[string]any{"runs": orEmpty(runs)}, err)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	attempts, err := s.Store.ListAttempts(r.Context(), run.ID)
	respond(w, map[string]any{"run": run, "attempts": orEmpty(attempts)}, err)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	err := s.Store.CancelRun(r.Context(), r.PathValue("id"))
	respond(w, map[string]any{"cancelled": err == nil}, err)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	ev, err := s.Store.ListEvents(r.Context(), r.PathValue("id"), intParam(r, "attempt"), intParam(r, "after"), intParam(r, "limit"))
	respond(w, map[string]any{"events": orEmpty(ev)}, err)
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.ListArtifacts(r.Context(), r.PathValue("id"))
	respond(w, map[string]any{"artifacts": orEmpty(a)}, err)
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.ListDeliveries(r.Context(), r.PathValue("id"))
	respond(w, map[string]any{"deliveries": orEmpty(d)}, err)
}

func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	b, mt, err := s.Store.GetArtifactContent(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", mt)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	_, _ = w.Write(b)
}

// ---------------------------------------------------------------- actions

func (s *Server) listActions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ActionFilter{Agent: q.Get("agent"), Status: q.Get("status"), RunID: q.Get("run_id"), Limit: intParam(r, "limit")}
	if f.RunID != "" && !isUUID(f.RunID) {
		writeErr(w, http.StatusBadRequest, "run_id: not a run id")
		return
	}
	if b := q.Get("before"); b != "" {
		c, err := store.ParseConversationCursor(b)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "before: "+err.Error())
			return
		}
		f.Before = c
	}
	page, err := s.Store.ListActionsPage(r.Context(), f)
	if err != nil {
		respond(w, nil, err)
		return
	}
	resp := map[string]any{"actions": orEmpty(page.Actions), "has_more": page.HasMore, "next_before": nil}
	if page.HasMore {
		resp["next_before"] = page.NextBefore
	}
	respond(w, resp, nil)
}

func (s *Server) getAction(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.GetAction(r.Context(), r.PathValue("id"))
	respond(w, map[string]any{"action": a}, err)
}

func (s *Server) decide(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			By              string `json:"by"`
			ClientRequestID string `json:"client_request_id"`
		}
		if r.ContentLength != 0 {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
		}
		// A person decides as themselves; by is for the operator token.
		if p := principalOf(r); p.Person != "" {
			body.By = p.Person
		} else if body.By == "" {
			body.By = "operator"
		}
		replayed, err := s.Store.DecideActionOnce(r.Context(), r.PathValue("id"), approve, body.By, body.ClientRequestID)
		if err == nil && !replayed {
			s.Wake()
		}
		respond(w, map[string]any{"ok": err == nil}, err)
	}
}

// ---------------------------------------------------------------- helpers

func respond(w http.ResponseWriter, v any, err error) { respondStatus(w, http.StatusOK, v, err) }

func respondStatus(w http.ResponseWriter, status int, v any, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case store.IsBadID(err):
		writeErr(w, http.StatusBadRequest, "not a valid id")
	case errors.Is(err, store.ErrReplyElsewhere), errors.Is(err, store.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrReceiptMismatch):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "no longer in a state that allows this")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func intParam(r *http.Request, name string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(name))
	return n
}

// isUUID reports whether s is a UUID in its canonical text form, so an id
// that cannot name a row is a 400, not a Postgres cast error.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f', 'A' <= c && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// orEmpty makes a nil slice encode as [] rather than null.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
