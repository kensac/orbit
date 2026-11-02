package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kensac/orbit/internal/auth"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// MCP: orbit's agents as tools for an MCP client (the Claude apps), over Streamable HTTP at /mcp. The same store calls the JSON API makes, with the same person: the token's preferred_username is the author and decider.

// mcpWaitDefault is how long send_message waits for the reply when the caller does not say: inside the minute a client keeps a tool call open without complaint, and long enough for a short answer.
const mcpWaitDefault = 45 * time.Second

// mcpWaitMax bounds one wait. A longer run is waited for again with wait_for_reply, so no tool call outlives a client's timeout by much.
const mcpWaitMax = 5 * time.Minute

// mcpPoll is how often a wait reads the run, as the stream does (streamPoll).
const mcpPoll = 500 * time.Millisecond

// mcpThreadLimit is read_thread's default page: the recent exchange, small enough for a model's context.
const mcpThreadLimit = 20

// mcpHandler serves /mcp. Stateless: every request stands alone, so a call lands on whichever instance traefik picks (two run during a rollout) and nothing is lost when one exits. Progress notifications ride on the request's own response stream.
func (s *Server) mcpHandler(internal bool) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "orbit", Version: "1"}, &mcp.ServerOptions{
		Instructions: "orbit runs standing agents. list_agents shows them; send_message talks to one " +
			"(in its default conversation, or in conversation_id) and returns its reply; " +
			"a reply still being written is fetched with wait_for_reply. Agents may ask questions " +
			"(answer with send_message, reply_to set to the question id) or propose actions " +
			"(approve_action / reject_action).",
	})
	s.addMCPTools(srv)
	s.addSearchTool(srv)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: false})
	return mcpauth.RequireBearerToken(s.mcpVerify(internal), &mcpauth.RequireBearerTokenOptions{
		ResourceMetadataURL:    s.resourceMetadataURL(),
		AllowMissingExpiration: true, // auth.Verifier has already required and checked exp
	})(h)
}

// mcpVerify accepts what /api/v1 accepts on the same listener: an authentik token, and on the internal listener the operator token.
func (s *Server) mcpVerify(internal bool) mcpauth.TokenVerifier {
	return func(ctx context.Context, raw string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		if internal && s.Token != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(s.Token)) == 1 {
			return &mcpauth.TokenInfo{UserID: ""}, nil
		}
		if s.Verifier == nil {
			return nil, fmt.Errorf("%w: authentik tokens are not configured on this server", mcpauth.ErrInvalidToken)
		}
		id, err := s.Verifier.Verify(ctx, raw)
		var ae *auth.Error
		if errors.As(err, &ae) {
			return nil, fmt.Errorf("%w: %s", mcpauth.ErrInvalidToken, ae.Error())
		}
		if err != nil {
			return nil, err
		}
		return &mcpauth.TokenInfo{UserID: id.Person}, nil
	}
}

func mcpPerson(req *mcp.CallToolRequest) string {
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		return req.Extra.TokenInfo.UserID
	}
	return ""
}

type mcpListAgentsIn struct{}

type mcpListConversationsIn struct {
	Agent    string `json:"agent" jsonschema:"the agent's name, from list_agents"`
	Archived bool   `json:"archived,omitempty" jsonschema:"list archived conversations instead"`
	Limit    int    `json:"limit,omitempty" jsonschema:"at most this many, newest activity first (default 50, at most 200)"`
	Before   string `json:"before,omitempty" jsonschema:"next_before from a previous page, to page back"`
}

type mcpCreateConversationIn struct {
	Agent       string `json:"agent" jsonschema:"the agent's name, from list_agents"`
	Title       string `json:"title,omitempty" jsonschema:"optional; the first message names it otherwise"`
	Message     string `json:"message,omitempty" jsonschema:"optional first message; the agent answers it"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the reply (default 45, at most 300, 0 waits the default, -1 does not wait)"`
}

