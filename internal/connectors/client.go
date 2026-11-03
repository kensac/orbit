// Package connectors is orbit's client side of MCP: it lists and calls the tools of the servers orbit.yaml names (spec.Connector), adding each one's credential itself, so a run's CLI and its sandbox never hold one. The engine decides which calls run (engine/connectors.go); this package only speaks to the servers.
package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kensac/orbit/internal/spec"
)

// Bounds on what a server can send orbit.
const (
	// MaxResponse bounds one HTTP response body, a tool list or a result.
	MaxResponse = 16 << 20
	// MaxTools bounds the tools orbit takes from one server.
	MaxTools = 512
	// CallTimeout bounds one call when the caller's context has no deadline.
	CallTimeout = 2 * time.Minute
)

// Tool is one tool a server lists.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// ReadOnly is the server's readOnlyHint.
	ReadOnly bool `json:"read_only"`
}

// Result is a call's result as the server sent it.
type Result struct {
	// Content is the result's content blocks, as MCP's JSON.
	Content json.RawMessage
	IsError bool
	// Raw is the whole CallToolResult.
	Raw json.RawMessage
}

// Authorizer adds a connector's credential to its requests.
type Authorizer interface {
	// Authorize sets req's credential.
	Authorize(ctx context.Context, req *http.Request) error
	// Refresh is called once after a 401: true means a new credential is in place and the request is tried again.
	Refresh(ctx context.Context) (bool, error)
}

// NoAuth is a connector without a credential.
type NoAuth struct{}

func (NoAuth) Authorize(context.Context, *http.Request) error { return nil }
func (NoAuth) Refresh(context.Context) (bool, error)          { return false, nil }

// HeaderAuth sends one header, rendered once from the connector's value.
type HeaderAuth struct{ Name, Value string }

func (h HeaderAuth) Authorize(_ context.Context, req *http.Request) error {
	req.Header.Set(h.Name, h.Value)
	return nil
}
func (HeaderAuth) Refresh(context.Context) (bool, error) { return false, nil }

// NewHeaderAuth renders a header connector's value: every ${VAR} in it was checked at load to be a credential bound to the connector's URL, and is checked again here.
func NewHeaderAuth(c spec.Connector, creds spec.Credentials, env func(string) (string, bool)) (HeaderAuth, error) {
	for _, r := range spec.EnvRefs(c.Auth.Value) {
		if _, ok := creds[r]; !ok {
			return HeaderAuth{}, fmt.Errorf("${%s} is not a credential", r)
		}
		if err := creds.Check(r, c.URL); err != nil {
			return HeaderAuth{}, err
		}
	}
	v, err := spec.ExpandEnv(c.Auth.Value, env)
	if err != nil {
		return HeaderAuth{}, err
	}
	return HeaderAuth{Name: c.Auth.Header, Value: v}, nil
}

// Client is one connector's MCP session, opened on first use and kept for the client's life (one run).
type Client struct {
	Name string
	URL  string
	HTTP *http.Client
	Auth Authorizer

	mu      sync.Mutex
	session *mcp.ClientSession
}

// New returns a client for connector name. hc's transport carries the requests; its redirects are never followed, so a credential goes only to the connector's own URL.
func New(name string, c spec.Connector, auth Authorizer, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	if auth == nil {
		auth = NoAuth{}
	}
	return &Client{Name: name, URL: c.URL, HTTP: hc, Auth: auth}
}

func (c *Client) connect(ctx context.Context) (*mcp.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		return c.session, nil
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	base := c.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hc := &http.Client{
		Transport: &authTransport{base: base, auth: c.Auth, host: u.Host},
		Timeout:   c.HTTP.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	cl := mcp.NewClient(&mcp.Implementation{Name: "orbit", Version: "1"}, nil)
	s, err := cl.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: c.URL, HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: MaxResponse,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connector %s: connect: %w", c.Name, err)
	}
	c.session = s
	return s, nil
}

// Close ends the session.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
}

func (c *Client) drop() {
	c.mu.Lock()
	s := c.session
	c.session = nil
	c.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// ListTools returns every tool the server lists, at most MaxTools.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	s, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	var out []Tool
	for t, err := range s.Tools(ctx, nil) {
		if err != nil {
			c.drop()
			return nil, fmt.Errorf("connector %s: list tools: %w", c.Name, err)
		}
		if len(out) >= MaxTools {
			break
		}
		schema, _ := json.Marshal(t.InputSchema)
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, Tool{Name: t.Name, Description: t.Description, InputSchema: schema,
			ReadOnly: t.Annotations != nil && t.Annotations.ReadOnlyHint})
	}
	return out, nil
}

// Call calls one tool once. A failed request is not retried: the server may have acted, and a write must not happen twice.
func (c *Client) Call(ctx context.Context, tool string, args json.RawMessage) (*Result, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, CallTimeout)
		defer cancel()
	}
	s, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		c.drop()
		return nil, fmt.Errorf("connector %s: %s: %w", c.Name, tool, err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	content, _ := json.Marshal(res.Content)
	if len(res.Content) == 0 {
		content = json.RawMessage(`[]`)
		if res.StructuredContent != nil {
			sc, _ := json.Marshal(res.StructuredContent)
			content, _ = json.Marshal([]map[string]any{{"type": "text", "text": string(sc)}})
		}
	}
	return &Result{Content: content, IsError: res.IsError, Raw: raw}, nil
}

// authTransport adds the credential to every request, sends nothing to another host, refreshes once on a 401, and bounds every response body.
type authTransport struct {
	base http.RoundTripper
	auth Authorizer
	host string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != t.host {
		return nil, fmt.Errorf("connector requests go only to %s, not %s", t.host, req.URL.Host)
	}
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	send := func() (*http.Response, error) {
		r := req.Clone(req.Context())
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		if err := t.auth.Authorize(req.Context(), r); err != nil {
			return nil, err
		}
		return t.base.RoundTrip(r)
	}
	resp, err := send()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		ok, rerr := t.auth.Refresh(req.Context())
		if rerr != nil {
			_ = resp.Body.Close()
			return nil, rerr
		}
		if ok {
			_ = resp.Body.Close()
			if resp, err = send(); err != nil {
				return nil, err
			}
		}
	}
	resp.Body = &limited{r: resp.Body, n: MaxResponse}
	return resp, nil
}

// limited fails a body past its bound instead of truncating it.
type limited struct {
	r io.ReadCloser
	n int64
}

// ErrTooLarge is a response past MaxResponse.
var ErrTooLarge = errors.New("the connector's response is larger than orbit accepts")

func (l *limited) Read(p []byte) (int, error) {
	if l.n <= 0 {
		var one [1]byte
		if n, err := l.r.Read(one[:]); n == 0 && err != nil {
			return 0, err
		}
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

func (l *limited) Close() error { return l.r.Close() }
