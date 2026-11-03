package engine_test

import (
	"bytes"
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

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

const txID = "0b6f2c1e-7d1a-4c3b-9e8f-2a5d6c7b8e9f"
const catID = "5f0e3a2b-1c4d-4e6f-8a9b-0c1d2e3f4a5b"

// A Sure-shaped categorize: the model names a transaction and a category, orbit sends PUT /api/v1/transactions/{id} with the key the credential binds to Sure's API, and nothing the model wrote reaches the URL outside its placeholder.
func TestWebhookPathFromCheckedParams(t *testing.T) {
	s := testdb.New(t)
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body bytes.Buffer
		_ = json.Compact(&body, b)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.EscapedPath()+" "+r.Header.Get("X-Api-Key")+" "+body.String())
		mu.Unlock()
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/transactions/") {
			fmt.Fprintf(w, `{"id":%q}`, strings.TrimPrefix(r.URL.Path, "/api/v1/transactions/"))
			return
		}
		http.NotFound(w, r)
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
defaults: {runner: fake, channels: [log]}
runners: {fake: {kind: claude-code}}
channels: {log: {kind: log}}
credentials:
  SURE_API_KEY: {url_prefix: "%[1]s/api/"}
endpoints:
  sure-categorize:
    url: %[1]s/api/v1
    method: PUT
    path: /transactions/{id}
    description: Set one transaction's category.
    params:
      id: {type: string, pattern: "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"}
    headers: {X-Api-Key: "${SURE_API_KEY}"}
    body_schema:
      type: object
      additionalProperties: false
      required: [transaction]
      properties:
        transaction:
          type: object
          additionalProperties: false
          required: [category_id]
          properties:
            category_id: {type: string, pattern: "^[0-9a-f-]{36}$"}
            user_modified: {const: true}
`, srv.URL))
	write("agents/finance.yaml", `
name: finance
charter: Categorize my transactions.
actions:
  webhook:sure-categorize: auto
`)
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := fmt.Sprintf(`{"summary": "categorized", "report": "r", "notify": {"send": false, "priority": "default", "title": ""},
  "memory": {"update": false, "content": ""}, "questions": [], "actions": [
    {"type": "webhook:sure-categorize", "params": {"path": {"id": %[1]q}, "body": {"transaction": {"category_id": %[2]q, "user_modified": true}}}, "reason": "groceries"},
    {"type": "webhook:sure-categorize", "params": {"path": {"id": "../accounts"}, "body": {"transaction": {"category_id": %[2]q}}}, "reason": "escape"},
    {"type": "webhook:sure-categorize", "params": {"path": {"id": %[1]q}, "body": {"transaction": {"name": "x"}}}, "reason": "rename"},
    {"type": "webhook:sure-categorize", "params": {"body": {"transaction": {"category_id": %[2]q}}}, "reason": "no id"}
  ]}`, txID, catID)
	fr := &fakeRunner{output: out}
	env := func(k string) (string, bool) {
		if k == "SURE_API_KEY" {
			return "sure-key", true
		}
		return "", false
	}
	e := &engine.Engine{
		Store: s, DataDir: t.TempDir(), ConfigDir: dir, Env: env, Client: srv.Client(), Secret: []byte("secret"),
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) { return fr, nil },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg.Agents["finance"])
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"finance": {SHA: cfg.Agents["finance"].SHA, JSON: b, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fr.got.System+fr.got.Prompt, "/transactions/{id}") {
		t.Error("the model is told the endpoint's path template")
	}
	if strings.Contains(fr.got.System+fr.got.Prompt, "sure-key") {
		t.Fatal("the credential reached the model")
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	acts, _ := s.ListActions(ctx, "finance", "", 0)
	byReason := map[string]store.Action{}
	for _, a := range acts {
		byReason[a.Reason] = a
	}
	for reason, want := range map[string]string{"escape": "does not match", "rename": "body does not match", "no id": "path.id is required"} {
		a := byReason[reason]
		if a.Status != "denied" || a.Error == nil || !strings.Contains(*a.Error, want) {
			t.Errorf("%s: %s %v, want denied with %q", reason, a.Status, a.Error, want)
		}
	}
	if a := byReason["groceries"]; a.Status != "executed" || !strings.Contains(string(a.Result), txID) {
		t.Fatalf("groceries: %s %s %v", a.Status, a.Result, a.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	want := "PUT /api/v1/transactions/" + txID + " sure-key " + `{"transaction":{"category_id":"` + catID + `","user_modified":true}}`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("requests = %q, want %q", got, want)
	}
}
