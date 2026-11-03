package engine_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// A homelab-ops agent: its issues from GitHub as the App (a token per fetch, revoked), and its errors from Loki, both inputs it reads like any other.
func TestGitHubAndLokiSourcesAreInputs(t *testing.T) {
	s := testdb.New(t)
	gh := githubtest.New(t)
	gh.Issues = []map[string]any{{"number": 9, "title": "orbit restarts", "state": "open", "user": map[string]any{"login": "kanishk"},
		"created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-02T00:00:00Z", "body": "it restarts"}}
	var lokiQuery string
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lokiQuery = r.URL.Path + "?" + r.URL.Query().Get("query")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"container":"orbit"},"values":[["1730550000000000000","level=ERROR msg=boom"]]}],"stats":{}}}`)
	}))
	t.Cleanup(loki.Close)
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
github:
  app_id: %d
  private_key_env: GH_KEY
  api_url: %s
  repositories: [kensac/elsewhere]
  read_repositories: [kensac/orbit]
`, githubtest.AppID, gh.URL()))
	write("agents/ops.yaml", fmt.Sprintf(`
name: ops
charter: Watch the homelab.
needs: [github]
sources:
  - {name: issues, kind: github, repo: kensac/orbit, resource: issues}
  - {name: errors, kind: loki, url: %q, expr: '{container="orbit"} |= "ERROR"', since: 2h, limit: 50}
`, loki.URL))
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) { return githubtest.KeyPEM(), k == "GH_KEY" }
	fr := &fakeRunner{output: repoOutput}
	e := &engine.Engine{
		Store: s, DataDir: t.TempDir(), ConfigDir: dir, Env: env, Client: gh.Srv.Client(), Secret: []byte("secret"),
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) { return fr, nil },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg.Agents["ops"])
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"ops": {SHA: cfg.Agents["ops"].SHA, JSON: b, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "ops", SpecSHA: cfg.Agents["ops"].SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	run, _ := s.GetRun(ctx, c.ID)
	if run.Status != "succeeded" {
		t.Fatalf("run = %s %v", run.Status, run.Error)
	}
	in := map[string]string{}
	for _, i := range fr.got.Inputs {
		in[i.Name] = string(i.Content)
	}
	if !strings.Contains(in["issues"], `"title":"orbit restarts"`) || !strings.Contains(in["errors"], "msg=boom") || strings.Contains(in["errors"], "stats") {
		t.Fatalf("inputs = %v", in)
	}
	if lokiQuery != `/loki/api/v1/query_range?{container="orbit"} |= "ERROR"` {
		t.Fatalf("loki got %s", lokiQuery)
	}
	gh.Mu.Lock()
	defer gh.Mu.Unlock()
	perms, _ := json.Marshal(gh.TokenReq["permissions"])
	if string(perms) != `{"issues":"read"}` || len(gh.Revoked) != 1 {
		t.Fatalf("token %s, revoked %d", perms, len(gh.Revoked))
	}
	if strings.Contains(fr.got.Prompt+fr.got.System, githubtest.Token) {
		t.Fatal("the token reached the model")
	}
}
