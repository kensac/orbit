package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// ToolSearch is orbit's full-text search over the agent's own history, as
// the model sees it. The tool server holds no database credential: it asks
// the engine that runs the agent, over a unix socket the engine serves for
// that agent alone (engine/search.go), so a run can only ever search its
// own agent's records.
const ToolSearch = "mcp__orbit__search"

// SearchRequest is one search as the model asks it; the engine adds the
// agent.
type SearchRequest struct {
	Q              string `json:"q"`
	Kind           string `json:"kind,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	From           string `json:"from,omitempty"`
	To             string `json:"to,omitempty"`
	Limit          int    `json:"limit,omitempty"`
}

// SearchResponse is the engine's answer: a result document, or why not.
type SearchResponse struct {
	Result map[string]any `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// SearchSchema is the tool's input schema.
var SearchSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"q"},
	"properties": map[string]any{
		"q": map[string]any{"type": "string",
			"description": `what to find: words, "a quoted phrase", "or" between alternatives, -word to exclude`},
		"kind": map[string]any{"type": "string",
			"description": "comma list of message (your person's), question (yours), report, error, narration (what you wrote while working), tool_call (a tool's input), memory"},
		"conversation_id": map[string]any{"type": "string", "description": "only this conversation's"},
		"from":            map[string]any{"type": "string", "description": "on or after: YYYY-MM-DD, or an RFC 3339 time"},
		"to":              map[string]any{"type": "string", "description": "up to: YYYY-MM-DD (that day included), or an RFC 3339 time"},
		"limit":           map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "at most this many (default 10)"},
	},
}

// searchTimeout bounds one search: an indexed query answers in well under
// a second, and the CLI waits on the tool call meanwhile.
const searchTimeout = 20 * time.Second

// maxSearchResponse bounds the answer read back: 50 results of two short
// fragments each fit many times over.
const maxSearchResponse = 1 << 20

// Search asks the engine listening on socket.
func Search(socket string, req SearchRequest) (map[string]any, error) {
	conn, err := net.DialTimeout("unix", socket, searchTimeout)
	if err != nil {
		return nil, fmt.Errorf("search is unavailable: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(searchTimeout))
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxSearchResponse)).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	var resp SearchResponse
	if err := json.Unmarshal(bytes.TrimSpace(line), &resp); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Result, nil
}

// ReadSearchRequest reads one request line, for the engine's side.
func ReadSearchRequest(conn net.Conn) (SearchRequest, error) {
	var req SearchRequest
	line, err := bufio.NewReader(io.LimitReader(conn, 64<<10)).ReadBytes('\n')
	if err != nil {
		return req, err
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("search: %w", err)
	}
	return req, nil
}
