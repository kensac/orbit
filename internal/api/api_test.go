package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/auth"
	"github.com/kensac/orbit/internal/auth/authtest"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

var token = strings.Repeat("t", 32)

var ctx = context.Background()

// env is a running API: the public listener (srv), the internal one, and
// the stand-in authentik issuer whose tokens the public one accepts.
type env struct {
	srv, internal *httptest.Server
	s             *store.Store
	e             *engine.Engine
	is            *authtest.Issuer
}

const (
	clientID = "orbit-ui"
	group    = "infra-admins"
)

// person returns a valid authentik token for name.
func (v *env) person(name string) string { return v.is.Mint(v.is.Valid(clientID, group, name)) }

func server(t *testing.T) (*httptest.Server, *store.Store, *engine.Engine) {
	t.Helper()
	v := newEnv(t, nil)
	return v.internal, v.s, v.e
}

func newEnv(t *testing.T, tweak func(*api.Server, *http.Server)) *env {
	t.Helper()
	s := testdb.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(`
public_url: http://orbit.test
defaults: {runner: r, channels: [c]}
runners: {r: {kind: claude-code}}
channels: {c: {kind: log}}
`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "a.yaml"), []byte("name: a\ncharter: x\nwake_on_message: true\nactions: {notify: approve}\n"), 0o644)
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &engine.Engine{Store: s, Secret: []byte(token), Client: http.DefaultClient, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Env: func(string) (string, bool) { return "", false }}
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg.Agents["a"])
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"a": {SHA: cfg.Agents["a"].SHA, JSON: b, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	is := authtest.NewIssuer(t)
	ver, err := auth.New(auth.Config{Issuers: []string{is.URL}, ClientIDs: []string{clientID}, Group: group})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	a := &api.Server{Store: s, Engine: e, Token: token, Verifier: ver, Wake: func() {}, Stop: stop,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	pub := httptest.NewUnstartedServer(nil)
	if tweak != nil {
		tweak(a, pub.Config)
	}
	pub.Config.Handler = a.Handler()
	pub.Start()
	internal := httptest.NewServer(a.InternalHandler())
	t.Cleanup(func() {
		close(stop)
		pub.Close()
		internal.Close()
	})
	return &env{srv: pub, internal: internal, s: s, e: e, is: is}
}

func call(t *testing.T, method, u, auth, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func TestInternalListenerAcceptsTheOperatorToken(t *testing.T) {
	srv, _, _ := server(t)
	if code, _ := call(t, "GET", srv.URL+"/api/v1/agents", "", ""); code != 401 {
		t.Fatalf("no token = %d", code)
	}
	if code, _ := call(t, "GET", srv.URL+"/api/v1/agents", "wrong", ""); code != 401 {
		t.Fatalf("wrong token = %d", code)
	}
	code, v := call(t, "GET", srv.URL+"/api/v1/agents", token, "")
	if code != 200 || len(v["agents"].([]any)) != 1 {
		t.Fatalf("agents = %d %v", code, v)
	}
	if resp, _ := http.Get(srv.URL + "/healthz"); resp.StatusCode != 200 {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
}

func TestManualRunAndMessageWake(t *testing.T) {
	srv, s, _ := server(t)
	code, v := call(t, "POST", srv.URL+"/api/v1/agents/a/runs", token, `{"prompt":"now"}`)
	if code != 202 || v["run_id"] == "" {
		t.Fatalf("start run = %d %v", code, v)
	}
	if code, _ := call(t, "POST", srv.URL+"/api/v1/agents/nope/runs", token, ""); code != 404 {
		t.Fatalf("unknown agent = %d", code)
	}
	code, v = call(t, "POST", srv.URL+"/api/v1/agents/a/messages", token, `{"body":"the $500 was a deductible"}`)
	if code != 201 || v["run_id"] == nil {
		t.Fatalf("message = %d %v, want a run woken (wake_on_message)", code, v)
	}
	runs, _ := s.ListRuns(ctx, store.RunFilter{Agent: "a"})
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
}

func TestApprovalPage(t *testing.T) {
	v := newEnv(t, nil)
	srv, s, e := v.srv, v.s, v.e
	id, _ := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: e.Current().Config.Agents["a"].SHA, Trigger: "manual"})
	c, _ := s.ClaimRun(ctx, "w")
	ids, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{{Type: "notify", Params: json.RawMessage(`{"channel":"c","title":"t","body":"b"}`), Reason: "because", Rule: "approve"}},
	})
	if err != nil || id == "" {
		t.Fatal(err)
	}
	link := e.ApprovalLink(e.Current(), ids[0])
	page := strings.Replace(link, "http://orbit.test", srv.URL, 1)

	resp, _ := http.Get(page)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "because") || !strings.Contains(string(b), "Approve") {
		t.Fatalf("page = %d %s", resp.StatusCode, b)
	}
	if resp, _ := http.Get(srv.URL + "/ui/actions/" + ids[0] + "?sig=forged"); resp.StatusCode != 403 {
		t.Fatalf("forged sig = %d", resp.StatusCode)
	}
	// A GET never decides: still pending.
	if a, _ := s.GetAction(ctx, ids[0]); a.Status != "pending" {
		t.Fatalf("status after GET = %s", a.Status)
	}
	sig := strings.SplitN(link, "sig=", 2)[1]
	resp, _ = http.PostForm(srv.URL+"/ui/actions/"+ids[0], url.Values{"sig": {sig}, "decision": {"approve"}})
	resp.Body.Close()
	if a, _ := s.GetAction(ctx, ids[0]); resp.StatusCode != 200 || a.Status != "approved" || *a.DecidedBy != "approval-link" {
		t.Fatalf("after approve: %d %s", resp.StatusCode, a.Status)
	}
	resp, _ = http.PostForm(srv.URL+"/ui/actions/"+ids[0], url.Values{"sig": {sig}, "decision": {"reject"}})
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("second decision = %d, want 409", resp.StatusCode)
	}
}
