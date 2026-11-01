package engine_test

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

var ctx = context.Background()

// fakeRunner returns a fixed output and records the request it got.
type fakeRunner struct {
	output string
	err    error
	got    runner.Request
	block  chan struct{}
	// started is closed once Run begins, when block is set.
	started chan struct{}
}

func (f *fakeRunner) Kind() string  { return "claude-code" }
func (f *fakeRunner) Model() string { return "fake" }
func (f *fakeRunner) Run(ctx context.Context, req runner.Request, emit func(runner.Event)) (*runner.Result, error) {
	f.got = req
	emit(runner.Event{Kind: "assistant", Payload: []byte(`{"type":"assistant"}`)})
	if f.block != nil {
		close(f.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cost := 0.25
	return &runner.Result{Output: json.RawMessage(f.output), CostUSD: &cost, SessionID: "s1"}, f.err
}

// hooks collects what the webhook channel and endpoint receive.
type hooks struct {
	mu       sync.Mutex
	messages []map[string]any
	calls    []string
}

func harness(t *testing.T, fr *fakeRunner) (*engine.Engine, *store.Store, *hooks) {
	t.Helper()
	s := testdb.New(t)
	h := &hooks{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/notify":
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			h.mu.Lock()
			h.messages = append(h.messages, m)
			h.mu.Unlock()
		case "/endpoint":
			b, _ := io.ReadAll(r.Body)
			h.mu.Lock()
			h.calls = append(h.calls, string(b))
			h.mu.Unlock()
		case "/tx":
			if r.Header.Get("X-Api-Key") != "k-123" {
				http.Error(w, "no key", http.StatusUnauthorized)
				return
			}
			page := r.URL.Query().Get("page")
			fmt.Fprintf(w, `{"transactions":[{"page":%s}],"pagination":{"total_pages":2}}`, page)
		default:
			http.NotFound(w, r)
		}
	}))
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
timezone: America/New_York
public_url: http://orbit.test
defaults: {runner: fake, channels: [hook]}
runners: {fake: {kind: claude-code}}
channels:
  hook: {kind: webhook, url: %[1]s/notify}
endpoints:
  ep: {url: %[1]s/endpoint}
`, srv.URL))
	write("agents/finance.yaml", fmt.Sprintf(`
name: finance
charter: Review my spending.
triggers: [{schedule: "0 9 1 * *"}]
sources:
  - name: tx
    kind: http
    url: %s/tx
    headers: {X-Api-Key: "${TEST_KEY}"}
    paginate: {page_param: page, items_path: transactions, total_pages_path: pagination.total_pages}
actions:
  notify: auto
  webhook:ep: approve
