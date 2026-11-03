package api_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// A synthetic session; nothing here is from a real transcript.
const apiSession = "55555555-5555-4555-8555-555555555555"

func TestSessionImportAPI(t *testing.T) {
	v := newEnv(t, nil)
	pub, tok := v.srv.URL+"/api/v1", v.person("kanishk")
	line1 := `{"type":"user","sessionId":"` + apiSession + `","uuid":"u1","cwd":"/home/p/widget","timestamp":"2026-10-01T10:00:00Z","message":{"role":"user","content":"Why does the gizmo export fail? my DB_PASSWORD=hunter2hunter2"}}`
	line2 := `{"type":"assistant","sessionId":"` + apiSession + `","uuid":"a1","parentUuid":"u1","timestamp":"2026-10-01T10:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"The gizmo exporter reads a closed file."}]}}`
	body := `{"sessions":[{"session_id":"` + apiSession + `","repo":"someone/widget","from_offset":0,"from_prefix_sha256":"",
		"to_offset":400,"to_prefix_sha256":"` + strings.Repeat("a", 64) + `","lines":[` + line1 + `,` + line2 + `]}]}`
	code, out := call(t, "POST", pub+"/sessions/import", tok, body)
	if code != 200 || out["dry_run"] != false {
		t.Fatalf("import = %d %v", code, out)
	}
	r := out["results"].([]any)[0].(map[string]any)
	if r["outcome"] != "created" || r["added"].(float64) != 2 {
		t.Fatalf("result = %v", r)
	}
	id := r["id"].(string)

	code, out = call(t, "GET", pub+"/sessions?source=claude-code", tok, "")
	if code != 200 || len(out["sessions"].([]any)) != 1 || out["has_more"] != false {
		t.Fatalf("list = %d %v", code, out)
	}
	sess := out["sessions"].([]any)[0].(map[string]any)
	if sess["session_id"] != apiSession || sess["offset"].(float64) != 400 || sess["repo"] != "someone/widget" ||
		!strings.HasPrefix(sess["first_prompt"].(string), "Why does the gizmo export fail?") {
		t.Fatalf("session = %v", sess)
	}
	if code, _ := call(t, "GET", pub+"/sessions/"+id, tok, ""); code != 200 {
		t.Fatalf("get = %d", code)
	}
	code, out = call(t, "GET", pub+"/sessions/"+id+"/lines?after=0&limit=10", tok, "")
	lines := out["lines"].([]any)
	if code != 200 || len(lines) != 2 {
		t.Fatalf("lines = %d %v", code, out)
	}
	first := lines[0].(map[string]any)["line"].(map[string]any)
	if _, ok := first["sessionId"]; ok || strings.Contains(first["message"].(map[string]any)["content"].(string), "hunter2") {
		t.Fatalf("stored line keeps its session id or a credential: %v", first)
	}

	code, out = call(t, "GET", pub+"/search?q=closed+file&kind=session", tok, "")
	if code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("search = %d %v", code, out)
	}
	hit := out["results"].([]any)[0].(map[string]any)
	if hit["external_session_id"] != id || hit["links"].(map[string]any)["session"] != "/api/v1/sessions/"+id {
		t.Fatalf("hit = %v", hit)
	}

	if code, _ := call(t, "POST", pub+"/sessions/import", tok, `{"sessions":[{"session_id":"`+apiSession+`","to_offset":1,"to_prefix_sha256":"`+strings.Repeat("a", 64)+`","lines":["not an object"]}]}`); code != 400 {
		t.Fatalf("a line that is not an object = %d", code)
	}
	if code, _ := call(t, "GET", pub+"/sessions/"+strings.Repeat("0", 8)+"-0000-4000-8000-000000000000", tok, ""); code != 404 {
		t.Fatalf("unknown session = %d", code)
	}
}

func TestAConversationStartsFromAnImportedSession(t *testing.T) {
	v := newEnv(t, nil)
	pub, tok := v.srv.URL+"/api/v1", v.person("kanishk")
	// An agent that resumes sessions, beside a, which does not.
	if err := os.WriteFile(filepath.Join(v.e.ConfigDir, "agents", "chat.yaml"), []byte("name: chat\ncharter: x\nsession: {resume: true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := spec.Load(v.e.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	specs := map[string]store.AgentSpec{}
	for _, name := range []string{"a", "chat"} {
		b, _ := json.Marshal(cfg.Agents[name])
		specs[name] = store.AgentSpec{SHA: cfg.Agents[name].SHA, JSON: b, Enabled: true}
	}
	if err := v.s.SyncAgents(ctx, specs); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","sessionId":"` + apiSession + `","uuid":"u1","message":{"role":"user","content":"Plan the gizmo release"}}`
	code, out := call(t, "POST", pub+"/sessions/import", tok, `{"sessions":[{"session_id":"`+apiSession+`","from_offset":0,"from_prefix_sha256":"","to_offset":120,"to_prefix_sha256":"`+strings.Repeat("a", 64)+`","lines":[`+line+`]}]}`)
	if code != 200 {
		t.Fatalf("import = %d %v", code, out)
	}
	id := out["results"].([]any)[0].(map[string]any)["id"].(string)

	// chat resumes sessions; a does not.
	if code, out := call(t, "POST", pub+"/conversations", tok, `{"agent":"a","from_session":"`+apiSession+`"}`); code != 400 || !strings.Contains(out["error"].(string), "does not resume") {
		t.Fatalf("from a non-session agent = %d %v", code, out)
	}
	if code, _ := call(t, "POST", pub+"/conversations", tok, `{"agent":"chat","from_session":"99999999-9999-4999-8999-999999999999"}`); code != 404 {
		t.Fatalf("from an unknown session = %d", code)
	}
	code, out = call(t, "POST", pub+"/conversations", tok, `{"agent":"chat","from_session":"`+apiSession+`","body":"Continue it here.","wake":false}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, out)
	}
	code, out = call(t, "GET", pub+"/conversations/"+out["conversation_id"].(string), tok, "")
	c := out["conversation"].(map[string]any)
	if code != 200 || c["from_session"] != id || c["title"] != "Continue it here." {
		t.Fatalf("conversation = %d %v", code, c)
	}
	// Without a body, the session names it.
	code, out = call(t, "POST", pub+"/conversations", tok, `{"agent":"chat","from_session":"`+id+`"}`)
	if code != 201 {
		t.Fatalf("create by orbit's id = %d %v", code, out)
	}
	_, out = call(t, "GET", pub+"/conversations/"+out["conversation_id"].(string), tok, "")
	if c := out["conversation"].(map[string]any); c["title"] != "Plan the gizmo release" {
		t.Fatalf("conversation = %v", c)
	}
}
