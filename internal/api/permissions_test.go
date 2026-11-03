package api_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

func TestConversationPermissionsOverTheAPI(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	// A chat agent that starts at ask and may be raised to edits.
	if err := os.WriteFile(filepath.Join(v.e.ConfigDir, "agents", "coder.yaml"), []byte("name: coder\ncharter: x\nchat: true\npermissions: {level: ask, ceiling: edits}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := spec.Load(v.e.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg.Agents["coder"])
	if err := v.s.SyncAgents(ctx, map[string]store.AgentSpec{"a": {SHA: cfg.Agents["a"].SHA, Enabled: true, JSON: []byte(`{}`)}, "coder": {SHA: cfg.Agents["coder"].SHA, JSON: b, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	code, r := call(t, "POST", base+"/agents/coder/conversations", kanishk, `{"wake":false}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, r)
	}
	id := r["conversation_id"].(string)
	url := base + "/conversations/" + id + "/permissions"

	code, r = call(t, "GET", url, kanishk, "")
	p := r["permissions"].(map[string]any)
	if code != 200 || p["level"] != "ask" || p["source"] != "agent" || p["ceiling"] != "edits" || p["current"] != nil ||
		len(p["levels"].([]any)) != 3 || len(p["history"].([]any)) != 0 {
		t.Fatalf("get = %d %v", code, r)
	}
	code, r = call(t, "PUT", url, kanishk, `{"level":"edits","expires_in":"1h","by":"mallory"}`)
	p = r["permissions"].(map[string]any)
	cur, _ := p["current"].(map[string]any)
	if code != 200 || p["level"] != "edits" || p["source"] != "conversation" || cur["version"] != float64(1) || cur["set_by"] != "kanishk" || cur["expires_at"] == nil {
		t.Fatalf("put = %d %v", code, r)
	}
	for body, want := range map[string]int{
		`{"level":"full"}`:                     400, // above the ceiling
		`{"level":"sudo"}`:                     400,
		`{}`:                                   400,
		`{"level":"ask","expires_in":"9000h"}`: 400,
		`{"level":null,"expires_in":"1h"}`:     400,
		`{"level":"ask","mode":"bypass"}`:      400,
	} {
		if code, r := call(t, "PUT", url, kanishk, body); code != want {
			t.Errorf("%s = %d %v, want %d", body, code, r, want)
		}
	}
	code, r = call(t, "PUT", url, kanishk, `{"level":null}`)
	if p := r["permissions"].(map[string]any); code != 200 || p["level"] != "ask" || p["source"] != "agent" {
		t.Fatalf("back to the agent's = %d %v", code, r)
	}
	_, r = call(t, "GET", url, kanishk, "")
	if h := r["permissions"].(map[string]any)["history"].([]any); len(h) != 2 || h[0].(map[string]any)["level"] != "" {
		t.Fatalf("history = %v", h)
	}
	// A report agent's conversation is locked: nothing above it can be set.
	_, r = call(t, "GET", base+"/agents/a/conversations", kanishk, "")
	def := r["conversations"].([]any)[0].(map[string]any)["id"].(string)
	if code, r := call(t, "PUT", base+"/conversations/"+def+"/permissions", kanishk, `{"level":"ask"}`); code != 400 {
		t.Fatalf("a report agent's = %d %v", code, r)
	}
	if code, _ := call(t, "PUT", base+"/conversations/00000000-0000-0000-0000-000000000000/permissions", kanishk, `{"level":"ask"}`); code != 404 {
		t.Fatalf("unknown conversation = %d", code)
	}
}
