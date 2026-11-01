// Package api is orbit's HTTP surface: a JSON API under /api/v1 that a UI
// can be built on, the two pages notifications link to (a run, an
// approval), /healthz and /metrics. It listens on the tailnet only; the API
// also requires a bearer token, and the approval page accepts a per-action
// signature instead, so a link opened from a phone works without a login.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/store"
)

// Server serves the API.
type Server struct {
	Store  *store.Store
	Engine *engine.Engine
	Token  string
	Log    *slog.Logger
	// Wake nudges the executor after a manual run or message is enqueued.
	Wake func()
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /metrics", promhttp.Handler())

	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }
	api("GET /api/v1/agents", s.listAgents)
	api("GET /api/v1/agents/{name}", s.getAgent)
	api("GET /api/v1/agents/{name}/memory", s.memoryHistory)
	api("POST /api/v1/agents/{name}/runs", s.startRun)
	api("GET /api/v1/agents/{name}/messages", s.listMessages)
	api("POST /api/v1/agents/{name}/messages", s.sendMessage)
	api("GET /api/v1/runs", s.listRuns)
	api("GET /api/v1/runs/{id}", s.getRun)
	api("POST /api/v1/runs/{id}/cancel", s.cancelRun)
	api("GET /api/v1/runs/{id}/events", s.listEvents)
	api("GET /api/v1/runs/{id}/artifacts", s.listArtifacts)
	api("GET /api/v1/runs/{id}/deliveries", s.listDeliveries)
	api("GET /api/v1/artifacts/{id}", s.getArtifact)
	api("GET /api/v1/actions", s.listActions)
	api("GET /api/v1/actions/{id}", s.getAction)
	api("POST /api/v1/actions/{id}/approve", s.decide(true))
	api("POST /api/v1/actions/{id}/reject", s.decide(false))
	api("POST /api/v1/tickets", s.createTicket)
	api("GET /api/v1/tickets", s.listTickets)
	api("GET /api/v1/tickets/{id}", s.getTicket)
	api("POST /api/v1/tickets/{id}/messages", s.ticketMessage)
	api("POST /api/v1/tickets/{id}/plans/{version}/approve", s.approvePlan)
	api("POST /api/v1/tickets/{id}/abandon", s.ticketMove(store.TicketAbandoned))
	api("POST /api/v1/tickets/{id}/retry", s.ticketMove(store.TicketApproved))
	api("POST /api/v1/tickets/{id}/reopen", s.ticketMove(store.TicketOpen))

	mux.HandleFunc("GET /ui/actions/{id}", s.approvalPage)
	mux.HandleFunc("POST /ui/actions/{id}", s.approvalSubmit)
	mux.HandleFunc("GET /ui/runs/{id}", s.runPage)
	return s.logRequests(mux)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Pool.Ping(r.Context()); err != nil {
		http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		next(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path != "/healthz" && r.URL.Path != "/metrics" {
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
			respond(w, map[string]any{"agent": a, "memory": mem}, err)
			return
		}
	}
	respond(w, nil, store.ErrNotFound)
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

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Body    string  `json:"body"`
		ReplyTo *string `json:"reply_to"`
		Author  string  `json:"author"`
		// Wake overrides the agent's wake_on_message for this message.
		Wake *bool `json:"wake"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		writeErr(w, http.StatusBadRequest, "body is required")
		return
	}
	a, ok := s.Engine.Current().Config.Agents[r.PathValue("name")]
	if !ok {
		respond(w, nil, store.ErrNotFound)
		return
	}
	id, err := s.Store.SendToAgent(r.Context(), a.Name, body.Body, body.Author, body.ReplyTo)
	if err != nil {
		respond(w, nil, err)
		return
	}
	resp := map[string]any{"message_id": id}
	wake := a.MessagesTrigger
	if body.Wake != nil {
		wake = *body.Wake
	}
	if wake {
		runID, err := s.Store.EnqueueRun(r.Context(), store.NewRun{Agent: a.Name, SpecSHA: a.SHA, Trigger: "message"})
		if err != nil {
			respond(w, nil, err)
			return
		}
		resp["run_id"] = runID
		s.Wake()
	}
	respondStatus(w, http.StatusCreated, resp, nil)
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
	respond(w, map[string]any{"run": run}, err)
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
	a, err := s.Store.ListActions(r.Context(), r.URL.Query().Get("agent"), r.URL.Query().Get("status"), intParam(r, "limit"))
	respond(w, map[string]any{"actions": orEmpty(a)}, err)
}

func (s *Server) getAction(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.GetAction(r.Context(), r.PathValue("id"))
	respond(w, map[string]any{"action": a}, err)
}

func (s *Server) decide(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			By string `json:"by"`
		}
		if r.ContentLength != 0 {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
		}
		if body.By == "" {
			body.By = "api"
		}
		err := s.Store.DecideAction(r.Context(), r.PathValue("id"), approve, body.By)
		if err == nil {
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

// orEmpty makes a nil slice encode as [] rather than null.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
