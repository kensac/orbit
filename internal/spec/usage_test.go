package spec

import (
	"strings"
	"testing"
	"time"
)

func TestUsagePollAndPriority(t *testing.T) {
	cfg, err := Load(write(t, map[string]string{
		"orbit.yaml":    base + "usage_poll: {}\ndefer_low_priority: true\n",
		"agents/a.yaml": "name: a\ncharter: x\npriority: low\n",
		"agents/b.yaml": "name: b\ncharter: x\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Deployment.UsagePoll
	if p == nil || time.Duration(p.Every) != DefaultUsagePollEvery || p.TokenEnv != "CLAUDE_CODE_OAUTH_TOKEN" || p.URL != DefaultUsageURL ||
		!cfg.Deployment.DeferLowPriority {
		t.Fatalf("usage_poll = %+v", p)
	}
	if !cfg.Agents["a"].Low() || cfg.Agents["b"].Low() || cfg.Agents["b"].Priority != "" {
		t.Fatal("priority: a is low, b normal and left unset (so its hash does not change)")
	}

	_, err = Load(write(t, map[string]string{
		"orbit.yaml":    base + "usage_poll: {every: 10s, url: 'ftp://x'}\n",
		"agents/a.yaml": "name: a\ncharter: x\npriority: urgent\n",
	}))
	for _, want := range []string{"every is 10s", "not an http(s) URL", `priority "urgent"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v lacks %q", err, want)
		}
	}
}
