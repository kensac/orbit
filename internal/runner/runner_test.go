package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

func TestClaudeCodeWithholdsTheAPIKeyBesideASubscriptionToken(t *testing.T) {
	bin, dir := fakeCLI(t, `echo '{"type":"result","subtype":"success","is_error":false,"structured_output":{}}'`)
	both := lookup(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-test", "ANTHROPIC_API_KEY": "sk-ant-api-stray"})
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, both)
	work := t.TempDir()
	if _, err := r.Run(context.Background(), Request{Workdir: work, OutputSchema: json.RawMessage(`{}`)}, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	env := read(t, filepath.Join(dir, "env"))
	if strings.Contains(env, "sk-ant-api-stray") || !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-test") {
		t.Fatalf("env must hold the subscription token only:\n%s", env)
	}
	if !strings.Contains(env, "CLAUDE_CONFIG_DIR="+filepath.Join(work, ".home", ".claude")+"\n") {
		t.Fatalf("CLAUDE_CONFIG_DIR is not pinned under HOME:\n%s", env)
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

func TestClaudeCodeWithoutSessionIsNotPersisted(t *testing.T) {
	bin, dir := fakeCLI(t, `echo '{"type":"result","subtype":"success","is_error":false,"structured_output":{},"session_id":"s1"}'`)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	if _, err := r.Run(context.Background(), Request{Workdir: t.TempDir(), OutputSchema: json.RawMessage(`{}`)}, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	argv := read(t, filepath.Join(dir, "argv"))
	if !strings.Contains(argv, "--no-session-persistence") || strings.Contains(argv, "--resume") {
		t.Fatalf("a run without a session must not persist one:\n%s", argv)
	}
	if !strings.Contains(argv, `"autoMemoryEnabled":false`) || !strings.Contains(argv, `"cleanupPeriodDays":1`) {
		t.Fatalf("settings lack auto memory off or retention:\n%s", argv)
	}
}

// started records the mode a runner reported through Session.Started.
type started struct{ mode, parent string }

func session(home string, resume string, base float64, got *started) *Session {
	return &Session{Home: home, Resume: resume, BaseCostUSD: base, KeepDays: 2, Key: "a",
		Started: func(mode, parent string) error { *got = started{mode, parent}; return nil }}
}

func TestClaudeCodeResumesAForkOfTheSession(t *testing.T) {
	bin, dir := fakeCLI(t, `echo '{"type":"result","subtype":"success","is_error":false,"structured_output":{"summary":"ok"},"total_cost_usd":0.75,"session_id":"s2"}'`)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	home := t.TempDir()
	var got started
	res, err := r.Run(context.Background(), Request{Workdir: t.TempDir(), Prompt: "p", OutputSchema: json.RawMessage(`{}`),
		Session: session(home, "s1", 0.5, &got)}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	argv := read(t, filepath.Join(dir, "argv"))
	if !strings.Contains(argv, "--resume\ns1\n--fork-session\n") || strings.Contains(argv, "--no-session-persistence") {
		t.Fatalf("argv:\n%s", argv)
	}
	if !strings.Contains(argv, `"cleanupPeriodDays":2`) {
		t.Fatalf("retention must cover the resume window:\n%s", argv)
	}
	if got != (started{"resumed", "s1"}) {
		t.Fatalf("started = %+v", got)
	}
	// The CLI's total is cumulative over the session; the run is charged
	// its own share.
	if *res.CostUSD != 0.25 || *res.SessionCostUSD != 0.75 || res.SessionID != "s2" {
		t.Fatalf("cost = %v of %v, session %s", *res.CostUSD, *res.SessionCostUSD, res.SessionID)
	}
	if env := read(t, filepath.Join(dir, "env")); !strings.Contains(env, "HOME="+home+"\n") || strings.Contains(env, "source-secret") {
		t.Fatalf("env:\n%s", env)
	}
}

// liveCLI is a fake `claude --input-format stream-json`: it counts process
// starts, and answers each stdin line with an init event and a result whose
// structured output numbers the turn. onTurn runs before the answer, with
// $n the turn number; it may exit or hang to simulate a crash.
func liveCLI(t *testing.T, onTurn string) (bin, dir string) {
	t.Helper()
	return fakeCLI(t, `echo start >> "$(dirname "$0")/starts"
echo $$ >> "$(dirname "$0")/pids"
trap 'echo exited >> "$(dirname "$0")/exits"' EXIT
n=0
while IFS= read -r line; do
  n=$((n+1))
  printf '%s\n' "$line" >> "$(dirname "$0")/turns"
  `+onTurn+`
  echo '{"type":"system","subtype":"init"}'
  echo '{"type":"result","subtype":"success","is_error":false,"structured_output":{"n":'$n'},"total_cost_usd":'$n',"session_id":"live-1"}'
done`)
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestClaudeCodeLiveProcessTakesFollowUps(t *testing.T) {
	// fakeCLI's script reads all of stdin before its body; a live CLI
	// must read line by line, so this one skips that.
	bin, dir := liveCLI(t, "")
	stripStdinCapture(t, bin)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	live := NewLive(MaxLive)
	defer live.Close()
	home, work := t.TempDir(), t.TempDir()
	run := func(resume string, base float64, prompt string) (*Result, started) {
		t.Helper()
		var got started
		s := session(home, resume, base, &got)
		s.Live, s.Open, s.LiveIdle = live, true, time.Minute
		res, err := r.Run(context.Background(), Request{Workdir: work, Prompt: prompt, OutputSchema: json.RawMessage(`{}`), Session: s}, func(Event) {})
		if err != nil {
			t.Fatal(err)
		}
		return res, got
	}
	res, got := run("", 0, "first")
	if string(res.Output) != `{"n":1}` || got.mode != "fresh" || res.SessionID != "live-1" {
		t.Fatalf("turn 1: %s %+v", res.Output, got)
	}
	res, got = run("live-1", 1, "second")
	if string(res.Output) != `{"n":2}` || got != (started{"live", "live-1"}) || *res.CostUSD != 1 {
		t.Fatalf("turn 2: %s %+v cost %v", res.Output, got, *res.CostUSD)
	}
	if n := len(lines(t, filepath.Join(dir, "starts"))); n != 1 {
		t.Fatalf("process starts = %d, want the follow-up fed to the same one", n)
	}
	turns := lines(t, filepath.Join(dir, "turns"))
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if len(turns) != 2 || json.Unmarshal([]byte(turns[1]), &msg) != nil || msg.Type != "user" || msg.Message.Content != "second" {
		t.Fatalf("stdin = %v", turns)
	}
	argv := read(t, filepath.Join(dir, "argv"))
	if !strings.Contains(argv, "--input-format\nstream-json\n") {
		t.Fatalf("argv:\n%s", argv)
	}
	// A run that must not continue live-1 closes the process and gets one
	// of its own.
	_, got = run("", 0, "third")
	if got.mode != "fresh" || len(lines(t, filepath.Join(dir, "starts"))) != 2 {
		t.Fatalf("after a fresh run: %+v, starts %d", got, len(lines(t, filepath.Join(dir, "starts"))))
	}
}

// stripStdinCapture removes fakeCLI's `cat > stdin`, which would swallow a
// live process's input.
func stripStdinCapture(t *testing.T, bin string) {
	t.Helper()
	b := read(t, bin)
	var keep []string
	for _, l := range strings.Split(b, "\n") {
		if !strings.HasPrefix(l, "cat > ") {
			keep = append(keep, l)
		}
	}
	if err := os.WriteFile(bin, []byte(strings.Join(keep, "\n")), 0o755); err != nil {
		t.Fatal(err)
	}
}

func liveRun(t *testing.T, r Runner, live *Live, ctx context.Context, resume, home, work string, idle time.Duration) (*Result, error) {
	t.Helper()
	var got started
	s := session(home, resume, 0, &got)
	s.Live, s.Open, s.LiveIdle = live, true, idle
	return r.Run(ctx, Request{Workdir: work, Prompt: "p", OutputSchema: json.RawMessage(`{}`), Session: s}, func(Event) {})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestClaudeCodeLiveProcessClosesWhenIdle(t *testing.T) {
	bin, dir := liveCLI(t, "")
	stripStdinCapture(t, bin)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	live := NewLive(MaxLive)
	defer live.Close()
	if _, err := liveRun(t, r, live, context.Background(), "", t.TempDir(), t.TempDir(), 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the idle process to exit", func() bool { return len(lines(t, filepath.Join(dir, "exits"))) == 1 })
	if live.Len() != 0 {
		t.Fatal("an expired process is still held")
	}
}

func TestClaudeCodeLiveProcessCrashFailsTheTurn(t *testing.T) {
	bin, dir := liveCLI(t, `if [ $n -eq 2 ]; then echo '{"type":"assistant"}'; echo "segfault" >&2; exit 3; fi`)
	stripStdinCapture(t, bin)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	live := NewLive(MaxLive)
	defer live.Close()
	home, work := t.TempDir(), t.TempDir()
	if _, err := liveRun(t, r, live, context.Background(), "", home, work, time.Minute); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := liveRun(t, r, live, context.Background(), "live-1", home, work, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "exited during the turn") || !strings.Contains(err.Error(), "segfault") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second || live.Len() != 0 {
		t.Fatalf("crash took %s, %d processes held", time.Since(start), live.Len())
	}
	// The next run starts a process of its own rather than reusing the dead one.
	if _, err := liveRun(t, r, live, context.Background(), "live-1", home, work, time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := len(lines(t, filepath.Join(dir, "starts"))); n != 2 {
		t.Fatalf("starts = %d", n)
	}
}

func TestClaudeCodeLiveProcessHangIsKilledAtTheTimeout(t *testing.T) {
	bin, dir := liveCLI(t, `if [ $n -eq 2 ]; then sleep 60 & wait; fi`)
	stripStdinCapture(t, bin)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	live := NewLive(MaxLive)
	defer live.Close()
	home, work := t.TempDir(), t.TempDir()
	if _, err := liveRun(t, r, live, context.Background(), "", home, work, time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := liveRun(t, r, live, ctx, "live-1", home, work, time.Minute); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the turn outlived its timeout by %s", time.Since(start))
	}
	// Not the EXIT trap: dash does not run it when a signal kills it.
	pid, err := strconv.Atoi(lines(t, filepath.Join(dir, "pids"))[0])
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the hung process to be killed", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
}

func TestClaudeCodeLiveProcessRefusesControlRequests(t *testing.T) {
	// A control request mid-turn is answered with an error at once, and
	// the turn carries on to its result.
	bin, dir := liveCLI(t, `echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool"}}'
  IFS= read -r reply; printf '%s\n' "$reply" > "$(dirname "$0")/reply"`)
	stripStdinCapture(t, bin)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	live := NewLive(MaxLive)
	defer live.Close()
	if _, err := liveRun(t, r, live, context.Background(), "", t.TempDir(), t.TempDir(), time.Minute); err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dir, "reply"))), &reply); err != nil ||
		reply.Type != "control_response" || reply.Response.Subtype != "error" || reply.Response.RequestID != "r1" {
		t.Fatalf("reply = %+v %v", reply, err)
	}
}

func TestSessionFile(t *testing.T) {
	home := t.TempDir()
	p, ok := SessionFile(home, "/data/agents/finance/work", "abc")
	if !ok || p != filepath.Join(home, ".claude/projects/-data-agents-finance-work/abc.jsonl") {
		t.Fatalf("path = %s %v", p, ok)
	}
	if _, ok := SessionFile(home, "/"+strings.Repeat("x", 250), "abc"); ok {
		t.Fatal("a name past 200 characters is hashed by the CLI; orbit must not guess it")
	}
}

func TestClaudeCodeHarnessToolsAndHooks(t *testing.T) {
	bin, dir := fakeCLI(t, `echo '{"type":"result","subtype":"success","is_error":false,"structured_output":{}}'`)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	work := t.TempDir()
	if _, err := r.Run(context.Background(), Request{Workdir: work, OutputSchema: json.RawMessage(`{}`),
		Harness: &Harness{Bin: "/opt/orbit", Figures: true}}, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(read(t, filepath.Join(dir, "argv")), "\n")
	flag := func(name string) string {
		for i, a := range argv {
			if a == name && i+1 < len(argv) {
				return argv[i+1]
			}
		}
		t.Fatalf("argv lacks %s", name)
		return ""
	}
	state := filepath.Join(work, ".home", ".orbit-harness")
	var mcp struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(flag("--mcp-config")), &mcp); err != nil {
		t.Fatal(err)
	}
	if s := mcp.MCPServers["orbit"]; s.Command != "/opt/orbit" || strings.Join(s.Args, " ") != "mcp --root "+work+" --state "+state {
		t.Fatalf("mcp server = %+v", s)
	}
	if got := flag("--allowedTools"); got != "mcp__orbit__query,mcp__orbit__calc" {
		t.Fatalf("allowedTools = %q", got)
	}
	var settings struct {
		DisableAllHooks bool `json:"disableAllHooks"`
		Hooks           map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(flag("--settings")), &settings); err != nil {
		t.Fatal(err)
	}
	pre := settings.Hooks["PreToolUse"]
	if settings.DisableAllHooks || len(pre) != 2 || pre[0].Matcher != "Read|Glob|Grep" || pre[1].Matcher != "StructuredOutput" {
		t.Fatalf("settings = %+v", settings)
	}
	if want := "'/opt/orbit' hook --root '" + work + "' --state '" + state + "' --figures"; pre[1].Hooks[0].Command != want {
		t.Fatalf("hook command = %q, want %q", pre[1].Hooks[0].Command, want)
	}
}

func TestClaudeCodeWithoutHarnessDisablesHooks(t *testing.T) {
	bin, dir := fakeCLI(t, `echo '{"type":"result","subtype":"success","is_error":false,"structured_output":{}}'`)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	if _, err := r.Run(context.Background(), Request{Workdir: t.TempDir(), OutputSchema: json.RawMessage(`{}`)}, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	argv := read(t, filepath.Join(dir, "argv"))
	if !strings.Contains(argv, `"disableAllHooks":true`) || strings.Contains(argv, "--mcp-config") {
		t.Fatalf("argv:\n%s", argv)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/a b/it's"); got != `'/a b/it'\''s'` {
		t.Fatalf("got %s", got)
	}
}

func TestClaudeCodeArgsAreTheTransportsThenOrbits(t *testing.T) {
	bin, dir := fakeCLI(t, `echo '{"type":"result","subtype":"success","is_error":false,"structured_output":{}}'`)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	if _, err := r.Run(context.Background(), Request{Workdir: t.TempDir(), OutputSchema: json.RawMessage(`{}`)}, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if argv := read(t, filepath.Join(dir, "argv")); !strings.HasPrefix(argv, "-p\n--output-format\nstream-json\n--verbose\n--no-session-persistence\n--json-schema\n") {
		t.Fatalf("argv:\n%s", argv)
	}
}

func TestClaudeCodeReportsTheErrorsOfAFailedTurn(t *testing.T) {
	bin, _ := fakeCLI(t, `echo '{"type":"result","subtype":"error_max_turns","is_error":true,"terminal_reason":"max_turns","errors":["Reached maximum number of turns (3)"]}'`)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	_, err := r.Run(context.Background(), Request{Workdir: t.TempDir(), OutputSchema: json.RawMessage(`{}`)}, func(Event) {})
	if !errors.Is(err, ErrBackend) || !strings.Contains(err.Error(), "Reached maximum number of turns (3)") {
		t.Fatalf("err = %v", err)
	}
}
