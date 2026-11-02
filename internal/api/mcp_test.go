package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/store"
)

type bearerRT struct{ tok string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func mcpSession(t *testing.T, endpoint, tok string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint,
		HTTPClient: &http.Client{Transport: bearerRT{tok}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	var v map[string]any
	if !res.IsError {
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			t.Fatalf("%s: %v: %s", name, err, text)
		}
	} else {
		v = map[string]any{"error": text}
	}
	return v, res.IsError
}

func TestMCPNeedsAToken(t *testing.T) {
	v := newEnv(t, func(a *api.Server, _ *http.Server) {
		a.OAuth = &api.OAuthProxy{Issuer: "http://unused/", ClientID: "x", ClientSecret: "y"}
		a.OAuth.SetKey([]byte(token))
	})
	req, _ := http.NewRequest("POST", v.srv.URL+"/mcp", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"),
		`resource_metadata="http://orbit.test/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("no token = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	// The operator token is refused on the public listener, as on /api/v1.
	req, _ = http.NewRequest("POST", v.srv.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("operator token on the public listener = %d", resp.StatusCode)
	}
	code, meta := get(t, v.srv.URL+"/.well-known/oauth-protected-resource/mcp", "")
	if code != 200 || meta["resource"] != "http://orbit.test/mcp" {
		t.Fatalf("resource metadata = %d %v", code, meta)
	}
}

func TestMCPTalksToAnAgent(t *testing.T) {
	v := newEnv(t, nil)
	cs := mcpSession(t, v.srv.URL+"/mcp", v.person("kanishk"))

	agents, _ := callTool(t, cs, "list_agents", nil)
	if list := agents["agents"].([]any); len(list) != 1 || list[0].(map[string]any)["name"] != "a" {
		t.Fatalf("agents = %v", agents)
	}

	// The agent answers while send_message waits.
	done := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			c, err := v.s.ClaimRun(ctx, "w")
			if err != nil {
				done <- err
				return
			}
			if c == nil {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			_, err = v.s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: c.Attempt, Status: "succeeded",
				Summary: "s", Report: "You spent $84."}, store.Outcome{
				Actions: []store.ProposedAction{{Type: "notify", Params: json.RawMessage(`{"channel":"c","title":"t","body":"b"}`),
					Reason: "tell them", Rule: "approve"}}})
			done <- err
			return
		}
		done <- context.DeadlineExceeded
	}()
	out, isErr := callTool(t, cs, "send_message", map[string]any{"agent": "a", "body": "How much on Uber Eats?", "wait_seconds": 10})
	if isErr {
		t.Fatalf("send_message: %v", out)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	reply := out["reply"].(map[string]any)
	if reply["status"] != "succeeded" || reply["report"] != "You spent $84." {
		t.Fatalf("reply = %v", out)
	}
	thread, _ := callTool(t, cs, "read_thread", map[string]any{"agent": "a"})
	items := thread["items"].([]any)
	// A run sorts before a message at the same instant (docs/api.md).
	if len(items) != 2 || items[1].(map[string]any)["author"] != "kanishk" {
		t.Fatalf("thread = %v", thread)
	}

	acts, _ := callTool(t, cs, "list_actions", nil)
	list := acts["actions"].([]any)
	if len(list) != 1 {
		t.Fatalf("pending = %v", acts)
	}
	id := list[0].(map[string]any)["id"].(string)
	got, _ := callTool(t, cs, "approve_action", map[string]any{"action_id": id})
	act := got["action"].(map[string]any)
	if act["status"] != "approved" || act["decided_by"] != "kanishk" {
		t.Fatalf("approved = %v", got)
	}
	if _, isErr := callTool(t, cs, "reject_action", map[string]any{"action_id": id}); !isErr {
		t.Fatal("a second decision should fail")
	}

	// A new conversation without waiting returns its run at once.
	conv, _ := callTool(t, cs, "create_conversation", map[string]any{"agent": "a", "message": "New topic", "wait_seconds": -1})
	if conv["conversation_id"] == nil || conv["run_id"] == nil || conv["reply"].(map[string]any)["status"] != "queued" {
		t.Fatalf("create = %v", conv)
	}
	convs, _ := callTool(t, cs, "list_conversations", map[string]any{"agent": "a"})
	if len(convs["conversations"].([]any)) != 2 {
		t.Fatalf("conversations = %v", convs)
	}
	if out, isErr := callTool(t, cs, "send_message", map[string]any{"agent": "nope", "body": "x"}); !isErr {
		t.Fatalf("unknown agent = %v", out)
	}
}

func TestMCPOperatorTokenOnTheInternalListener(t *testing.T) {
	v := newEnv(t, nil)
	cs := mcpSession(t, v.internal.URL+"/mcp", token)
	if out, isErr := callTool(t, cs, "list_agents", nil); isErr {
		t.Fatalf("list_agents = %v", out)
	}
}

// TestOAuthProxy walks the sign-in an MCP client makes: register, authorize (to authentik and back), exchange the code with PKCE, refresh.
func TestOAuthProxy(t *testing.T) {
	var v *env
	var refreshes int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/application/o/orbit-mcp/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"authorization_endpoint": "https://auth.test/application/o/authorize/",
				"token_endpoint":         "http://" + r.Host + "/application/o/token/"})
		case "/application/o/token/":
			_ = r.ParseForm()
			if r.PostForm.Get("client_id") != "mcp-client" || r.PostForm.Get("client_secret") != "shh" {
				w.WriteHeader(401)
				return
			}
			switch r.PostForm.Get("grant_type") {
			case "authorization_code":
				if r.PostForm.Get("code") != "up-code" {
					w.WriteHeader(400)
					return
				}
			case "refresh_token":
				if r.PostForm.Get("refresh_token") != "up-refresh" {
					w.WriteHeader(400)
					return
				}
				refreshes++
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": v.person("kanishk"), "token_type": "Bearer",
				"expires_in": 300, "refresh_token": "up-refresh"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	v = newEnv(t, func(a *api.Server, _ *http.Server) {
		a.OAuth = &api.OAuthProxy{Issuer: up.URL + "/application/o/orbit-mcp/", ClientID: "mcp-client", ClientSecret: "shh"}
		a.OAuth.SetKey([]byte(token))
	})
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	code, meta := get(t, v.srv.URL+"/.well-known/oauth-authorization-server", "")
	if code != 200 || meta["registration_endpoint"] != "http://orbit.test/oauth/register" {
		t.Fatalf("metadata = %d %v", code, meta)
	}
	if code, _ := call(t, "POST", v.srv.URL+"/oauth/register", "", `{"redirect_uris":["https://evil.test/cb"]}`); code != 400 {
		t.Fatalf("foreign redirect registered: %d", code)
	}
	cb := "https://claude.ai/api/mcp/auth_callback"
	code, reg := call(t, "POST", v.srv.URL+"/oauth/register", "", `{"redirect_uris":["`+cb+`"],"client_name":"Claude"}`)
	if code != 201 {
		t.Fatalf("register = %d %v", code, reg)
	}
	client := reg["client_id"].(string)

	verifier := "a-verifier-of-sufficient-length-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{"response_type": {"code"}, "client_id": {client}, "redirect_uri": {cb}, "state": {"cs"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	resp, err := noFollow.Get(v.srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 302 || loc.Host != "auth.test" || loc.Query().Get("client_id") != "mcp-client" ||
		loc.Query().Get("redirect_uri") != "http://orbit.test/oauth/callback" {
		t.Fatalf("authorize = %d %s", resp.StatusCode, loc)
	}
	// An unregistered redirect is refused, not followed.
	q2 := url.Values{"response_type": {"code"}, "client_id": {client}, "redirect_uri": {"https://claude.ai/other"}}
	if resp, _ := noFollow.Get(v.srv.URL + "/oauth/authorize?" + q2.Encode()); resp.StatusCode != 400 {
		t.Fatalf("unregistered redirect = %d", resp.StatusCode)
	}

	resp, err = noFollow.Get(v.srv.URL + "/oauth/callback?" + url.Values{"code": {"up-code"}, "state": {loc.Query().Get("state")}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(back.String(), cb+"?") || back.Query().Get("state") != "cs" || back.Query().Get("code") == "" {
		t.Fatalf("callback = %d %s", resp.StatusCode, back)
	}
	ourCode := back.Query().Get("code")

	form := url.Values{"grant_type": {"authorization_code"}, "code": {ourCode}, "client_id": {client},
		"redirect_uri": {cb}, "code_verifier": {"wrong"}}
	resp, _ = http.PostForm(v.srv.URL+"/oauth/token", form)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("wrong verifier = %d", resp.StatusCode)
	}
	form.Set("code_verifier", verifier)
	resp, _ = http.PostForm(v.srv.URL+"/oauth/token", form)
	var tok map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if resp.StatusCode != 200 || tok["access_token"] == nil || tok["refresh_token"] == nil || tok["refresh_token"] == "up-refresh" {
		t.Fatalf("token = %d %v", resp.StatusCode, tok)
	}
	resp, _ = http.PostForm(v.srv.URL+"/oauth/token", form)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("code reused = %d", resp.StatusCode)
	}

	// The access token works on /mcp.
	cs := mcpSession(t, v.srv.URL+"/mcp", tok["access_token"].(string))
	if out, isErr := callTool(t, cs, "list_agents", nil); isErr {
		t.Fatalf("list_agents = %v", out)
	}

	resp, _ = http.PostForm(v.srv.URL+"/oauth/token", url.Values{"grant_type": {"refresh_token"},
		"refresh_token": {tok["refresh_token"].(string)}, "client_id": {client}})
	var again map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&again)
	resp.Body.Close()
	if resp.StatusCode != 200 || again["access_token"] == nil || refreshes != 1 {
		t.Fatalf("refresh = %d %v (%d upstream)", resp.StatusCode, again, refreshes)
	}
}
