package runner

import (
	"slices"
	"strings"
	"testing"
)

func flag(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestWebAllowBecomesDomainRules(t *testing.T) {
	c := &claudeCode{bin: "claude"}
	web := []string{"example.com", "*.wikipedia.org"}
	check := func(name string, args []string) {
		t.Helper()
		allowed := strings.Split(flag(args, "--allowedTools"), ",")
		if slices.Contains(allowed, "WebFetch") || !slices.Contains(allowed, "WebFetch(domain:example.com)") || !slices.Contains(allowed, "WebFetch(domain:*.wikipedia.org)") || !slices.Contains(allowed, "WebSearch") {
			t.Errorf("%s: --allowedTools %v", name, allowed)
		}
		if d := flag(args, "--disallowedTools"); !strings.Contains(d, "WebFetch(domain:localhost)") || !strings.Contains(d, "WebFetch(domain:*.ts.net)") || !strings.Contains(d, "WebFetch(domain:169.254.169.254)") {
			t.Errorf("%s: --disallowedTools %q", name, d)
		}
		if !slices.Contains(strings.Split(flag(args, "--tools"), ","), "WebFetch") {
			t.Errorf("%s: WebFetch must stay a tool: %q", name, flag(args, "--tools"))
		}
	}
	tools := []string{"WebSearch", "WebFetch"}
	check("report", c.baseArgs(Request{Tools: tools, WebAllow: web, OutputSchema: []byte(`{}`)}, t.TempDir(), 1))
	check("chat", c.chatArgs(Request{Tools: tools, WebAllow: web, Chat: &Chat{}}, t.TempDir(), 1))

	plain := c.baseArgs(Request{Tools: tools, OutputSchema: []byte(`{}`)}, t.TempDir(), 1)
	if !strings.Contains(flag(plain, "--allowedTools"), "WebFetch") || slices.Contains(plain, "--disallowedTools") {
		t.Errorf("without web.allow nothing changes: %v", plain)
	}
	sandboxed := c.chatArgs(Request{Tools: tools, WebAllow: web, Chat: &Chat{Sandbox: &Sandbox{Profile: "build"}}}, t.TempDir(), 1)
	if slices.Contains(sandboxed, "--disallowedTools") || strings.Contains(flag(sandboxed, "--allowedTools"), "domain:") {
		t.Errorf("a sandboxed run's network is its egress: %v", sandboxed)
	}
}
