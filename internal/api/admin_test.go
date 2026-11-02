package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// withTrigger reconfigures agent a with a daily trigger in timezone tz.
func withTrigger(t *testing.T, v *env, tz string) {
	t.Helper()
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(`
timezone: `+tz+`
public_url: http://orbit.test
defaults: {runner: r, channels: [c]}
runners: {r: {kind: claude-code}}
channels: {c: {kind: log}}
`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "a.yaml"), []byte("name: a\ncharter: x\ntriggers: [{schedule: '0 9 * * *'}]\n"), 0o644)
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg.Agents["a"])
	if err := v.s.SyncAgents(ctx, map[string]store.AgentSpec{"a": {SHA: cfg.Agents["a"].SHA, JSON: b, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
}

func TestUsage(t *testing.T) {
	v := newEnv(t, nil)
	withTrigger(t, v, "America/New_York")
	sha := v.e.Current().Config.Agents["a"].SHA
	id, err := v.s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: sha, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.s.ClaimRun(ctx, "w"); err != nil {
		t.Fatal(err)
	}
	if err := v.s.AppendEvent(ctx, id, 1, 1, "rate_limit_event", []byte(
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":4102444800,"rateLimitType":"seven_day","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"seven_day":{"utilization":0.14,"resetsAt":4102444800,"status":"allowed"}}}}`)); err != nil {
		t.Fatal(err)
	}
	cost := 0.017097
	if err := v.s.FinishRun(ctx, id, store.RunResult{Attempt: 1, Status: "succeeded", CostUSD: &cost}); err != nil {
		t.Fatal(err)
	}

	if code, _ := call(t, "GET", v.srv.URL+"/api/v1/usage", "", ""); code != 401 {
		t.Fatalf("no token = %d", code)
	}
	code, u := call(t, "GET", v.srv.URL+"/api/v1/usage", v.person("kanishk"), "")
	if code != 200 || u["timezone"] != "America/New_York" {
		t.Fatalf("usage = %d %v", code, u)
	}
	from, _ := time.Parse(time.DateOnly, u["from"].(string))
	to, _ := time.Parse(time.DateOnly, u["to"].(string))
	if to.Sub(from) != 29*24*time.Hour {
		t.Fatalf("default range = %v..%v", from, to)
	}
	sub := u["subscription"].(map[string]any)
	w := sub["windows"].([]any)[0].(map[string]any)
	if w["window"] != "seven_day" || w["utilization"] != 0.14 || w["resets_at"] != "2100-01-01T00:00:00Z" || w["run_id"] != id || w["expired"] != false {
		t.Fatalf("window = %v", w)
	}
	if o := sub["overage"].(map[string]any); o["status"] != "rejected" || o["disabled_reason"] != "org_level_disabled" {
		t.Fatalf("overage = %v", o)
	}
	if w["source"] != "stream" || w["status"] != "allowed" {
		t.Fatalf("window = %v, want the stream's reading", w)
	}
	if p := sub["poll"].(map[string]any); p["enabled"] != false || p["last_at"] != nil {
		t.Fatalf("poll = %v, want off and never polled", p)
	}
	if d := u["deferral"].(map[string]any); d["enabled"] != false || d["threshold"] != 0.8 || len(d["held"].([]any)) != 0 {
		t.Fatalf("deferral = %v", d)
	}

	// A poll newer than the stream's report wins its window, and names
	// itself; a failed poll after it keeps the reading and says why.
	if err := v.s.RecordUsageSample(ctx, "control", 200, json.RawMessage(
		`{"seven_day":{"utilization":31,"resets_at":"2100-01-01T00:00:00+00:00"},"five_hour":null}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := v.s.RecordUsageSample(ctx, "control", 500, nil, errors.New("HTTP 500: overloaded")); err != nil {
		t.Fatal(err)
	}
	_, u = call(t, "GET", v.internal.URL+"/api/v1/usage", token, "")
	sub = u["subscription"].(map[string]any)
	w = sub["windows"].([]any)[0].(map[string]any)
	if w["source"] != "poll" || w["utilization"] != 0.31 || w["status"] != nil || w["run_id"] != nil {
		t.Fatalf("window = %v, want the poll's reading", w)
	}
	if p := sub["poll"].(map[string]any); p["last_ok_at"] == nil || p["last_error"] != "HTTP 500: overloaded" {
		t.Fatalf("poll = %v", p)
	}
	c := u["cost"].(map[string]any)
	total := c["total"].(map[string]any)
	if total["api_equivalent_usd"] != 0.017097 || total["runs"] != 1.0 || total["by_status"].(map[string]any)["succeeded"] != 1.0 {
		t.Fatalf("total = %v", total)
	}
	if days := c["by_day"].([]any); len(days) != 30 || days[29].(map[string]any)["runs"] != 1.0 {
		t.Fatalf("by_day = %v", days)
	}
	if a := c["by_agent"].([]any); len(a) != 1 || a[0].(map[string]any)["agent"] != "a" {
		t.Fatalf("by_agent = %v", a)
	}

	code, u = call(t, "GET", v.internal.URL+"/api/v1/usage?from=2020-01-01&to=2020-01-03", token, "")
	if code != 200 || len(u["cost"].(map[string]any)["by_day"].([]any)) != 3 {
		t.Fatalf("explicit range = %d %v", code, u)
	}
	for _, q := range []string{"from=2020-01-03&to=2020-01-01", "from=2020-01-01&to=2022-01-01", "from=yesterday"} {
		if code, _ := call(t, "GET", v.internal.URL+"/api/v1/usage?"+q, token, ""); code != 400 {
			t.Fatalf("%s = %d, want 400", q, code)
		}
	}
}

func TestHealthAndAgentNextFires(t *testing.T) {
	cs := &engine.ConfigState{}
	v := newEnv(t, func(a *api.Server, _ *http.Server) { a.ConfigState = cs })
	withTrigger(t, v, "UTC")
	loaded := time.Now()
	cs.Loaded(loaded)
	cs.Failed(errors.New("agents/b.yaml: unknown field"), loaded.Add(time.Minute))

	code, h := call(t, "GET", v.srv.URL+"/api/v1/health", v.person("kanishk"), "")
	if code != 200 {
		t.Fatalf("health = %d %v", code, h)
	}
	cfg := h["config"].(map[string]any)
	if cfg["valid"] != false || cfg["last_error"] != "agents/b.yaml: unknown field" || cfg["loaded_at"] == nil {
		t.Fatalf("config = %v", cfg)
	}
	if q := h["queue"].(map[string]any); q["due"] != 0.0 || q["waiting_continuations"] != 0.0 {
		t.Fatalf("queue = %v", q)
	}
	if r := h["running"].([]any); len(r) != 0 || h["pending_actions"] != 0.0 {
		t.Fatalf("running = %v pending = %v", r, h["pending_actions"])
	}
	agents := h["agents"].([]any)
	a := agents[0].(map[string]any)
	fires := a["next_fires"].([]any)
	if len(agents) != 1 || a["name"] != "a" || a["last_success"] != nil || len(fires) != 1 {
		t.Fatalf("agents = %v", agents)
	}
	at, _ := time.Parse(time.RFC3339, fires[0].(map[string]any)["at"].(string))
	if at.Hour() != 9 || at.Minute() != 0 || !at.After(time.Now()) || time.Until(at) > 24*time.Hour {
		t.Fatalf("next fire = %v", at)
	}

	code, d := call(t, "GET", v.internal.URL+"/api/v1/agents/a", token, "")
	if code != 200 || len(d["next_fires"].([]any)) != 1 || d["agent"].(map[string]any)["spec"] == nil {
		t.Fatalf("agent detail = %d %v", code, d)
	}
	if hs := h["holds"].([]any); len(hs) != 0 || a["held"] != false {
		t.Fatalf("holds = %v, held = %v", hs, a["held"])
	}
	// A held runner is listed, and so is every agent on it.
	if _, err := v.s.OpenHold(ctx, store.NewHold{Runner: "r", Kind: store.HoldAuth, Reason: "Not logged in", FirstProbe: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, h = call(t, "GET", v.srv.URL+"/api/v1/health", v.person("kanishk"), "")
	hs := h["holds"].([]any)
	if len(hs) != 1 || hs[0].(map[string]any)["runner"] != "r" || hs[0].(map[string]any)["kind"] != "auth" || h["agents"].([]any)[0].(map[string]any)["held"] != true {
		t.Fatalf("holds = %v, agents = %v", hs, h["agents"])
	}
}
