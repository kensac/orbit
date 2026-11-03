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
	orbitharness "github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// An agent with a prometheus source queries that Prometheus during its run through orbit: the tool reaches only the source's URL, with the source's credential, and one with no prometheus source has no tool.
func TestPrometheusQueryToolReadsTheAgentsSource(t *testing.T) {
	s := testdb.New(t)
	var seen []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" "+r.URL.Query().Get("query")+" "+r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"orbit"},"value":[1730548800,"1"]}]}}`)
	}))
	t.Cleanup(prom.Close)
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("orbit.yaml", `
timezone: America/New_York
defaults: {runner: fake, channels: [log]}
runners: {fake: {kind: claude-code}}
channels: {log: {kind: log}}
`)
	write("agents/ops.yaml", fmt.Sprintf(`
name: ops
charter: Watch the homelab.
sources:
  - {name: up, kind: prometheus, url: %q, expr: up}
`, prom.URL))
	write("agents/plain.yaml", "name: plain\ncharter: x\n")
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{output: repoOutput}
	var answers []string
	var offered []bool
	fr.seen = func(req runner.Request) {
		offered = append(offered, req.Harness != nil && req.Harness.Prometheus != "")
		if req.Harness == nil || req.Harness.Prometheus == "" {
			return
		}
		for _, args := range []string{`{"query":"sum(up)"}`, `{"query":"up","start":"now-1h","step":"1m"}`, `{"query":"up","source":"other"}`,
			`{"query":"up","url":"http://elsewhere/"}`, `{"query":"up","start":"now-40d"}`} {
			text, err := orbitharness.Prometheus(req.Harness.Prometheus, json.RawMessage(args))
			if err != nil {
				text = "error: " + err.Error()
			}
			answers = append(answers, text)
		}
	}
	e := &engine.Engine{
		Store: s, DataDir: t.TempDir(), ConfigDir: dir, Env: func(string) (string, bool) { return "", false }, Client: prom.Client(), Secret: []byte("secret"),
		Harness:   "/nonexistent/orbit",
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
	for _, agent := range []string{"ops", "plain"} {
		if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: agent, SpecSHA: cfg.Agents[agent].SHA, Trigger: "manual"}); err != nil {
			t.Fatal(err)
		}
		c, err := s.ClaimRun(ctx, "test")
		if err != nil || c == nil {
			t.Fatalf("claim: %v %v", c, err)
		}
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if len(offered) != 2 || !offered[0] || offered[1] {
		t.Fatalf("offered = %v: only the agent with a prometheus source has the tool", offered)
	}
	if len(answers) != 5 || !strings.Contains(answers[0], `"job":"orbit"`) || !strings.Contains(answers[1], `"resultType":"vector"`) ||
		!strings.Contains(answers[2], `"other" is not one of your prometheus sources (up)`) || !strings.Contains(answers[3], `unknown field "url"`) ||
		!strings.Contains(answers[4], "spans at most") {
		t.Fatalf("answers = %q", answers)
	}
	// One fetch of the source itself, then the two queries the bounds allowed.
	if len(seen) != 3 || seen[1] != "/api/v1/query sum(up) " || !strings.HasPrefix(seen[2], "/api/v1/query_range up") {
		t.Fatalf("prometheus saw %q", seen)
	}
}