type mcpSendMessageIn struct {
	Agent          string `json:"agent,omitempty" jsonschema:"the agent's name; the message goes to its default conversation unless conversation_id is set"`
	ConversationID string `json:"conversation_id,omitempty" jsonschema:"the conversation to post in (from list_conversations or create_conversation)"`
	Body           string `json:"body,omitempty" jsonschema:"the message, Markdown; may be empty when answers are given"`
	ReplyTo        string `json:"reply_to,omitempty" jsonschema:"the id of the agent question this answers"`
	WaitSeconds    int    `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the reply (default 45, at most 300, -1 does not wait)"`
	// Answers fill a question's form, as POST .../messages takes them.
	Answers map[string]any `json:"answers,omitempty" jsonschema:"answers to the form of the question reply_to names, by field name (the question's fields in read_thread)"`
}

type mcpWaitIn struct {
	RunID       string `json:"run_id" jsonschema:"the run send_message returned"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"how long to wait (default 45, at most 300)"`
}

type mcpReadThreadIn struct {
	Agent          string `json:"agent,omitempty" jsonschema:"read this agent's default conversation"`
	ConversationID string `json:"conversation_id,omitempty" jsonschema:"read this conversation"`
	Limit          int    `json:"limit,omitempty" jsonschema:"the newest this many items (default 20, at most 200)"`
	Before         string `json:"before,omitempty" jsonschema:"next_before from a previous page, to page back"`
}

type mcpListActionsIn struct {
	Agent  string `json:"agent,omitempty" jsonschema:"only this agent's"`
	Status string `json:"status,omitempty" jsonschema:"pending (default), approved, rejected, denied, executing, executed, failed, or all"`
	RunID  string `json:"run_id,omitempty" jsonschema:"only this run's proposals"`
	Limit  int    `json:"limit,omitempty" jsonschema:"at most this many, newest first (default 100, at most 500)"`
	Before string `json:"before,omitempty" jsonschema:"next_before from a previous page, to page back"`
}

type mcpDecideIn struct {
	ActionID string `json:"action_id" jsonschema:"the action's id, from list_actions or a reply"`
}

