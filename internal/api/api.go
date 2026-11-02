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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kensac/orbit/internal/auth"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/push"
	"github.com/kensac/orbit/internal/sandbox"
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
	// Push sends Web Push and holds its key; nil is push off (the inbox
	// still works).
	Push *push.Sender
	// OAuth, when set, serves the authorization server MCP clients sign in through (oauth.go) on the public listener. Without it /mcp still takes an authentik token from a client that has one.
	OAuth *OAuthProxy
	// Boxes returns sandboxd's client for agents' boxes (terminals.go);
	// nil is no boxes, and every workspace has no terminals.
	Boxes func() (*sandbox.Client, error)
	// WorkspaceOrigins are the Origins a terminal socket accepts
	// (ORBIT_WORKSPACE_ORIGINS); empty is DefaultWorkspaceOrigins.
	WorkspaceOrigins []string
	// TerminalPing overrides terminalPing when set; tests shorten it.
	TerminalPing time.Duration
	// WorkspaceGroup is ORBIT_WORKSPACE_GROUP: the group a person must be
	// in to change a workspace. Empty is ORBIT_OIDC_GROUP's, which every
	// verified person is in.
	WorkspaceGroup string
	// WorkspaceReadRate and WorkspaceWriteRate override the workspace's
	// per-person rates (readRate, writeRate) when set; tests raise them.
	WorkspaceReadRate, WorkspaceWriteRate float64

	termMu      sync.Mutex
	termClients map[string]int
	wsOnce      sync.Once
	ws          *workspaceState
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
	api("POST /api/v1/agents", s.createAgent)
	api("PUT /api/v1/agents/{name}", s.updateAgent)
	api("POST /api/v1/agents/{name}/enable", s.setAgentState("enable"))
	api("POST /api/v1/agents/{name}/disable", s.setAgentState("disable"))
	api("POST /api/v1/agents/{name}/archive", s.setAgentState("archive"))
	api("GET /api/v1/agents/{name}/definitions", s.agentDefinitions)
	api("POST /api/v1/agents/{name}/definitions/{version}/revert", s.revertAgent)
	api("GET /api/v1/agents/{name}/thread", s.thread)
	api("GET /api/v1/agents/{name}/conversations", s.listConversations)
	api("POST /api/v1/agents/{name}/conversations", s.createConversation)
	api("POST /api/v1/conversations", s.createConversation)
	api("GET /api/v1/conversations/{id}", s.getConversation)
	api("PATCH /api/v1/conversations/{id}", s.updateConversation)
	api("GET /api/v1/conversations/{id}/thread", s.conversationThread)
	api("POST /api/v1/conversations/{id}/messages", s.conversationMessage)
	api("GET /api/v1/conversations/{id}/tasks", s.conversationTasks)
	api("GET /api/v1/conversations/{id}/tasks/stream", s.streamTasks)
	api("GET /api/v1/tasks/{id}", s.getTask)
	api("POST /api/v1/tasks/{id}/cancel", s.cancelTask)
	api("POST /api/v1/conversations/{id}/uploads", s.createUpload)
	api("POST /api/v1/conversations/{id}/uploads/url", s.uploadURL)
	api("GET /api/v1/conversations/{id}/uploads/{upload}", s.getUpload)
	api("GET /api/v1/conversations/{id}/files", s.workspaceFiles)
	api("GET /api/v1/conversations/{id}/files/{path...}", s.workspaceFile)
	// The signature in the URL is the credential (uploads.go).
	mux.HandleFunc("PUT /api/v1/uploads/signed", s.signedUpload)
	api("GET /api/v1/agents/{name}/memory", s.memoryHistory)
	api("POST /api/v1/agents/{name}/runs", s.startRun)
	api("GET /api/v1/agents/{name}/messages", s.listMessages)
	api("POST /api/v1/agents/{name}/messages", s.sendMessage)
	api("GET /api/v1/runs", s.listRuns)
	api("GET /api/v1/runs/{id}", s.getRun)
	api("POST /api/v1/runs/{id}/cancel", s.cancelRun)
	api("GET /api/v1/runs/{id}/events", s.listEvents)
	api("GET /api/v1/runs/{id}/stream", s.streamRun)
	api("GET /api/v1/runs/{id}/journal", s.readJournal)
	api("GET /api/v1/runs/{id}/journal/items/{item}", s.journalItem)
	api("GET /api/v1/runs/{id}/journal/bodies/{sha256}", s.journalBody)
	api("GET /api/v1/runs/{id}/artifacts", s.listArtifacts)
	api("GET /api/v1/runs/{id}/deliveries", s.listDeliveries)
	api("GET /api/v1/artifacts/{id}", s.getArtifact)
	api("GET /api/v1/search", s.searchHTTP)
	api("GET /api/v1/usage", s.usage)
	api("GET /api/v1/health", s.health)
	api("GET /api/v1/actions", s.listActions)
	api("GET /api/v1/actions/{id}", s.getAction)
	api("POST /api/v1/actions/{id}/approve", s.decide(true))
	api("POST /api/v1/actions/{id}/reject", s.decide(false))
	api("GET /api/v1/attention", s.listAttention)
	api("POST /api/v1/attention/{id}/dismiss", s.dismissAttention)
	api("GET /api/v1/push", s.pushInfo)
	api("GET /api/v1/push/subscriptions", s.listPushSubscriptions)
	api("POST /api/v1/push/subscriptions", s.savePushSubscription)
	api("DELETE /api/v1/push/subscriptions", s.deletePushSubscription)
	api("POST /api/v1/push/test", s.testPush)
	api("POST /api/v1/tickets", s.createTicket)
	api("GET /api/v1/tickets", s.listTickets)
	api("GET /api/v1/tickets/{id}", s.getTicket)
	api("POST /api/v1/tickets/{id}/plans/{version}/approve", s.approvePlan)
	api("POST /api/v1/tickets/{id}/abandon", s.ticketMove(store.TicketAbandoned))
	api("POST /api/v1/tickets/{id}/retry", s.ticketMove(store.TicketApproved))
	api("POST /api/v1/tickets/{id}/reopen", s.ticketMove(store.TicketOpen))
	api("POST /api/v1/tickets/{id}/checks/rerun", s.rerunChecks)
	api("GET /api/v1/tickets/{id}/comments", s.listComments)
	api("POST /api/v1/tickets/{id}/comments", s.addComment)
	api("POST /api/v1/tickets/{id}/comments/{comment}/edit", s.editComment)
	api("POST /api/v1/tickets/{id}/comments/{comment}/withdraw", s.withdrawComment)
	api("POST /api/v1/tickets/{id}/review", s.submitReview)
	api("GET /api/v1/agents/{name}/workspace", s.agentWorkspace)
	api("POST /api/v1/workspaces/{id}/terminals", s.createTerminal)
	api("GET /api/v1/workspaces/{id}/terminals", s.listTerminals)
	api("GET /api/v1/terminals/{tid}", s.getTerminal)
	api("POST /api/v1/terminals/{tid}/kill", s.killTerminal)
	api("POST /api/v1/terminals/{tid}/attach", s.attachTicket)
	// The ticket in Sec-WebSocket-Protocol is the credential (terminals.go).
	mux.HandleFunc("GET /api/v1/terminals/{tid}/ws", s.terminalSocket)
	s.memoryRoutes(api)
	s.workspaceRoutes(api)
	s.workspaceWriteRoutes(api, mux)
	s.skillsRoutes(api)

	mux.Handle("/mcp", s.mcpHandler(internal))
	// The signature is the credential (hooks.go); traefik routes /hooks/ past forward-auth.
	mux.HandleFunc("POST /hooks/{agent}/{trigger}", s.hook)

	if internal {
		// Only the host's rollout reaches this listener; a person cannot
		// take an instance out of service from the public one.
		api("POST /api/v1/drain", s.drain)
	} else {
		mux.HandleFunc("GET /ui/actions/{id}", s.approvalPage)
		mux.HandleFunc("POST /ui/actions/{id}", s.approvalSubmit)
		mux.HandleFunc("GET /ui/runs/{id}", s.runPage)
		if s.OAuth != nil {
			s.oauthRoutes(mux)
		}
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
	s.withOrigin(agents)
	respond(w, map[string]any{"agents": orEmpty(agents)}, err)
}