`, srv.URL))
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		if k == "TEST_KEY" {
			return "k-123", true
		}
		return "", false
	}
	e := &engine.Engine{
		Store: s, DataDir: t.TempDir(), Env: env, Client: srv.Client(), Secret: []byte("secret"),
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) { return fr, nil },
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
	return e, s, h
}

func claim(t *testing.T, e *engine.Engine, s *store.Store) *store.ClaimedRun {
	t.Helper()
	a := e.Current().Config.Agents["finance"]
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "finance", SpecSHA: a.SHA, Trigger: "manual", Prompt: "check October"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	return c
}

const goodOutput = `{
  "summary": "October spend was $5,914",
  "report": "Spend was fine.",
  "notify": {"send": true, "priority": "default", "title": ""},
  "memory": {"update": true, "content": "baseline: 2700"},
  "questions": ["Was the $500 a deductible?"],
  "actions": [
    {"type": "notify", "params": {"channel": "hook", "title": "extra", "body": "hi"}, "reason": "test auto"},
    {"type": "webhook:ep", "params": {"body": {"x": 1}}, "reason": "test approve"},
    {"type": "webhook:ep", "params": {"nope": 1}, "reason": "bad params"}
  ]
}`

func TestRunEndToEnd(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, h := harness(t, fr)
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}

	run, _ := s.GetRun(ctx, c.ID)
	if run.Status != "succeeded" || *run.Summary != "October spend was $5,914" || *run.CostUSD != 0.25 {
		t.Fatalf("run = %s err=%v", run.Status, *run.Error)
	}
	// The source was fetched with the credential, both pages merged, and
	// written where the prompt says it is.
	if len(fr.got.Inputs) != 1 || string(fr.got.Inputs[0].Content) != `[{"page":1},{"page":2}]` {
		t.Fatalf("inputs = %+v", fr.got.Inputs)
	}
	if !strings.Contains(fr.got.Prompt, "inputs/tx.json") || !strings.Contains(fr.got.Prompt, "check October") {
		t.Fatalf("prompt is missing the input or the wake reason:\n%s", fr.got.Prompt)
	}
	if strings.Contains(fr.got.Prompt+fr.got.System, "k-123") {
		t.Fatal("the source credential reached the model")
	}
	// The workdir is scratch: gone after the run, its content kept as artifacts.
	if _, err := os.Stat(fr.got.Workdir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workdir still exists: %v", err)
	}
	arts, _ := s.ListArtifacts(ctx, c.ID)
	names := map[string]bool{}
	for _, a := range arts {
		names[a.Direction+":"+a.Name] = true
	}
	for _, want := range []string{"in:inputs/tx.json", "in:prompt.md", "in:system.md", "in:schema.json", "in:memory.md", "out:output.json"} {
		if !names[want] {
			t.Errorf("missing artifact %s (have %v)", want, names)
		}
	}
	if m, _ := s.Memory(ctx, "finance"); m != "baseline: 2700" {
		t.Fatalf("memory = %q", m)
	}

	acts, _ := s.ListActions(ctx, "finance", "", 0)
	status := map[string]int{}
	for _, a := range acts {
		status[a.Status]++
	}
	if status["approved"] != 1 || status["pending"] != 1 || status["denied"] != 1 {
		t.Fatalf("action statuses = %v", status)
	}

	// The report (with its question) and the approval request went out.
	h.mu.Lock()
	if len(h.messages) != 2 {
		t.Fatalf("notifications = %d, want report and approval: %v", len(h.messages), h.messages)
	}
	if !strings.Contains(h.messages[0]["body"].(string), "Was the $500 a deductible?") {
		t.Errorf("report notification lacks the question: %v", h.messages[0])
	}
	approval := h.messages[1]["link"].(string)
	h.mu.Unlock()
	if !strings.HasPrefix(approval, "http://orbit.test/ui/actions/") || !strings.Contains(approval, "sig=") {
		t.Fatalf("approval link = %q", approval)
	}

	// The auto action runs; the pending one waits for a decision.
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	if len(h.messages) != 3 || len(h.calls) != 0 {
		t.Fatalf("after auto: %d messages, %d endpoint calls", len(h.messages), len(h.calls))
	}
	h.mu.Unlock()
	pending, _ := s.ListActions(ctx, "finance", "pending", 0)
	if err := s.DecideAction(ctx, pending[0].ID, true, "test"); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var body map[string]any
	if len(h.calls) == 1 {
		_ = json.Unmarshal([]byte(h.calls[0]), &body)
	}
	if len(h.calls) != 1 || body["x"] != 1.0 {
		t.Fatalf("endpoint calls = %v", h.calls)
	}
}

func TestOutputOutsideTheContractFailsTheRun(t *testing.T) {
	fr := &fakeRunner{output: `{"summary": "x"}`}
	e, s, h := harness(t, fr)
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	run, _ := s.GetRun(ctx, c.ID)
	if run.Status != "failed" || !strings.Contains(*run.Error, "contract") {
		t.Fatalf("run = %s %v", run.Status, run.Error)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.messages) != 1 || !strings.Contains(h.messages[0]["title"].(string), "failed") {
		t.Fatalf("failure notification = %v", h.messages)
	}
	// The raw output is kept even though it was rejected.
	arts, _ := s.ListArtifacts(ctx, c.ID)
	found := false
	for _, a := range arts {
		found = found || a.Name == "output.json"
	}
	if !found {
		t.Fatal("rejected output was not stored")
	}
}

func TestBackendErrorFailsTheRun(t *testing.T) {
	fr := &fakeRunner{err: fmt.Errorf("%w: not logged in", runner.ErrBackend)}
	e, s, _ := harness(t, fr)
	c := claim(t, e, s)
	_ = e.Execute(ctx, c)
	run, _ := s.GetRun(ctx, c.ID)
	if run.Status != "failed" || !strings.Contains(*run.Error, "not logged in") {
		t.Fatalf("run = %s %v", run.Status, run.Error)
	}
}

func TestShutdownRequeuesTheRun(t *testing.T) {
	fr := &fakeRunner{output: goodOutput, block: make(chan struct{}), started: make(chan struct{})}
	e, s, h := harness(t, fr)
	c := claim(t, e, s)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { done <- e.Execute(runCtx, c) }()
	select {
	case <-fr.started:
	case err := <-done:
		t.Fatalf("finished before the runner started: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	run, _ := s.GetRun(ctx, c.ID)
	if run.Status != "queued" {
		t.Fatalf("status after shutdown = %s, want queued", run.Status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.messages) != 0 {
		t.Fatalf("a requeued run notified: %v", h.messages)
	}
}

func TestApprovalSignature(t *testing.T) {
	e := &engine.Engine{Secret: []byte("secret")}
	sig := e.ApprovalSig("a1")
	if !e.CheckApprovalSig("a1", sig) || e.CheckApprovalSig("a2", sig) || e.CheckApprovalSig("a1", "") {
		t.Fatal("signature must name exactly one action")
	}
}
