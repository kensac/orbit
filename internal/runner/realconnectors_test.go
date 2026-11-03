package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/spec"
)

// TestRealCLIConnectors runs connectors against the real CLI: ORBIT_REAL_CLAUDE=<wrapper> go test -run RealCLIConnectors ./internal/runner/ (docs/agents/testing.md). It checks what the fakes cannot: a connector named with a dash is an SDK server whose tools the model calls as mcp__<name>__<tool>; an auto tool runs unasked; an ask tool reaches Permit with the tool use id its tools/call then carries, in a report run (--json-schema, stream-json for the control channel) and in a chat run; a denied one never reaches Call.
func TestRealCLIConnectors(t *testing.T) {
	bin := os.Getenv("ORBIT_REAL_CLAUDE")
	if bin == "" {
		t.Skip("ORBIT_REAL_CLAUDE is not set")
	}
	model := os.Getenv("ORBIT_REAL_CLAUDE_MODEL")
	if model == "" {
		model = "haiku"
	}
	r, err := newClaudeCode(spec.RunnerSpec{Kind: "claude-code", Binary: bin, Model: model},
		func(n string) (string, bool) {
			return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "placeholder"}[n], n == "CLAUDE_CODE_OAUTH_TOKEN"
		})
	if err != nil {
		t.Fatal(err)
	}
	type call struct{ tool, id string }
	var mu sync.Mutex
	var calls, permits []call
	var recorded int
	conns := func(allow bool) *Connectors {
		return &Connectors{
			Servers: []ConnectorServer{{Name: "housing-watch", Tools: []ConnectorTool{
				{Name: "find_listings", Description: "Find listings.", InputSchema: json.RawMessage(`{"type":"object"}`), ReadOnly: true},
				{Name: "create_view", Description: "Create a saved view.", InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`), Ask: true},
			}}},
			Call: func(_ context.Context, server, tool, id string, _ json.RawMessage) ConnectorResult {
				mu.Lock()
				calls = append(calls, call{tool, id})
				mu.Unlock()
				text := map[string]string{"find_listings": "3 listings: Elm, Oak, Pine", "create_view": "view created"}[tool]
				b, _ := json.Marshal([]map[string]any{{"type": "text", "text": text}})
				return ConnectorResult{Content: b}
			},
			Permit: func(_ context.Context, server, tool, id string, _ json.RawMessage) claudeproto.Permission {
				mu.Lock()
				permits = append(permits, call{tool, id})
				mu.Unlock()
				if allow {
					return claudeproto.Permission{Allow: true}
				}
				return claudeproto.Permission{Message: "Not run now: waiting for your person's approval."}
			},
			Record: func(claudeproto.PermissionRequest, claudeproto.Permission) error {
				mu.Lock()
				recorded++
				mu.Unlock()
				return nil
			},
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	prompt := "Call mcp__housing-watch__find_listings, then call mcp__housing-watch__create_view with name \"cheap\". Then answer with what each returned."

	dir, _ := filepath.EvalSymlinks(t.TempDir())
	res, err := r.Run(ctx, Request{Workdir: dir, System: "Answer briefly.", Prompt: prompt, Connectors: conns(true),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)}, func(Event) {})
	if err != nil {
		t.Fatalf("report run: %v", err)
	}
	if len(calls) != 2 || calls[0].tool != "find_listings" || calls[1].tool != "create_view" || len(permits) != 1 ||
		permits[0].id == "" || permits[0].id != calls[1].id || recorded != 1 {
		t.Fatalf("report run: calls %v, permits %v, recorded %d", calls, permits, recorded)
	}
	t.Logf("report output %s", res.Output)

	calls, permits, recorded = nil, nil, 0
	work, _ := filepath.EvalSymlinks(t.TempDir())
	res, err = r.Run(ctx, Request{Workdir: work, System: "Answer briefly.", Prompt: prompt, Connectors: conns(false),
		Chat: &Chat{}}, func(Event) {})
	if err != nil {
		t.Fatalf("chat run: %v", err)
	}
	if len(calls) != 1 || calls[0].tool != "find_listings" || len(permits) != 1 || permits[0].tool != "create_view" {
		t.Fatalf("chat run: calls %v, permits %v", calls, permits)
	}
	if !strings.Contains(strings.ToLower(res.Reply), "elm") {
		t.Fatalf("chat reply %q", res.Reply)
	}
	t.Logf("chat reply %q", res.Reply)
}
