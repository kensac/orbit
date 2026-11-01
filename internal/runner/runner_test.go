package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// fakeCLI writes an executable script that records its argv, environment
// and stdin under dir, then prints body.
func fakeCLI(t *testing.T, body string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "fake")
	script := "#!/bin/sh\n" +
		`printf '%s\n' "$@" > "` + dir + `/argv"` + "\n" +
		`env > "` + dir + `/env"` + "\n" +
		`cat > "` + dir + `/stdin"` + "\n" +
		body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var orbitEnv = lookup(map[string]string{
	"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-test",
	"SURE_API_KEY":            "source-secret",
	"ORBIT_API_TOKEN":         "api-secret",
})

func TestClaudeCodeRun(t *testing.T) {
	bin, dir := fakeCLI(t, `echo '{"type":"system","subtype":"init"}'
echo '{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","structured_output":{"summary":"ok"},"total_cost_usd":0.5,"session_id":"s1","num_turns":2}'`)
	r, err := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin, Model: "m1", MaxTurns: 7}, orbitEnv)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	res, err := r.Run(context.Background(), Request{
		Workdir: t.TempDir(), System: "contract", Prompt: "the prompt", Tools: []string{"WebFetch"},
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(e Event) { kinds = append(kinds, e.Kind) })
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `{"summary":"ok"}` || *res.CostUSD != 0.5 || res.SessionID != "s1" {
		t.Fatalf("result = %+v", res)
	}
	if strings.Join(kinds, ",") != "system.init,result.success" {
		t.Fatalf("events = %v", kinds)
	}
	argv := read(t, filepath.Join(dir, "argv"))
	for _, want := range []string{"--tools\nRead,Glob,Grep,WebFetch\n", "--allowedTools\nWebFetch\n", "--model\nm1\n", "--max-turns\n7\n", "--strict-mcp-config", "--json-schema"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q:\n%s", want, argv)
		}
	}
	if read(t, filepath.Join(dir, "stdin")) != "the prompt" {
		t.Error("prompt was not sent on stdin")
	}
	env := read(t, filepath.Join(dir, "env"))
	if !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-test") {
		t.Error("the backend's own credential is missing")
	}
	if strings.Contains(env, "source-secret") || strings.Contains(env, "api-secret") {
		t.Fatal("orbit's other secrets reached the model's process")
	}
}

func TestClaudeCodeReportsLoginFailure(t *testing.T) {
	// A failed login is subtype "success" with is_error set.
	bin, _ := fakeCLI(t, `echo '{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"Not logged in"}'`)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	_, err := r.Run(context.Background(), Request{Workdir: t.TempDir(), OutputSchema: json.RawMessage(`{}`)}, func(Event) {})
	if !errors.Is(err, ErrBackend) || !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("err = %v", err)
	}
}

func TestClaudeCodeWithoutResult(t *testing.T) {
	bin, _ := fakeCLI(t, `echo "boom" >&2; exit 3`)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	_, err := r.Run(context.Background(), Request{Workdir: t.TempDir(), OutputSchema: json.RawMessage(`{}`)}, func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the stderr tail", err)
	}
}

func TestClaudeCodeTimeoutKillsTheProcessGroup(t *testing.T) {
	bin, _ := fakeCLI(t, `sleep 30 & wait`)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, Request{Workdir: t.TempDir(), OutputSchema: json.RawMessage(`{}`)}, func(Event) {})
	if err == nil || time.Since(start) > 15*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

func TestClaudeCodeNeedsACredential(t *testing.T) {
	if _, err := New("c", spec.RunnerSpec{Kind: "claude-code"}, lookup(nil)); err == nil {
		t.Fatal("a runner without a credential must fail to build")
	}
}

func TestCodexRun(t *testing.T) {
	bin, dir := fakeCLI(t, `out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
echo '{"type":"thread.started","thread_id":"t1"}'
echo '{"type":"turn.completed","usage":{"input_tokens":5}}'
printf '{"summary":"ok"}' > "$out"`)
	r, err := New("x", spec.RunnerSpec{Kind: "codex", Binary: bin, Model: "gpt"}, lookup(map[string]string{"OPENAI_API_KEY": "k", "SURE_API_KEY": "source-secret"}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), Request{Workdir: t.TempDir(), System: "contract", Prompt: "p", OutputSchema: json.RawMessage(`{}`)}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `{"summary":"ok"}` || res.SessionID != "t1" || string(res.Usage) != `{"input_tokens":5}` {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(read(t, filepath.Join(dir, "argv")), "read-only") {
		t.Error("codex must run with the read-only sandbox")
	}
	if strings.Contains(read(t, filepath.Join(dir, "env")), "source-secret") {
		t.Fatal("orbit's other secrets reached codex")
	}
}

func TestCodexTurnFailure(t *testing.T) {
	bin, _ := fakeCLI(t, `echo '{"type":"turn.failed","error":{"message":"model not supported"}}'`)
	r, _ := New("x", spec.RunnerSpec{Kind: "codex", Binary: bin}, lookup(map[string]string{"OPENAI_API_KEY": "k"}))
	_, err := r.Run(context.Background(), Request{Workdir: t.TempDir(), OutputSchema: json.RawMessage(`{}`)}, func(Event) {})
	if !errors.Is(err, ErrBackend) || !strings.Contains(err.Error(), "model not supported") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnthropicRun(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "ak" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"id":"msg_1","model":"m","usage":{"input_tokens":3},"stop_reason":"tool_use",
			"content":[{"type":"tool_use","name":"submit","input":{"summary":"ok"}}]}`))
	}))
	defer srv.Close()
	t.Setenv("ORBIT_ANTHROPIC_URL", srv.URL)
	r, err := New("a", spec.RunnerSpec{Kind: "anthropic", Model: "m", APIKeyEnv: "K"}, lookup(map[string]string{"K": "ak"}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), Request{
		System: "contract", Prompt: "p", OutputSchema: json.RawMessage(`{"type":"object"}`),
		Inputs: []Input{{Name: "tx", Path: "inputs/tx.json", MediaType: "application/json", Content: []byte(`[1]`)}},
	}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `{"summary":"ok"}` {
		t.Fatalf("output = %s", res.Output)
	}
	content := got["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, `<input name="tx"`) {
		t.Fatalf("inputs were not inlined: %s", content)
	}
	if got["tool_choice"].(map[string]any)["name"] != "submit" {
		t.Fatal("the submit tool must be forced")
	}
}