func (s *Server) addMCPTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "list_agents", Description: "List orbit's agents: what each is for, its last run, " +
		"and whether it is waiting on you (an open question or an action to approve)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ mcpListAgentsIn) (*mcp.CallToolResult, any, error) {
			rows, err := s.Store.ListAgents(ctx)
			if err != nil {
				return nil, nil, err
			}
			out := []map[string]any{}
			cfg := s.Engine.Current().Config
			for _, a := range rows {
				if !a.Present {
					continue
				}
				m := map[string]any{"name": a.Name, "enabled": a.Enabled, "needs_input": a.NeedsInput,
					"open_questions": a.Unread, "pending_actions": a.Pending, "last_run": a.LastRun}
				if sa, ok := cfg.Agents[a.Name]; ok {
					m["description"] = sa.Description
				}
				out = append(out, m)
			}
			return mcpJSON(map[string]any{"agents": out})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "list_conversations", Description: "List one agent's conversations, newest activity first."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpListConversationsIn) (*mcp.CallToolResult, any, error) {
			out, err := s.conversationPage(ctx, in.Agent, in.Archived, in.Before, in.Limit)
			if err != nil {
				return nil, nil, mcpErr(err)
			}
			return mcpJSON(out)
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "create_conversation", Description: "Open a new conversation with an agent, " +
		"optionally with a first message, and wait for the agent's reply to it."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpCreateConversationIn) (*mcp.CallToolResult, any, error) {
			a, err := s.mcpAgent(in.Agent)
			if err != nil {
				return nil, nil, err
			}
			res, replayed, err := s.Store.CreateConversation(ctx, store.NewConversation{
				Agent: a.Name, SpecSHA: a.SHA, Title: in.Title, Body: strings.TrimSpace(in.Message),
				Author: mcpPerson(req), Wake: a.MessagesTrigger,
			})
			if err != nil {
				return nil, nil, mcpErr(err)
			}
			if res.RunID != "" && !replayed {
				s.Wake()
			}
			out := map[string]any{"conversation_id": res.ConversationID}
			if res.MessageID != "" {
				out["message_id"] = res.MessageID
			}
			if res.RunID != "" {
				out["run_id"] = res.RunID
				if err := s.mcpWait(ctx, req, res.RunID, in.WaitSeconds, out); err != nil {
					return nil, nil, err
				}
			}
			return mcpJSON(out)
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "send_message", Description: "Send a message to an agent and wait for its reply. " +
		"The reply is the run's report, any questions it asked, and any actions it proposed. If the run is still going " +
		"when the wait ends, call wait_for_reply with the run_id."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSendMessageIn) (*mcp.CallToolResult, any, error) {
			if strings.TrimSpace(in.Body) == "" && in.Answers == nil {
				return nil, nil, errors.New("body or answers is required")
			}
			var a *spec.Agent
			var err error
			conv := in.ConversationID
			if conv != "" {
				c, cerr := s.Store.GetConversation(ctx, conv)
				if cerr != nil {
					return nil, nil, mcpErr(cerr)
				}
				a, err = s.mcpAgent(c.Agent)
			} else {
				a, err = s.mcpAgent(in.Agent)
			}
			if err != nil {
				return nil, nil, err
			}
			var replyTo *string
			if in.ReplyTo != "" {
				replyTo = &in.ReplyTo
			}
			var answers map[string]json.RawMessage
			if in.Answers != nil {
				answers = map[string]json.RawMessage{}
				for k, v := range in.Answers {
					answers[k], _ = json.Marshal(v)
				}
			}
			res, replayed, err := s.Store.PostMessageOnce(ctx, store.PostMessage{
				Agent: a.Name, SpecSHA: a.SHA, Body: in.Body, Author: mcpPerson(req), ReplyTo: replyTo,
				Wake: a.MessagesTrigger, ConversationID: conv, Answers: answers,
			})
			if err != nil {
				return nil, nil, mcpErr(err)
			}
			if res.RunID != "" && !replayed {
				s.Wake()
			}
			out := map[string]any{"message_id": res.MessageID}
			if res.RunID == "" {
				out["note"] = "the message is queued for the agent's next run; it did not start one"
				return mcpJSON(out)
			}
			out["run_id"] = res.RunID
			if err := s.mcpWait(ctx, req, res.RunID, in.WaitSeconds, out); err != nil {
				return nil, nil, err
			}
			return mcpJSON(out)
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "wait_for_reply", Description: "Wait for a run to finish and return its reply " +
		"(report, questions, actions). Returns at once for a finished run."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpWaitIn) (*mcp.CallToolResult, any, error) {
			out := map[string]any{"run_id": in.RunID}
			if in.WaitSeconds < 0 {
				in.WaitSeconds = 0
			}
			if err := s.mcpWait(ctx, req, in.RunID, in.WaitSeconds, out); err != nil {
				return nil, nil, err
			}
			return mcpJSON(out)
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "read_thread", Description: "Read a conversation: the person's messages, the agent's " +
		"questions, and every run's report and proposed actions, oldest first."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpReadThreadIn) (*mcp.CallToolResult, any, error) {
			limit := in.Limit
			if limit <= 0 {
				limit = mcpThreadLimit
			}
			var fetch func(*store.ThreadCursor, int) (*store.ThreadPage, error)
			switch {
			case in.ConversationID != "":
				fetch = func(before *store.ThreadCursor, limit int) (*store.ThreadPage, error) {
					return s.Store.ConversationThread(ctx, in.ConversationID, before, limit)
				}
			case in.Agent != "":
				fetch = func(before *store.ThreadCursor, limit int) (*store.ThreadPage, error) {
					return s.Store.Thread(ctx, in.Agent, before, limit)
				}
			default:
				return nil, nil, errors.New("give agent or conversation_id")
			}
			out, err := threadPage(in.Before, limit, fetch)
			if err != nil {
				return nil, nil, mcpErr(err)
			}
			return mcpJSON(out)
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "list_actions", Description: "List actions agents proposed; by default the ones " +
		"waiting for your approval."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mcpListActionsIn) (*mcp.CallToolResult, any, error) {
			status := in.Status
			switch status {
			case "":
				status = "pending"
			case "all":
				status = ""
			}
			out, err := s.actionPage(ctx, store.ActionFilter{Agent: in.Agent, Status: status, RunID: in.RunID, Limit: in.Limit}, in.Before)
			if err != nil {
				return nil, nil, mcpErr(err)
			}
			return mcpJSON(out)
		})

	decide := func(approve bool) mcp.ToolHandlerFor[mcpDecideIn, any] {
		return func(ctx context.Context, req *mcp.CallToolRequest, in mcpDecideIn) (*mcp.CallToolResult, any, error) {
			if err := s.decideAction(ctx, in.ActionID, approve, mcpPerson(req), ""); err != nil {
				return nil, nil, mcpErr(err)
			}
			a, err := s.Store.GetAction(ctx, in.ActionID)
			if err != nil {
				return nil, nil, mcpErr(err)
			}
			return mcpJSON(map[string]any{"action": a})
		}
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "approve_action", Description: "Approve an action an agent proposed; orbit executes it."},
		decide(true))
	mcp.AddTool(srv, &mcp.Tool{Name: "reject_action", Description: "Reject an action an agent proposed; it never executes."},
		decide(false))
}

