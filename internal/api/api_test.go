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

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/auth"
	"github.com/kensac/orbit/internal/auth/authtest"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
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

type catalogDefineRunner struct{ output string }

func (*catalogDefineRunner) Kind() string  { return "claude-code" }
func (*catalogDefineRunner) Model() string { return "fake" }
func (r *catalogDefineRunner) Run(context.Context, runner.Request, func(runner.Event)) (*runner.Result, error) {
	cost := 0.01
	return &runner.Result{Output: json.RawMessage(r.output), CostUSD: &cost, SessionID: "catalog-define"}, nil
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
github: {app_id: 1, private_key_env: GH_KEY, repositories: [kensac/orbit]}
`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "a.yaml"), []byte("name: a\ncharter: x\nwake_on_message: true\nactions: {notify: approve}\n"), 0o644)
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &engine.Engine{Store: s, ConfigDir: dir, Secret: []byte(token), Client: http.DefaultClient, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
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

func TestAddCatalogConnectorDiscoversOAuthAndStartsSignIn(t *testing.T) {
	var oauthURL string
	oauthServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/.well-known/oauth-protected-resource/mcp"), r.URL.Path == "/.well-known/oauth-protected-resource":
			_, _ = io.WriteString(w, `{"resource":"`+oauthURL+`/mcp","authorization_servers":["`+oauthURL+`"],"scopes_supported":["read"]}`)
		case r.URL.Path == "/.well-known/oauth-authorization-server":
			_, _ = io.WriteString(w, `{"issuer":"`+oauthURL+`","authorization_endpoint":"`+oauthURL+`/authorize","token_endpoint":"`+oauthURL+`/token","registration_endpoint":"`+oauthURL+`/register"}`)
		case r.URL.Path == "/register":
			_, _ = io.WriteString(w, `{"client_id":"orbit-test"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer oauthServer.Close()
	oauthURL = oauthServer.URL
	v := newEnv(t, func(a *api.Server, _ *http.Server) { a.LinkClient = oauthServer.Client() })
	v.e.Client = oauthServer.Client()
	v.e.Env = func(name string) (string, bool) {
		if name == spec.ConnectorKeyEnv {
			return token, true
		}
		return "", false
	}
	url := v.srv.URL + "/api/v1/connectors/catalog"
	person := v.person("kanishk")
	if code, b := call(t, "POST", url, person, `{"name":"acme","url":"`+oauthServer.URL+`/mcp","description":"Acme data","scopes":[]}`); code != http.StatusCreated {
		t.Fatalf("add catalog connector = %d %v", code, b)
	} else if b["authorize_url"] == nil || !strings.Contains(b["authorize_url"].(string), "client_id=orbit-test") {
		t.Fatalf("authorize URL = %v", b)
	}
	if code, b := call(t, "GET", url, person, ""); code != http.StatusOK || len(b["connectors"].([]any)) != 1 {
		t.Fatalf("catalog = %d %v", code, b)
	}

	manager, err := v.e.DefineAgent(ctx, engine.DefineRequest{Author: "kanishk", Spec: json.RawMessage(`{"name":"manager","charter":"Manage reports.","actions":{"agent:define":"auto"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	var defineType actions.Type
	for _, typ := range actions.Catalog(manager.Agent, v.e.Current().Config.Deployment) {
		if typ.Name == actions.DefineType {
			defineType = typ
			break
		}
	}
	if defineType.Name == "" {
		t.Fatal("agent:define was not offered")
	}
	specProps := defineType.ParamsSchema["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
	connectorItems := specProps["connectors"].(map[string]any)["items"].(map[string]any)
	if enum := connectorItems["enum"].([]string); len(enum) != 1 || enum[0] != "acme" {
		t.Fatalf("agent:define connector choices = %v", enum)
	}
	v.e.NewRunner = func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) {
		return &catalogDefineRunner{output: `{"summary":"s","report":"r","notify":{"send":false,"priority":"default","title":""},"memory":{"update":false,"content":""},"questions":[],"actions":[{"type":"agent:define","reason":"Enable the Acme connector for reports.","params":{"spec":{"name":"reports","charter":"Read Acme data.","connectors":["acme"]}}}]}`}, nil
	}
	if _, err := v.s.EnqueueRun(ctx, store.NewRun{Agent: manager.Agent.Name, SpecSHA: manager.Agent.SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := v.s.ClaimRun(ctx, "catalog-define-test")
	if err != nil || claimed == nil {
		t.Fatalf("claim manager run = %v, %v", claimed, err)
	}
	if err := v.e.Execute(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	acts, err := v.s.ListActions(ctx, "manager", "pending", 10)
	if err != nil || len(acts) != 1 {
		t.Fatalf("pending agent:define = %+v, %v", acts, err)
	}
	act := acts[0]
	if act.Type != actions.DefineType || act.Rule != string(spec.RuleApprove) || act.Status != "pending" {
		t.Fatalf("enabling a catalog connector was not held for approval: %+v", act)
	}
	preview, err := v.e.PreviewAction(ctx, &act)
	if err != nil || !strings.Contains(string(preview), "connector acme") {
		t.Fatalf("agent:define preview = %s, %v", preview, err)
	}
	if got := v.e.Current().Config.Agents["reports"]; got != nil {
		t.Fatalf("reports was enabled before approval: %+v", got.Connectors)
	}
	if err := v.s.DecideAction(ctx, act.ID, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if err := v.e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	if got := v.e.Current().Config.Agents["reports"]; got == nil || len(got.Connectors) != 1 || got.Connectors[0] != "acme" {
		t.Fatalf("approved catalog connector not enabled: %+v", got)
	}
	if code, _ := call(t, "POST", url, person, `{"name":"unsafe","url":"http://127.0.0.1/mcp"}`); code != http.StatusBadRequest {
		t.Fatalf("non-HTTPS connector URL = %d", code)
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

type replayRunner struct {
	got    runner.Request
	calls  int
	output json.RawMessage
}

func (r *replayRunner) Kind() string  { return "claude-code" }
func (r *replayRunner) Model() string { return "fake" }
func (r *replayRunner) Run(_ context.Context, req runner.Request, _ func(runner.Event)) (*runner.Result, error) {
	r.got = req
	r.calls++
	cost := 0.13
	output := r.output
	if len(output) == 0 {
		output = json.RawMessage(`{"summary":"ok","report":"ok","notify":{"send":false,"priority":"default","title":""},"memory":{"update":false,"content":""},"questions":[],"actions":[]}`)
	}
	return &runner.Result{Output: output, CostUSD: &cost}, nil
}

func TestEvaluationReplayUsesCurrentAgentWithoutPersisting(t *testing.T) {
	v := newEnv(t, nil)
	fr := &replayRunner{}
	v.e.NewRunner = func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) { return fr, nil }
	before, err := v.s.ListRuns(ctx, store.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	code, got := call(t, "POST", v.internal.URL+"/api/v1/agents/a/replay", token, `{"cases":[{"name":"one","input":{"records":[1,2]}}]}`)
	results, _ := got["results"].([]any)
	if code != http.StatusOK || len(results) != 1 || results[0].(map[string]any)["cost_usd"] != 0.13 {
		t.Fatalf("replay = %d %v", code, got)
	}
	if len(fr.got.Tools) != 0 || fr.got.Harness != nil || fr.got.Session != nil || fr.got.Connectors != nil {
		t.Fatalf("replay received capabilities: %+v", fr.got)
	}
	if len(fr.got.Inputs) != 1 || string(fr.got.Inputs[0].Content) != `{"records":[1,2]}` || !strings.Contains(fr.got.Prompt, "x") {
		t.Fatalf("replay request = %+v", fr.got)
	}
	after, err := v.s.ListRuns(ctx, store.RunFilter{})
	if err != nil || len(after) != len(before) {
		t.Fatalf("replay persisted a run: before %d after %d err %v", len(before), len(after), err)
	}
	if code, _ := call(t, "POST", v.internal.URL+"/api/v1/agents/a/replay", token, `{"cases":[{"name":"one","input":{"records":[]}}],"unexpected":true}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", code)
	}
	if code, _ := call(t, "POST", v.internal.URL+"/api/v1/agents/a/replay", token, `{}`); code != http.StatusBadRequest || fr.calls != 1 {
		t.Fatalf("missing input = %d, backend calls %d", code, fr.calls)
	}
}

func TestEvaluationGradeUsesSeparateReadOnlyBackendWithoutPersisting(t *testing.T) {
	v := newEnv(t, nil)
	fr := &replayRunner{output: json.RawMessage(`{"passed":true,"reason":"the output follows the rubric"}`)}
	v.e.NewRunner = func(name string, _ spec.RunnerSpec, _ func(string) (string, bool)) (runner.Runner, error) {
		if name != "grader-backend" {
			t.Errorf("unexpected runner %q", name)
		}
		return fr, nil
	}
	cfg := *v.e.Current().Config
	targetAgent := *cfg.Agents["a"]
	targetAgent.Charter = "TARGET_AGENT_CHARTER_SENTINEL"
	gradeAgent := targetAgent
	gradeAgent.Name, gradeAgent.Runner = "grader", "grader-backend"
	gradeAgent.Charter = "GRADER_AGENT_CHARTER_SENTINEL"
	cfg.Agents = map[string]*spec.Agent{"a": &targetAgent, "grader": &gradeAgent}
	cfg.Deployment.Runners = map[string]spec.RunnerSpec{"r": {Kind: "claude-code"}, "grader-backend": {Kind: "claude-code", Model: "grader-model"}}
	if err := v.e.SetConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	before, err := v.s.ListRuns(ctx, store.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	code, got := call(t, "POST", v.internal.URL+"/api/v1/agents/grader/grade", token,
		`{"target_agent":"a","cases":[{"name":"one","rubric":"mentions the saved total","output":{"summary":"total is 4"}}]}`)
	grades, _ := got["grades"].([]any)
	if code != http.StatusOK || len(grades) != 1 || grades[0].(map[string]any)["passed"] != true {
		t.Fatalf("grade = %d %v", code, got)
	}
	if len(fr.got.Tools) != 0 || fr.got.Harness != nil || fr.got.Session != nil || fr.got.Connectors != nil || fr.got.SkillsPlugin != "" {
		t.Fatalf("grader received capabilities: %+v", fr.got)
	}
	if len(fr.got.Inputs) != 1 || !strings.Contains(string(fr.got.Inputs[0].Content), "mentions the saved total") || !strings.Contains(string(fr.got.Inputs[0].Content), `"summary":"total is 4"`) || strings.Contains(fr.got.System, targetAgent.Charter) || strings.Contains(fr.got.System, gradeAgent.Charter) {
		t.Fatalf("grader request = %+v", fr.got)
	}
	if code, _ := call(t, "POST", v.internal.URL+"/api/v1/agents/grader/grade", token,
		`{"target_agent":"a","unexpected":true,"cases":[]}`); code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", code)
	}
	if code, _ := call(t, "POST", v.internal.URL+"/api/v1/agents/grader/grade", token,
		`{"target_agent":"grader","cases":[{"name":"one","rubric":"check","output":{}}]}`); code != http.StatusBadRequest {
		t.Fatalf("same grader and target accepted: %d", code)
	}
	after, err := v.s.ListRuns(ctx, store.RunFilter{})
	if err != nil || len(after) != len(before) {
		t.Fatalf("grading persisted a run: before %d after %d err %v", len(before), len(after), err)
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

// TestAnIDThatIsNotAUUIDIs400: such an id names no row, and is the
// client's mistake, not a server error.
func TestAnIDThatIsNotAUUIDIs400(t *testing.T) {
	srv, _, _ := server(t)
	for _, path := range []string{"/runs/nope", "/runs/nope/events", "/actions/nope", "/conversations/nope/thread", "/artifacts/nope"} {
		if code, v := call(t, "GET", srv.URL+"/api/v1"+path, token, ""); code != 400 || v["error"] != "not a valid id" {
			t.Errorf("%s = %d %v", path, code, v)
		}
	}
}

// TestLimitsAreClampedNotReset: asking for more than a list's maximum gets
// the maximum, not the default.
func TestLimitsAreClampedNotReset(t *testing.T) {
	srv, s, e := server(t)
	a := e.Current().Config.Agents["a"]
	for range 60 {
		if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: a.SHA, Trigger: "manual"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, v := call(t, "GET", srv.URL+"/api/v1/runs?limit=100000", token, ""); len(v["runs"].([]any)) != 60 {
		t.Fatalf("runs = %d, want all 60", len(v["runs"].([]any)))
	}
}

// TestAPendingScheduleChangeShowsWhatItWillDo: the person deciding sees the rule code parsed and the next fires, in the API and on the approval page.
func TestAPendingScheduleChangeShowsWhatItWillDo(t *testing.T) {
	v := newEnv(t, nil)
	srv, s, e := v.internal, v.s, v.e
	if _, err := e.DefineAgent(ctx, engine.DefineRequest{Spec: json.RawMessage(`{"name": "news", "charter": "x", "actions": {"schedule:add": "approve"}}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "news", SpecSHA: e.Current().Config.Agents["news"].SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	c, _ := s.ClaimRun(ctx, "w")
	ids, err := s.RecordOutcome(ctx, c.ID, "news", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{{Type: "schedule:add", Params: json.RawMessage(`{"when": "daily at 7:15"}`), Reason: "asked", Rule: "approve"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	code, got := call(t, "GET", srv.URL+"/api/v1/actions/"+ids[0], token, "")
	p, _ := got["preview"].(map[string]any)
	if code != 200 || p["rule"] != "FREQ=DAILY;BYHOUR=7;BYMINUTE=15;BYSECOND=0" || p["timezone"] != "UTC" || len(p["next_fires"].([]any)) != 3 {
		t.Fatalf("action = %d %v", code, got)
	}
	resp, _ := http.Get(strings.Replace(e.ApprovalLink(e.Current(), ids[0]), "http://orbit.test", v.srv.URL, 1))
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "What it will do") || !strings.Contains(string(b), "BYMINUTE=15") {
		t.Fatalf("page = %s", b)
	}
	// Decided, there is nothing to preview: the result says what it did.
	if err := s.DecideAction(ctx, ids[0], false, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if _, got = call(t, "GET", srv.URL+"/api/v1/actions/"+ids[0], token, ""); got["preview"] != nil {
		t.Fatalf("decided = %v", got)
	}
}

// pendingAction records one pending action of agent's run.
func pendingAction(t *testing.T, s *store.Store, e *engine.Engine, agent, typ, params string) string {
	t.Helper()
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: agent, SpecSHA: e.Current().Config.Agents[agent].SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	c, _ := s.ClaimRun(ctx, "w")
	ids, err := s.RecordOutcome(ctx, c.ID, agent, store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{{Type: typ, Params: json.RawMessage(params), Reason: "r", Rule: "approve"}}})
	if err != nil {
		t.Fatal(err)
	}
	return ids[0]
}

func TestApproveAndAlwaysAllow(t *testing.T) {
	v := newEnv(t, nil)
	srv, s, e := v.srv, v.s, v.e
	me := v.person("kanishk")
	if _, err := e.DefineAgent(ctx, engine.DefineRequest{Spec: json.RawMessage(`{"name": "news", "charter": "x", "actions": {"notify": "approve"}}`)}); err != nil {
		t.Fatal(err)
	}
	notify := `{"channel": "c", "title": "t", "body": "b"}`
	id := pendingAction(t, s, e, "news", "notify", notify)
	if code, got := call(t, "POST", srv.URL+"/api/v1/actions/"+id+"/reject", me, `{"always": true}`); code != 400 {
		t.Fatalf("reject always = %d %v", code, got)
	}
	code, got := call(t, "POST", srv.URL+"/api/v1/actions/"+id+"/approve", me, `{"always": true, "client_request_id": "r1"}`)
	al, _ := got["always"].(map[string]any)
	if code != 200 || got["ok"] != true || al["agent"] != "news" || al["type"] != "notify" || al["version"] != float64(2) {
		t.Fatalf("approve always = %d %v", code, got)
	}
	if a, _ := s.GetAction(ctx, id); a.Status != "approved" || *a.DecidedBy != "kanishk" {
		t.Fatalf("action = %+v", a)
	}
	if r := e.Current().Config.Agents["news"].RuleFor("notify"); r != spec.RuleAuto {
		t.Fatalf("rule = %s", r)
	}
	h, _ := s.DefinitionHistory(ctx, "news")
	if h[0].Author == nil || *h[0].Author != "kanishk" || h[0].RunID != nil {
		t.Fatalf("version = %+v", h[0])
	}
	// A retry changes nothing again.
	if code, got = call(t, "POST", srv.URL+"/api/v1/actions/"+id+"/approve", me, `{"always": true, "client_request_id": "r1"}`); code != 200 || got["always"] != nil {
		t.Fatalf("replay = %d %v", code, got)
	}
	if h2, _ := s.DefinitionHistory(ctx, "news"); len(h2) != len(h) {
		t.Fatalf("a replay wrote a version: %d", len(h2))
	}

	// A file agent cannot be changed here, and the action stays undecided.
	fileID := pendingAction(t, s, e, "a", "notify", notify)
	if code, got = call(t, "POST", srv.URL+"/api/v1/actions/"+fileID+"/approve", me, `{"always": true}`); code != 409 || !strings.Contains(got["error"].(string), "configuration files") {
		t.Fatalf("file agent = %d %v", code, got)
	}
	if a, _ := s.GetAction(ctx, fileID); a.Status != "pending" {
		t.Fatalf("file agent's action = %s", a.Status)
	}
	// A type already auto waits for another reason; always cannot help.
	autoID := pendingAction(t, s, e, "news", "notify", notify)
	if code, got = call(t, "POST", srv.URL+"/api/v1/actions/"+autoID+"/approve", me, `{"always": true}`); code != 409 || !strings.Contains(got["error"].(string), "already auto") {
		t.Fatalf("already auto = %d %v", code, got)
	}
}