// withOrigin fills each row's origin and version from the configuration in
// force.
func (s *Server) withOrigin(rows []store.AgentRow) {
	cfg := s.Engine.Current().Config
	for i := range rows {
		if a, ok := cfg.Agents[rows[i].Name]; ok {
			rows[i].Origin, rows[i].Version = a.Origin, a.Version
		}
	}
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agents, err := s.Store.ListAgents(r.Context())
	if err != nil {
		respond(w, nil, err)
		return
	}
	s.withOrigin(agents)
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

// servePage is the one way a thread page is served over HTTP, whichever conversation (threadPage).
func servePage(w http.ResponseWriter, r *http.Request, fetch func(before *store.ThreadCursor, limit int) (*store.ThreadPage, error)) {
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	fold := true
	switch r.URL.Query().Get("fold") {
	case "", "true":
	case "false":
		fold = false
	default:
		writeErr(w, http.StatusBadRequest, "fold: want true or false")
		return
	}
	resp, err := threadPage(r.URL.Query().Get("before"), limit, fold, fetch)
	respond(w, resp, err)
}

// threadPage reads a thread page for HTTP and MCP alike: the before cursor and limit in, items, has_more and next_before out (listPage).
func threadPage(before string, limit int, fold bool, fetch func(before *store.ThreadCursor, limit int) (*store.ThreadPage, error)) (map[string]any, error) {
	var c *store.ThreadCursor
	if before != "" {
		var err error
		if c, err = store.ParseThreadCursor(before); err != nil {
			return nil, requestError("before: " + err.Error())
		}
	}
	page, err := fetch(c, limit)
	if err != nil {
		return nil, err
	}
	items := page.Items
	if fold {
		// limit and the cursor count every run, so paging is unchanged; a fold never crosses a page.
		items = store.FoldSkips(items)
	}
	return listPage("items", items, page.HasMore, page.NextBefore), nil
}

// listPage is a page as every paged list is served, over HTTP and MCP alike: the items under key (always an array), has_more, and next_before, null exactly when has_more is false.
func listPage[T any](key string, items []T, hasMore bool, next string) map[string]any {
	resp := map[string]any{key: orEmpty(items), "has_more": hasMore, "next_before": nil}
	if hasMore {
		resp["next_before"] = next
	}
	return resp
}

func (s *Server) memoryHistory(w http.ResponseWriter, r *http.Request) {
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	v, err := s.Store.MemoryHistory(r.Context(), r.PathValue("name"), limit)
	respond(w, map[string]any{"versions": orEmpty(v)}, err)
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt string `json:"prompt"`
	}
	if !decode(w, r, 64<<10, &body, bodyOptional) {
		return
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
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	m, err := s.Store.ListMessages(r.Context(), r.PathValue("name"), limit)
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
		// Attachments are uploads of this conversation the message carries.
		Attachments []string `json:"attachments"`
		// Answers fill the form of the question reply_to names.
		Answers map[string]json.RawMessage `json:"answers"`
		// Delivery is "queue" (the default) or "steer": into the reply
		// being written, when one is.
		Delivery string `json:"delivery"`
	}
	if !decode(w, r, 64<<10, &body, bodyRequired) {
		return
	}
	if body.Delivery != "" && body.Delivery != store.DeliveryQueue && body.Delivery != store.DeliverySteer {
		writeErr(w, http.StatusBadRequest, `delivery is "queue" or "steer"`)
		return
	}
	if strings.TrimSpace(body.Body) == "" && len(body.Attachments) == 0 && body.Answers == nil {
		writeErr(w, http.StatusBadRequest, "body, attachments or answers is required")
		return
	}
	wake := a.MessagesTrigger
	if body.Wake != nil {
		wake = *body.Wake
	}
	res, replayed, err := s.Store.PostMessageOnce(r.Context(), store.PostMessage{
		Agent: a.Name, SpecSHA: a.SHA, Body: body.Body, Author: actor(r, body.Author), ReplyTo: body.ReplyTo,
		Wake: wake, ClientID: body.ClientMessageID, ConversationID: conversationID, Attachments: body.Attachments,
		Answers: body.Answers, Delivery: body.Delivery, Steerable: a.Chat,
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	if res.RunID != "" && !replayed && res.Delivered == store.DeliveredQueued {
		s.Wake()
	}
	resp := map[string]any{"message_id": res.MessageID, "delivered": res.Delivered}
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
	f := store.RunFilter{Agent: r.URL.Query().Get("agent"), Status: r.URL.Query().Get("status")}
	if !queryInt(w, r, "limit", &f.Limit) {
		return
	}
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
	st, err := s.Store.CancelRun(r.Context(), r.PathValue("id"), actor(r, ""))
	code := http.StatusOK
	if st == "cancelling" {
		code = http.StatusAccepted
	}
	respondStatus(w, code, map[string]any{"cancelled": st == store.StatusCancelled, "status": st}, err)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	var attempt, after, limit int
	if !queryInt(w, r, "attempt", &attempt) || !queryInt(w, r, "after", &after) || !queryInt(w, r, "limit", &limit) {
		return
	}
	ev, err := s.Store.ListEvents(r.Context(), r.PathValue("id"), attempt, after, limit)
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

// getArtifact is GET /artifacts/{id}: the content, streamed from the store, with Range support.
func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	a, b, err := s.Store.OpenArtifact(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", a.MediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	http.ServeContent(w, r, "", a.CreatedAt, b)
}

// ---------------------------------------------------------------- actions

func (s *Server) listActions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ActionFilter{Agent: q.Get("agent"), Status: q.Get("status"), RunID: q.Get("run_id")}
	if !queryInt(w, r, "limit", &f.Limit) {
		return
	}
	resp, err := s.actionPage(r.Context(), f, q.Get("before"))
	respond(w, resp, err)
}

// actionPage is a page of actions for HTTP and MCP alike; before is the previous page's next_before.
func (s *Server) actionPage(ctx context.Context, f store.ActionFilter, before string) (map[string]any, error) {
	if f.RunID != "" && !store.IsUUID(f.RunID) {
		return nil, requestError("run_id: not a run id")
	}
	if before != "" {
		c, err := store.ParseConversationCursor(before)
		if err != nil {
			return nil, requestError("before: " + err.Error())
		}
		f.Before = c
	}
	page, err := s.Store.ListActionsPage(ctx, f)
	if err != nil {
		return nil, err
	}
	return listPage("actions", page.Actions, page.HasMore, page.NextBefore), nil
}

func (s *Server) getAction(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.GetAction(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	out := map[string]any{"action": a}
	if p, perr := s.Engine.PreviewAction(r.Context(), a); perr != nil {
		out["preview_error"] = perr.Error()
	} else if p != nil {
		out["preview"] = p
	}
	respond(w, out, nil)
}

func (s *Server) decide(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			By              string `json:"by"`
			ClientRequestID string `json:"client_request_id"`
			// Always also makes the action's type auto for its agent (approve only).
			Always bool `json:"always"`
		}
		if !decode(w, r, 4<<10, &body, bodyOptional) {
			return
		}
		if body.Always && !approve {
			writeErr(w, http.StatusBadRequest, "always is for approving")
			return
		}
		// A person decides as themselves; by is for the operator token.
		by := body.By
		if p := principalOf(r); p.Person != "" {
			by = p.Person
		}
		if body.Always {
			out, err := s.approveAlways(r.Context(), r.PathValue("id"), by, body.ClientRequestID)
			if err != nil {
				s.respondDefine(w, http.StatusOK, nil, err)
				return
			}
			respond(w, out, nil)
			return
		}
		err := s.decideAction(r.Context(), r.PathValue("id"), approve, by, body.ClientRequestID)
		respond(w, map[string]any{"ok": err == nil}, err)
	}
}

// decideAction records a decision for HTTP and MCP alike, as by ("" is the operator), and wakes the action loop unless it replayed an earlier one.
func (s *Server) decideAction(ctx context.Context, id string, approve bool, by, clientID string) error {
	if by == "" {
		by = "operator"
	}
	replayed, err := s.Store.DecideActionOnce(ctx, id, approve, by, clientID)
	if err == nil && !replayed {
		s.Wake()
	}
	return err
}

// approveAlways approves an action and makes its type auto for its agent: the new version is validated before the decision, so a change that cannot be made decides nothing, and stored after it. A replayed decision changes nothing again.
func (s *Server) approveAlways(ctx context.Context, id, by, clientRequestID string) (map[string]any, error) {
	act, err := s.Store.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if act.Status == "pending" {
		if _, err := s.Engine.AlwaysAllow(ctx, act, by, true); err != nil {
			return nil, err
		}
	}
	if by == "" {
		by = "operator"
	}
	replayed, err := s.Store.DecideActionOnce(ctx, id, true, by, clientRequestID)
	if err != nil {
		return nil, err
	}
	if replayed {
		return map[string]any{"ok": true}, nil
	}
	s.Wake()
	d, err := s.Engine.AlwaysAllow(ctx, act, by, false)
	if err != nil {
		return nil, fmt.Errorf("approved, but the rule was not changed: %w", err)
	}
	return map[string]any{"ok": true, "always": map[string]any{"agent": d.Definition.Name, "type": act.Type, "rule": "auto", "version": d.Definition.Version}}, nil
}

// ---------------------------------------------------------------- helpers

func respond(w http.ResponseWriter, v any, err error) { respondStatus(w, http.StatusOK, v, err) }

func respondStatus(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		code, msg := errorStatus(err)
		writeErr(w, code, msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// requestError is a request its caller must change, told to them as is (HTTP 400).
type requestError string

func (e requestError) Error() string { return string(e) }

// errorStatus is how an error is told to a caller, the same over HTTP (the status) and MCP (the message, mcpErr).
func errorStatus(err error) (int, string) {
	var bad requestError
	switch {
	case errors.As(err, &bad):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "not found"
	case store.IsBadID(err):
		return http.StatusBadRequest, "not a valid id"
	case errors.Is(err, store.ErrReplyElsewhere), errors.Is(err, store.ErrInvalid):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, store.ErrReceiptMismatch):
		return http.StatusConflict, err.Error()
	case errors.Is(err, store.ErrConflict):
		return http.StatusConflict, "no longer in a state that allows this"
	}
	return http.StatusInternalServerError, err.Error()
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// bodyRule says whether a request body may be empty.
type bodyRule bool

const (
	// bodyRequired: an empty body is a 400.
	bodyRequired bodyRule = false
	// bodyOptional: an empty body decodes as the zero value.
	bodyOptional bodyRule = true
)

// decode is how every handler reads its JSON body: at most limit bytes,
// one JSON value, and strictly, so a field the handler does not know is a
// 400. Lenient decoding would silently drop a misspelt field, and with it
// what the field was for (a dropped client_request_id loses idempotency).
// It writes the 400 itself; false means the handler returns.
func decode(w http.ResponseWriter, r *http.Request, limit int64, into any, rule bodyRule) bool {
	return decodeOr(w, r, limit, into, rule, func(msg string) { writeErr(w, http.StatusBadRequest, msg) })
}

// decodeOr is decode with the 400 written by fail, for routes whose errors
// have their own shape (the workspace's carry a check).
func decodeOr(w http.ResponseWriter, r *http.Request, limit int64, into any, rule bodyRule, fail func(msg string)) bool {
	if rule == bodyOptional && r.ContentLength == 0 {
		return true
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	err := dec.Decode(into)
	switch {
	case errors.Is(err, io.EOF) && rule == bodyOptional:
		// An empty body sent without a length (chunked).
		return true
	case errors.Is(err, io.EOF):
		fail("a JSON body is required")
		return false
	case err != nil:
		fail(err.Error())
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		fail("the body must be one JSON value")
		return false
	}
	return true
}

// queryInt reads an integer query parameter into dst, leaving it 0 when
// absent. A value that is not an integer is a 400 (written here), never a
// silent 0 that would read as "the default".
func queryInt(w http.ResponseWriter, r *http.Request, name string, dst *int) bool {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, name+": want an integer")
		return false
	}
	*dst = n
	return true
}

// orEmpty makes a nil slice encode as [] rather than null.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