// mcpAgent is a configured agent by name.
func (s *Server) mcpAgent(name string) (*spec.Agent, error) {
	if name == "" {
		return nil, errors.New("agent is required")
	}
	a, ok := s.Engine.Current().Config.Agents[name]
	if !ok {
		return nil, fmt.Errorf("no agent named %q (list_agents shows them)", name)
	}
	return a, nil
}

// mcpWait waits up to seconds (0: mcpWaitDefault; negative: not at all) for run id to finish, sending its narration as progress notifications when the client asked for progress, and puts the run (a thread run item) in out["reply"]. A run still going when the wait ends is reported as such, with what to call next.
func (s *Server) mcpWait(ctx context.Context, req *mcp.CallToolRequest, id string, seconds int, out map[string]any) error {
	wait := mcpWaitDefault
	switch {
	case seconds < 0:
		wait = 0
	case seconds > 0:
		wait = min(time.Duration(seconds)*time.Second, mcpWaitMax)
	}
	var token any
	if req != nil && req.Params != nil {
		token = req.Params.GetProgressToken()
	}
	n := &mcpNarrator{s: s, req: req, token: token, run: id}
	deadline := time.Now().Add(wait)
	for {
		p, err := s.Store.GetRunProgress(ctx, id)
		if err != nil {
			return mcpErr(err)
		}
		n.drain(ctx, p)
		if store.Terminal(p.Status) || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(mcpPoll):
		}
	}
	run, err := s.Store.ThreadRunByID(ctx, id)
	if err != nil {
		return mcpErr(err)
	}
	out["reply"] = run
	if !store.Terminal(run.Status) {
		out["note"] = "the run is still " + run.Status + "; call wait_for_reply with this run_id for the reply"
	}
	return nil
}

// mcpNarrator turns a run's journal into progress notifications, the MCP
// form of /runs/{id}/stream's text and tool events.
type mcpNarrator struct {
	s        *Server
	req      *mcp.CallToolRequest
	token    any
	run      string
	epoch    int
	seq      int
	progress float64
}

func (n *mcpNarrator) drain(ctx context.Context, p *store.RunProgress) {
	if n.token == nil || n.req == nil || n.req.Session == nil || p.Attempt == 0 {
		return
	}
	if n.s.Store.SyncJournal(ctx, n.run) != nil {
		return
	}
	h, err := n.s.Store.GetJournalHead(ctx, n.run, 0)
	if err != nil {
		return
	}
	if h.Epoch != n.epoch {
		n.epoch, n.seq = h.Epoch, 0
	}
	for {
		items, more, err := n.s.Store.ReadJournal(ctx, n.run, n.epoch, n.seq, store.MaxJournalLimit)
		if err != nil {
			return
		}
		for _, it := range items {
			n.seq = it.Seq
			ev, data := narration(it)
			if ev == "" {
				continue
			}
			m := data.(map[string]string)
			msg := m["text"]
			if ev == "tool" {
				msg = m["name"] + " " + m["summary"]
			}
			n.progress++
			// Best effort: a client that dropped progress still gets the reply.
			_ = n.req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: n.token, Progress: n.progress, Message: strings.TrimSpace(msg)})
		}
		if !more {
			return
		}
	}
}

func mcpJSON(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

// mcpErr words an error for a model exactly as the HTTP API words it (errorStatus).
func mcpErr(err error) error {
	_, msg := errorStatus(err)
	return errors.New(msg)
}
