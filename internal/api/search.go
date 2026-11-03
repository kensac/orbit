package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kensac/orbit/internal/store"
)

// searchParams is a search as a client writes it, from the query string or
// an MCP call.
type searchParams struct {
	Q              string `json:"q" jsonschema:"what to find: words or identifiers (parseConfig, parse_config, a/b.go), \"a quoted phrase\", or for either, -word to exclude; and operators agent:<name>, kind:<kinds>, repo:<owner/name or name>, path:<part of a file path>"`
	Agent          string `json:"agent,omitempty" jsonschema:"only this agent's"`
	ConversationID string `json:"conversation_id,omitempty" jsonschema:"only this conversation's"`
	RunID          string `json:"run_id,omitempty" jsonschema:"only this run's"`
	Kind           string `json:"kind,omitempty" jsonschema:"comma list of message, question, report, error, narration, tool_call, memory, memory_entry, skill, session"`
	From           string `json:"from,omitempty" jsonschema:"on or after: YYYY-MM-DD in the deployment's timezone, or an RFC 3339 time"`
	To             string `json:"to,omitempty" jsonschema:"up to: YYYY-MM-DD (that day included), or an RFC 3339 time (excluded)"`
	Limit          int    `json:"limit,omitempty" jsonschema:"at most this many results (default 20, at most 100)"`
	Offset         int    `json:"offset,omitempty" jsonschema:"skip this many, for the next page"`
}

// storeQuery resolves the dates in loc and splits the kinds.
func (p searchParams) storeQuery(loc *time.Location) (store.SearchQuery, error) {
	q := store.SearchQuery{Q: p.Q, Agent: p.Agent, ConversationID: p.ConversationID, RunID: p.RunID,
		Limit: p.Limit, Offset: p.Offset}
	for k := range strings.SplitSeq(p.Kind, ",") {
		if k = strings.TrimSpace(k); k != "" {
			q.Kinds = append(q.Kinds, k)
		}
	}
	var err error
	if q.From, err = store.SearchBound(p.From, loc, false); err != nil {
		return q, fmt.Errorf("from: %w", err)
	}
	if q.To, err = store.SearchBound(p.To, loc, true); err != nil {
		return q, fmt.Errorf("to: %w", err)
	}
	return q, nil
}

// searchLinks are where a result is read in full.
type searchLinks struct {
	Run          string `json:"run,omitempty"`
	RunPage      string `json:"run_page,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	Session      string `json:"session,omitempty"`
}

type searchHit struct {
	store.SearchResult
	Links searchLinks `json:"links"`
}

// search runs p against the store with the deployment's timezone and adds
// each result's links.
func (s *Server) search(ctx context.Context, p searchParams) (map[string]any, error) {
	cfg := s.Engine.Current().Config
	q, err := p.storeQuery(cfg.Deployment.Location())
	if err != nil {
		return nil, fmt.Errorf("%w: %s", store.ErrInvalid, err.Error())
	}
	page, err := s.Store.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	hits := make([]searchHit, len(page.Results))
	for i, r := range page.Results {
		h := searchHit{SearchResult: r}
		if r.RunID != nil {
			h.Links.Run = "/api/v1/runs/" + *r.RunID
			h.Links.RunPage = cfg.Deployment.RunURL(*r.RunID)
		}
		if r.ConversationID != nil {
			h.Links.Conversation = "/api/v1/conversations/" + *r.ConversationID + "/thread"
		}
		if r.ExternalSessionID != nil {
			h.Links.Session = "/api/v1/sessions/" + *r.ExternalSessionID
		}
		hits[i] = h
	}
	return map[string]any{"query": p.Q, "total": page.Total, "results": hits, "repaired": page.Repaired}, nil
}

// searchHTTP serves GET /search (docs/api.md).
func (s *Server) searchHTTP(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	p := searchParams{Q: v.Get("q"), Agent: v.Get("agent"), ConversationID: v.Get("conversation_id"),
		RunID: v.Get("run_id"), Kind: v.Get("kind"), From: v.Get("from"), To: v.Get("to")}
	if !queryInt(w, r, "limit", &p.Limit) || !queryInt(w, r, "offset", &p.Offset) {
		return
	}
	out, err := s.search(r.Context(), p)
	respond(w, out, err)
}

func (s *Server) addSearchTool(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "search", Description: "Full-text search across everything orbit stores: " +
		"people's messages, agents' questions, run reports and errors, what agents narrated while working, " +
		"the inputs of their tool calls, their memory, skills, and the person's imported Claude Code sessions. Ranked best first, each with a highlighted snippet " +
		"and the run and conversation it came from (read them with read_thread). A query that matches nothing is retried once with its misspelt words corrected, and repaired says so."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchParams) (*mcp.CallToolResult, any, error) {
			out, err := s.search(ctx, in)
			if err != nil {
				return nil, nil, mcpErr(err)
			}
			return mcpJSON(out)
		})
}
