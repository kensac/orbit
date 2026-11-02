package api_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

const hookSecret = "It's a Secret to Everybody"

// hookServer is the public listener with one agent, triage, whose issues trigger is a GitHub webhook of at most 64 bytes.
func hookServer(t *testing.T, enabled bool) (*httptest.Server, *store.Store, *int) {
	t.Helper()
	s := testdb.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(`
defaults: {runner: r, channels: [c]}
runners: {r: {kind: claude-code}}
channels: {c: {kind: log}}
webhook_secrets:
  HOOK_GH: {scheme: github, agents: [triage]}
`), 0o644)
	agent := "name: triage\ncharter: x\ntriggers: [{webhook: issues, secret: HOOK_GH, max_bytes: 64, prompt: An issue changed.}]\n"
	if !enabled {
		agent += "enabled: false\n"
	}
	_ = os.WriteFile(filepath.Join(dir, "agents", "triage.yaml"), []byte(agent), 0o644)
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &engine.Engine{Store: s, ConfigDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Env: func(k string) (string, bool) { return hookSecret, k == "HOOK_GH" }}
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg.Agents["triage"])
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"triage": {SHA: cfg.Agents["triage"].SHA, JSON: b, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	wakes := 0
	a := &api.Server{Store: s, Engine: e, Wake: func() { wakes++ }, Stop: make(chan struct{}), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv, s, &wakes
}

func signedHook(t *testing.T, url, delivery, event, body, secret string) (int, map[string]any) {
	t.Helper()
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	req.Header.Set("X-GitHub-Event", event)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

// TestAWebhookStartsOneRunPerDelivery: a delivery signed with the trigger's secret starts a webhook run, with no token and no forward-auth; a redelivery answers the first run; a bad signature, a missing delivery id, an oversized body, an unknown route and a disabled agent start nothing.
func TestAWebhookStartsOneRunPerDelivery(t *testing.T) {
	srv, s, wakes := hookServer(t, true)
	url := srv.URL + "/hooks/triage/issues"
	body := `{"action":"opened"}`

	code, v := signedHook(t, url, "g-1", "issues", body, hookSecret)
	if code != 202 || v["duplicate"] != false || v["run_id"] == "" || *wakes != 1 {
		t.Fatalf("delivery = %d %v, wakes %d", code, v, *wakes)
	}
	runID := v["run_id"].(string)
	r, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Trigger != store.TriggerWebhook || r.TriggerIndex == nil || *r.TriggerIndex != 0 || r.Status != "queued" ||
		r.Prompt != "An issue changed.\n\nWebhook issues: delivery g-1, event issues." {
		t.Fatalf("run = %+v", r)
	}
	d, err := s.WebhookForRun(ctx, runID)
	if err != nil || d == nil || string(d.Body) != body || d.MediaType != "application/json" || d.Event != "issues" || d.DeliveryID != "g-1" {
		t.Fatalf("delivery = %+v %v", d, err)
	}

	code, v = signedHook(t, url, "g-1", "issues", body, hookSecret)
	if code != 200 || v["duplicate"] != true || v["run_id"] != runID || *wakes != 1 {
		t.Fatalf("redelivery = %d %v: it answers the first run and starts nothing", code, v)
	}
	if code, v := signedHook(t, url, "g-1", "ping", `{"zen":"x"}`, hookSecret); code != 200 || v["ping"] != true {
		t.Fatalf("ping = %d %v", code, v)
	}
	for name, c := range map[string]struct {
		url, delivery, body, secret string
		want                        int
	}{
		"wrong secret":   {url, "g-2", body, "guess", 401},
		"no delivery id": {url, "", body, hookSecret, 400},
		"too large":      {url, "g-3", `{"pad":"` + strings.Repeat("x", 64) + `"}`, hookSecret, 413},
		"unknown agent":  {srv.URL + "/hooks/nobody/issues", "g-4", body, hookSecret, 404},
		"unknown hook":   {srv.URL + "/hooks/triage/pushes", "g-5", body, hookSecret, 404},
	} {
		if code, v := signedHook(t, c.url, c.delivery, "issues", c.body, c.secret); code != c.want {
			t.Errorf("%s = %d %v, want %d", name, code, v, c.want)
		}
	}
	runs, err := s.ListRuns(ctx, store.RunFilter{Agent: "triage"})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %d %v: only the one delivery started a run", len(runs), err)
	}
}

func TestADisabledAgentRefusesWebhooks(t *testing.T) {
	srv, s, _ := hookServer(t, false)
	if code, v := signedHook(t, srv.URL+"/hooks/triage/issues", "g-1", "issues", `{}`, hookSecret); code != 409 {
		t.Fatalf("disabled = %d %v", code, v)
	}
	if runs, _ := s.ListRuns(ctx, store.RunFilter{Agent: "triage"}); len(runs) != 0 {
		t.Fatalf("a disabled agent's webhook started %d runs", len(runs))
	}
}
