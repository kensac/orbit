package engine_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// fakeMCP is an MCP server over streamable HTTP with a read tool, a write tool, a tool with a large result and one the agent denies; it refuses a request without the connector's credential.
type fakeMCP struct {
	mu     sync.Mutex
	writes []string
	auths  []string
}

const bigMarker = "the-end-of-a-large-result"

func (f *fakeMCP) handler(token func() string) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	obj := map[string]any{"type": "object"}
	text := func(s string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
	}
	srv.AddTool(&mcp.Tool{Name: "find", Description: "Find listings.", InputSchema: obj, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return text("found 3 listings for " + string(r.Params.Arguments)), nil
		})
	srv.AddTool(&mcp.Tool{Name: "create_view", Description: "Create a saved view.", InputSchema: obj},
		func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			f.mu.Lock()
			f.writes = append(f.writes, string(r.Params.Arguments))
			f.mu.Unlock()
			return text("created " + string(r.Params.Arguments)), nil
		})
	srv.AddTool(&mcp.Tool{Name: "dump", Description: "Everything.", InputSchema: obj},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return text(strings.Repeat("listing ", 20000) + bigMarker), nil
		})
	srv.AddTool(&mcp.Tool{Name: "drop_all", Description: "Delete everything.", InputSchema: obj},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return text("dropped"), nil })
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token() {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (f *fakeMCP) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

// connRunner is a claude-code runner whose turn is a Go function over the request, standing in for the CLI driving the run's connectors.
type connRunner struct {
	script func(ctx context.Context, req runner.Request) error
	output string
}

func (s *connRunner) Kind() string  { return "claude-code" }
func (s *connRunner) Model() string { return "fake" }
func (s *connRunner) Run(ctx context.Context, req runner.Request, emit func(runner.Event)) (*runner.Result, error) {
	if req.Session != nil && req.Session.Started != nil {
		_ = req.Session.Started("fresh", "")
	}
	if err := s.script(ctx, req); err != nil {
		return nil, err
	}
	cost := 0.01
	return &runner.Result{Output: json.RawMessage(s.output), CostUSD: &cost, SessionID: "s1"}, nil
}

const connQuietOutput = `{"summary": "done", "report": "done", "notify": {"send": false, "priority": "default", "title": ""},
  "memory": {"update": false, "content": ""}, "questions": [], "actions": []}`

type connectorHarness struct {
	e   *engine.Engine
	s   *store.Store
	mcp *fakeMCP
	h   *hooks
}

func connectorSetup(t *testing.T, sr *connRunner) *connectorHarness {
	t.Helper()
	s := testdb.New(t)
	f := &fakeMCP{}
	h := &hooks{}
	mux := http.NewServeMux()
	mux.Handle("/mcp", f.handler(func() string { return "hw-secret" }))
	mux.HandleFunc("/notify", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		h.mu.Lock()
		h.messages = append(h.messages, m)
		h.mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("orbit.yaml", fmt.Sprintf(`
timezone: UTC
public_url: http://orbit.test
defaults: {runner: fake, channels: [hook]}
runners: {fake: {kind: claude-code}}
channels:
  hook: {kind: webhook, url: %[1]s/notify}
credentials:
  HW_TOKEN: {url_prefix: %[1]s/}
connectors:
  hw:
    url: %[1]s/mcp
    description: Housing listings.
    auth: {kind: header, header: Authorization, value: "Bearer ${HW_TOKEN}"}
    write_tools: [create_view, drop_all]
`, srv.URL))
	write("agents/finance.yaml", `
name: finance
charter: Watch the listings.
triggers: [{schedule: "0 9 1 * *"}]
wake_on_message: true
connectors: [hw]
actions:
  "mcp:hw:drop_all": deny
`)
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		if k == "HW_TOKEN" {
			return "hw-secret", true
		}
		return "", false
	}
	e := &engine.Engine{
		Store: s, DataDir: t.TempDir(), ConfigDir: dir, Env: env, Client: srv.Client(), Secret: []byte("secret"),
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) { return sr, nil },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	specs := map[string]store.AgentSpec{}
	for n, a := range cfg.Agents {
		b, _ := json.Marshal(a)
		specs[n] = store.AgentSpec{SHA: a.SHA, JSON: b, Enabled: true}
	}
	if err := s.SyncAgents(ctx, specs); err != nil {
		t.Fatal(err)
	}
	return &connectorHarness{e: e, s: s, mcp: f, h: h}
}

// run executes one run of finance: trigger message (its person is watching) or manual.
func (ch *connectorHarness) run(t *testing.T, attended bool) *store.ClaimedRun {
	t.Helper()
	a := ch.e.Current().Config.Agents["finance"]
	if attended {
		if _, _, err := ch.s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: a.SHA, Body: "make me a view", Author: "kanishk", Wake: true}); err != nil {
			t.Fatal(err)
		}
	} else if _, err := ch.s.EnqueueRun(ctx, store.NewRun{Agent: "finance", SpecSHA: a.SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	c, err := ch.s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := ch.e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	run, err := ch.s.GetRun(ctx, c.ID)
	if err != nil || run.Status != "succeeded" {
		t.Fatalf("run = %+v %v", run, err)
	}
	return c
}

func resultText(t *testing.T, r runner.ConnectorResult) string {
	t.Helper()
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(r.Content, &blocks); err != nil || len(blocks) == 0 {
		t.Fatalf("content = %s %v", r.Content, err)
	}
	return blocks[0].Text
}

func server(t *testing.T, req runner.Request) runner.ConnectorServer {
	t.Helper()
	if req.Connectors == nil || len(req.Connectors.Servers) != 1 || req.Connectors.Servers[0].Name != "hw" {
		t.Fatalf("connectors = %+v", req.Connectors)
	}
	return req.Connectors.Servers[0]
}

func TestConnectorToolsAreListedAndAReadToolRuns(t *testing.T) {
	var got []string
	var file, fileContent string
	sr := &connRunner{output: connQuietOutput, script: func(ctx context.Context, req runner.Request) error {
		for _, tl := range server(t, req).Tools {
			got = append(got, fmt.Sprintf("%s ask=%v ro=%v", tl.Name, tl.Ask, tl.ReadOnly))
		}
		if !strings.Contains(req.System, "# Connectors") || !strings.Contains(req.System, "Housing listings.") || !strings.Contains(req.System, "Approve: create_view") {
			t.Errorf("system prompt lacks the connectors section:\n%s", req.System)
		}
		r := req.Connectors.Call(ctx, "hw", "find", "toolu_1", json.RawMessage(`{"q":"studio"}`))
		if r.IsError || !strings.Contains(resultText(t, r), `found 3 listings for {"q":"studio"}`) {
			t.Errorf("find = %+v", r)
		}
		big := req.Connectors.Call(ctx, "hw", "dump", "toolu_2", nil)
		file = big.File
		if b, err := os.ReadFile(filepath.Join(req.Workdir, big.File)); err == nil {
			fileContent = string(b)
		}
		if !strings.Contains(resultText(t, big), big.File) {
			t.Errorf("the model is not told where the result is: %s", big.Content)
		}
		if r := req.Connectors.Call(ctx, "hw", "drop_all", "toolu_3", nil); !r.IsError {
			t.Error("a denied tool ran")
		}
		return nil
	}}
	ch := connectorSetup(t, sr)
	c := ch.run(t, false)
	if strings.Join(got, "; ") != "create_view ask=true ro=false; dump ask=false ro=false; find ask=false ro=true" {
		t.Fatalf("tools offered = %v (drop_all is denied, so it is not even offered)", got)
	}
	if !strings.HasPrefix(file, "inputs/") || !strings.Contains(file, "connectors/hw-dump-") || !strings.Contains(fileContent, bigMarker) {
		t.Fatalf("large result: file %q, %d bytes", file, len(fileContent))
	}
	arts, err := ch.s.ListArtifacts(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range arts {
		names = append(names, a.Name)
	}
	if !strings.Contains(strings.Join(names, " "), "connectors/hw.tools.json") || !strings.Contains(strings.Join(names, " "), file) {
		t.Fatalf("artifacts = %v", names)
	}
	evs, err := ch.s.ListEvents(ctx, c.ID, 0, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, ev := range evs {
		var p struct {
			Step   string `json:"step"`
			Result *struct {
				Head  string `json:"head"`
				Bytes int    `json:"bytes"`
			} `json:"result"`
		}
		if ev.Kind == "orbit.connector" && json.Unmarshal(ev.Payload, &p) == nil && p.Step == "call" {
			calls++
			if p.Result == nil || p.Result.Head == "" || p.Result.Bytes == 0 {
				t.Errorf("a call's event lacks its bounded result: %s", ev.Payload)
			}
		}
	}
	if calls != 2 {
		t.Fatalf("connector call events = %d, want 2 (find, dump)", calls)
	}
	for _, a := range ch.mcp.auths {
		if a != "Bearer hw-secret" {
			t.Fatalf("a request went without the credential: %q", a)
		}
	}
	if acts, _ := ch.s.ListActions(ctx, "finance", "", 10); len(acts) != 0 {
		t.Fatalf("reads became actions: %+v", acts)
	}
}

func TestAnApproveToolWaitsForItsPersonInTheChat(t *testing.T) {
	var ch *connectorHarness
	sr := &connRunner{output: connQuietOutput, script: func(ctx context.Context, req runner.Request) error {
		server(t, req)
		// The person approves the first call and declines the second, from the chat.
		go func() {
			decided := 0
			for decided < 2 {
				time.Sleep(50 * time.Millisecond)
				acts, _ := ch.s.ListActions(ctx, "finance", "pending", 10)
				for _, a := range acts {
					_ = ch.s.DecideAction(ctx, a.ID, decided == 0, "kanishk")
					decided++
				}
			}
		}()
		args := json.RawMessage(`{"name":"cheap"}`)
		if p := req.Connectors.Permit(ctx, "hw", "create_view", "toolu_7", args); !p.Allow {
			t.Errorf("approved call denied: %+v", p)
		}
		if ch.mcp.writeCount() != 1 {
			t.Errorf("writes after approval = %d, want 1", ch.mcp.writeCount())
		}
		r := req.Connectors.Call(ctx, "hw", "create_view", "toolu_7", args)
		if r.IsError || !strings.Contains(resultText(t, r), `created {"name":"cheap"}`) {
			t.Errorf("the approved call's result = %+v", r)
		}
		if p := req.Connectors.Permit(ctx, "hw", "create_view", "toolu_8", json.RawMessage(`{"name":"other"}`)); p.Allow || !strings.Contains(p.Message, "declined") {
			t.Errorf("declined call = %+v", p)
		}
		return nil
	}}
	ch = connectorSetup(t, sr)
	ch.run(t, true)
	if ch.mcp.writeCount() != 1 {
		t.Fatalf("writes = %d, want exactly the approved one", ch.mcp.writeCount())
	}
	acts, err := ch.s.ListActions(ctx, "finance", "", 10)
	if err != nil || len(acts) != 2 {
		t.Fatalf("actions = %+v %v", acts, err)
	}
	byStatus := map[string]store.Action{}
	for _, a := range acts {
		byStatus[a.Status] = a
	}
	ex := byStatus["executed"]
	if ex.Type != "connector:hw/create_view" || ex.DecidedBy == nil || *ex.DecidedBy != "kanishk" || !strings.Contains(string(ex.Result), "created") {
		t.Fatalf("executed action = %+v (result %s)", ex, ex.Result)
	}
	if _, ok := byStatus["rejected"]; !ok {
		t.Fatalf("declined action missing: %+v", acts)
	}
	// The action loop finds nothing left to run.
	if err := ch.e.ExecuteActions(ctx); err != nil || ch.mcp.writeCount() != 1 {
		t.Fatalf("after the action loop: %v, writes %d", err, ch.mcp.writeCount())
	}
	ch.h.mu.Lock()
	defer ch.h.mu.Unlock()
	if len(ch.h.messages) != 0 {
		t.Fatalf("an attended run notified: %v", ch.h.messages)
	}
}

func TestAnApproveToolInAnUnattendedRunBecomesAPendingAction(t *testing.T) {
	sr := &connRunner{output: connQuietOutput, script: func(ctx context.Context, req runner.Request) error {
		p := req.Connectors.Permit(ctx, "hw", "create_view", "toolu_1", json.RawMessage(`{"name":"later"}`))
		if p.Allow || !strings.Contains(p.Message, "waiting for your person's approval") {
			t.Errorf("unattended permit = %+v", p)
		}
		// A call that never went through the prompt does not run either.
		if r := req.Connectors.Call(ctx, "hw", "create_view", "toolu_2", json.RawMessage(`{"name":"sneaky"}`)); !r.IsError {
			t.Errorf("an unprompted approve call ran: %+v", r)
		}
		return nil
	}}
	ch := connectorSetup(t, sr)
	c := ch.run(t, false)
	if ch.mcp.writeCount() != 0 {
		t.Fatalf("writes during the run = %d", ch.mcp.writeCount())
	}
	acts, err := ch.s.ListActions(ctx, "finance", "pending", 10)
	if err != nil || len(acts) != 2 || acts[0].RunID != c.ID || acts[0].Type != "connector:hw/create_view" {
		t.Fatalf("pending = %+v %v", acts, err)
	}
	ch.h.mu.Lock()
	approvals := 0
	for _, m := range ch.h.messages {
		if strings.Contains(fmt.Sprint(m), "wants to connector:hw/create_view") {
			approvals++
		}
	}
	ch.h.mu.Unlock()
	if approvals != 2 {
		t.Fatalf("approval notifications = %d: %v", approvals, ch.h.messages)
	}
	var later string
	for _, a := range acts {
		if strings.Contains(string(a.Params), "later") {
			later = a.ID
		}
	}
	if err := ch.s.DecideAction(ctx, later, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ch.e.ExecuteActions(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if ch.mcp.writeCount() != 1 || ch.mcp.writes[0] != `{"name":"later"}` {
		t.Fatalf("writes = %v, want the approved one once", ch.mcp.writes)
	}
	a, err := ch.s.GetAction(ctx, later)
	if err != nil || a.Status != "executed" || !strings.Contains(string(a.Result), "created") {
		t.Fatalf("action = %+v %v", a, err)
	}
}

// fakeAS is an MCP server's OAuth authorization server: registration, a code for a PKCE challenge, and tokens that it can revoke.
type fakeAS struct {
	mu        sync.Mutex
	challenge string
	valid     string
	refreshes int
}

func TestAnOAuthConnectorSignsInStoresVersionsAndRefreshes(t *testing.T) {
	s := testdb.New(t)
	as := &fakeAS{}
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base + "/as"}, "scopes_supported": []string{"listings"}})
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/as", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": base + "/as", "authorization_endpoint": base + "/as/authorize",
			"token_endpoint": base + "/as/token", "registration_endpoint": base + "/as/register"})
	})
	mux.HandleFunc("POST /as/register", func(w http.ResponseWriter, r *http.Request) {
		var reg struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reg)
		if len(reg.RedirectURIs) != 1 || reg.RedirectURIs[0] != "http://orbit.test"+engine.ConnectorCallbackPath {
			http.Error(w, "bad redirect", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "client-1"})
	})
	mux.HandleFunc("GET /as/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "client-1" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != base+"/mcp" || q.Get("scope") != "listings" {
			http.Error(w, "bad request "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		as.mu.Lock()
		as.challenge = q.Get("code_challenge")
		as.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=code-1&state="+q.Get("state"), http.StatusFound)
	})
	mux.HandleFunc("POST /as/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		as.mu.Lock()
		defer as.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			_, challenge := pkceOf(r.Form.Get("code_verifier"))
			if r.Form.Get("code") != "code-1" || challenge != as.challenge || r.Form.Get("client_id") != "client-1" {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			as.valid = "at-1"
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1", "refresh_token": "rt-1", "token_type": "Bearer", "expires_in": 3600})
		case "refresh_token":
			if r.Form.Get("refresh_token") != "rt-1" {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			as.refreshes++
			as.valid = "at-2"
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-2", "token_type": "Bearer", "expires_in": 3600})
		}
	})
	f := &fakeMCP{}
	mux.Handle("/mcp", f.handler(func() string {
		as.mu.Lock()
		defer as.mu.Unlock()
		return as.valid
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base = srv.URL

	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(fmt.Sprintf(`
public_url: http://orbit.test
defaults: {runner: fake}
runners: {fake: {kind: claude-code}}
connectors:
  hw: {url: %s/mcp, auth: {kind: oauth}}
`, srv.URL)), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "finance.yaml"), []byte("name: finance\ncharter: Watch.\ntriggers: []\nconnectors: [hw]\n"), 0o644)
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	sr := &connRunner{output: connQuietOutput, script: func(ctx context.Context, req runner.Request) error {
		if r := req.Connectors.Call(ctx, "hw", "find", "toolu_1", json.RawMessage(`{}`)); !r.IsError {
			found++
		}
		return nil
	}}
	key := strings.Repeat("k", 40)
	e := &engine.Engine{Store: s, DataDir: t.TempDir(), ConfigDir: dir, Client: srv.Client(), Secret: []byte("secret"),
		Env: func(k string) (string, bool) {
			if k == spec.ConnectorKeyEnv {
				return key, true
			}
			return "", false
		},
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) { return sr, nil },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg.Agents["finance"])
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"finance": {SHA: cfg.Agents["finance"].SHA, JSON: b, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "finance", SpecSHA: cfg.Agents["finance"].SHA, Trigger: "manual"}); err != nil {
			t.Fatal(err)
		}
		c, err := s.ClaimRun(ctx, "test")
		if err != nil || c == nil {
			t.Fatalf("claim %v %v", c, err)
		}
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	// Not signed in: the run goes on without the connector.
	run()
	if found != 0 {
		t.Fatal("a connector nobody signed in to was called")
	}

	u, err := e.ConnectorAuthorize(ctx, "hw", "kanishk")
	if err != nil {
		t.Fatal(err)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(u)
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %v %v", resp, err)
	}
	loc, _ := resp.Location()
	if name, err := e.ConnectorCallback(ctx, loc.Query().Get("state"), loc.Query().Get("code")); err != nil || name != "hw" {
		t.Fatalf("callback = %q %v", name, err)
	}
	if _, err := e.ConnectorCallback(ctx, "forged", "code-1"); err == nil {
		t.Fatal("a forged state was accepted")
	}
	tok, err := s.LatestConnectorToken(ctx, "hw")
	if err != nil || tok == nil || tok.Version != 1 || tok.Reason != "authorize" || tok.CreatedBy != "kanishk" || tok.ExpiresAt == nil {
		t.Fatalf("token = %+v %v", tok, err)
	}
	if strings.Contains(string(tok.Sealed), "at-1") || strings.Contains(string(tok.Sealed), "rt-1") {
		t.Fatal("the token is stored in the clear")
	}
	run()
	if found != 1 {
		t.Fatalf("calls after sign-in = %d", found)
	}

	// The server stops taking the token: orbit refreshes once, as a new version, and the call goes through.
	as.mu.Lock()
	as.valid = "at-2"
	as.mu.Unlock()
	run()
	tok, err = s.LatestConnectorToken(ctx, "hw")
	if err != nil || tok.Version != 2 || tok.Reason != "refresh" || as.refreshes != 1 || found != 2 {
		t.Fatalf("after refresh: token %+v %v, refreshes %d, calls %d", tok, err, as.refreshes, found)
	}
}

func pkceOf(verifier string) (string, string) {
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}
