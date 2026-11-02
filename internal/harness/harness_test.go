package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tx = `{"transactions":[
 {"merchant":"Trader Joes","cents":4512,"category":"groceries","amount":"45.12"},
 {"merchant":"Uber Eats","cents":2399,"category":"takeout"},
 {"merchant":"Uber Eats","cents":3150,"category":"takeout"},
 {"merchant":"Refund","cents":-1000,"category":"takeout"},
 {"merchant":"Odd","dollars":0.1,"category":"misc"},
 {"merchant":"Odd","dollars":0.2,"category":"misc"}
]}`

func workdir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "inputs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inputs", "tx.json"), []byte(tx), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestQueryGroupsAndSumsExactly(t *testing.T) {
	root := workdir(t)
	res, err := RunQuery(root, Query{File: "inputs/tx.json", Items: "transactions",
		Where:      []Cond{{Field: "category", Op: "in", Value: json.RawMessage(`["takeout","groceries"]`)}},
		GroupBy:    []string{"category"},
		Aggregates: []Agg{{Op: "sum", Field: "cents"}, {Op: "count"}, {Op: "min", Field: "cents"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"matched":4,"records":6,"rows":[{"count":1,"group":{"category":"groceries"},"min(cents)":"4512","sum(cents)":"4512"},` +
		`{"count":3,"group":{"category":"takeout"},"min(cents)":"-1000","sum(cents)":"4549"}],"truncated":false}`
	if got := jsonOf(t, res); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// 0.1 + 0.2 is 0.3, not 0.30000000000000004.
	res, err = RunQuery(root, Query{File: "inputs/tx.json", Items: "transactions",
		Where: []Cond{{Field: "dollars", Op: "gt", Value: json.RawMessage(`0`)}}, Aggregates: []Agg{{Op: "sum", Field: "dollars"}}})
	if err != nil || jsonOf(t, res["rows"]) != `[{"sum(dollars)":"0.3"}]` {
		t.Fatalf("decimal sum = %v %v", res, err)
	}
	// Select returns fields of matching records.
	res, _ = RunQuery(root, Query{File: "inputs/tx.json", Items: "transactions",
		Where: []Cond{{Field: "merchant", Op: "contains", Value: json.RawMessage(`"uber"`)}}, Select: []string{"cents"}, Limit: 1})
	if jsonOf(t, res["rows"]) != `[{"cents":2399}]` || res["truncated"] != true {
		t.Fatalf("select = %v", res)
	}
}

func TestQueryRefusesWhatItCannotAnswer(t *testing.T) {
	root := workdir(t)
	if err := os.WriteFile(filepath.Join(t.TempDir(), "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.json")
	_ = os.WriteFile(outside, []byte(`[1]`), 0o600)
	_ = os.Symlink(outside, filepath.Join(root, "inputs", "link.json"))
	for name, q := range map[string]Query{
		"a path out":        {File: "../" + filepath.Base(filepath.Dir(outside)) + "/secret.json"},
		"an absolute path":  {File: outside},
		"a symlink out":     {File: "inputs/link.json"},
		"a misspelt field":  {File: "inputs/tx.json", Items: "transactions", Aggregates: []Agg{{Op: "sum", Field: "amonut"}}},
		"a non-numeric sum": {File: "inputs/tx.json", Items: "transactions", Aggregates: []Agg{{Op: "sum", Field: "merchant"}}},
		"not an array":      {File: "inputs/tx.json"},
		"an unknown op":     {File: "inputs/tx.json", Items: "transactions", Where: []Cond{{Field: "cents", Op: "like"}}},
	} {
		if _, err := RunQuery(root, q); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestCalcIsExact(t *testing.T) {
	for expr, want := range map[string]string{
		"0.1 + 0.2":           "0.3",
		"(4512 + 2399) / 100": "69.11",
		"16082 / 4 / 100":     "40.205",
		"-2 * (3 - 5)":        "4",
		"1,234.50 + 0.5":      "1235",
		"10 / 4":              "2.5",
	} {
		res, err := Calc(expr)
		if err != nil || res["result"] != want || res["rounded"] != nil {
			t.Errorf("%s = %v %v, want %s", expr, res["result"], err, want)
		}
	}
	res, err := Calc("1 / 3")
	if err != nil || res["result"] != "0.3333333333" || res["rounded"] != true || res["fraction"] != "1/3" {
		t.Fatalf("1/3 = %v %v", res, err)
	}
	for _, bad := range []string{"1 / 0", "2 +", "(1", "1 2", "a + 1", strings.Repeat("(", 100) + "1" + strings.Repeat(")", 100)} {
		if _, err := Calc(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestMCPServer(t *testing.T) {
	root, state := workdir(t), t.TempDir()
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"calc","arguments":{"expression":"2+2"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"query","arguments":{"file":"../x"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"nope"}`,
	}, "\n")
	var out bytes.Buffer
	if err := ServeMCP(strings.NewReader(in), &out, root, state, "", "", "test"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("a notification must get no reply; replies:\n%s", out.String())
	}
	var init struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(lines[0]), &init)
	if init.Result.ProtocolVersion != "2025-03-26" {
		t.Errorf("initialize = %s", lines[0])
	}
	if !strings.Contains(lines[1], `"name":"query"`) || !strings.Contains(lines[1], `"name":"calc"`) {
		t.Errorf("tools/list = %s", lines[1])
	}
	if !strings.Contains(lines[2], `{\"expression\":\"2+2\",\"result\":\"4\"}`) || strings.Contains(lines[2], "isError") {
		t.Errorf("calc = %s", lines[2])
	}
	if !strings.Contains(lines[3], `"isError":true`) || !strings.Contains(lines[3], "outside") {
		t.Errorf("a query out of the workdir must be a tool error: %s", lines[3])
	}
	if !strings.Contains(lines[4], `-32601`) {
		t.Errorf("unknown method = %s", lines[4])
	}
	ledger, _ := os.ReadFile(LedgerPath(state))
	if strings.Count(string(ledger), "\n") != 1 || !strings.Contains(string(ledger), `"tool":"calc"`) {
		t.Fatalf("ledger = %s, want the one successful result", ledger)
	}
}

func hook(t *testing.T, o HookOptions, event string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := Hook(strings.NewReader(event), &out, o); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("hook printed %q", out.String())
	}
	return v
}

func denied(v map[string]any) string {
	h, _ := v["hookSpecificOutput"].(map[string]any)
	if h == nil || h["permissionDecision"] != "deny" {
		return ""
	}
	return h["permissionDecisionReason"].(string)
}

func pre(tool, input string) string {
	return `{"hook_event_name":"PreToolUse","session_id":"s","prompt_id":"p","tool_name":"` + tool + `","tool_input":` + input + `}`
}

func TestHookGuardsPaths(t *testing.T) {
	root := workdir(t)
	outside := t.TempDir()
	_ = os.Symlink(outside, filepath.Join(root, "escape"))
	o := HookOptions{Root: root}
	for _, ok := range []string{
		pre("Read", `{"file_path":"inputs/tx.json"}`),
		pre("Read", `{"file_path":"`+filepath.Join(root, "inputs", "new.json")+`"}`),
		pre("Grep", `{"pattern":"x"}`),
		pre("Glob", `{"pattern":"inputs/**/*.json"}`),
		`{"hook_event_name":"Stop"}`,
	} {
		if r := denied(hook(t, o, ok)); r != "" {
			t.Errorf("%s denied: %s", ok, r)
		}
	}
	for _, bad := range []string{
		pre("Read", `{"file_path":"../../etc/passwd"}`),
		pre("Read", `{"file_path":"/etc/passwd"}`),
		pre("Read", `{"file_path":"~/.claude/x"}`),
		pre("Read", `{"file_path":"escape/secret"}`),
		pre("Grep", `{"pattern":"x","path":"/proc"}`),
		pre("Glob", `{"pattern":"/etc/*"}`),
		pre("Glob", `{"pattern":"../**"}`),
		`not json`,
	} {
		if denied(hook(t, o, bad)) == "" {
			t.Errorf("%s allowed", bad)
		}
	}
}

func TestHookSendsUntracedFiguresBack(t *testing.T) {
	root, state := workdir(t), t.TempDir()
	o := HookOptions{Root: root, Figures: true, State: state}
	submit := func(report string) map[string]any {
		in, _ := json.Marshal(map[string]any{"summary": "s", "report": report, "notify": map[string]any{"title": ""}})
		return hook(t, o, pre("StructuredOutput", string(in)))
	}
	// Input values, as written or as cents, and rounded figures, pass.
	if r := denied(submit("Trader Joes was $45.12; Uber $23.99; a $10 refund; $45.1 rounded.")); r != "" {
		t.Fatalf("traced figures sent back: %s", r)
	}
	// A total nothing computed is sent back, at most MaxFigureDenials times.
	r := denied(submit("Takeout was $45.49."))
	if !strings.Contains(r, "$45.49") || !strings.Contains(r, ToolQuery) {
		t.Fatalf("reason = %q", r)
	}
	_ = denied(submit("Takeout was $45.49."))
	if denied(submit("Takeout was $45.49.")) != "" {
		t.Fatal("a third denial would fail the run; the engine notes the figure instead")
	}
	// Once orbit's tools computed it, it passes.
	o.State = t.TempDir()
	ledger(o.State, "query", `{"rows":[{"sum(cents)":"4549"}]}`)
	if r := denied(submit("Takeout was $45.49.")); r != "" {
		t.Fatalf("a computed figure sent back: %s", r)
	}
}

func TestOrbitToolResultsReadsTheStream(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__orbit__query","input":{}},{"type":"tool_use","id":"t2","name":"Read","input":{}}]}}`),
		[]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"{\"sum\":\"5549\"}"}]},{"type":"tool_result","tool_use_id":"t2","content":"99999"}]}}`),
	}
	got := OrbitToolResults(lines)
	if len(got) != 1 || got[0] != `{"sum":"5549"}` {
		t.Fatalf("results = %v, want only orbit's", got)
	}
	b := NewBacked()
	b.AddText(got[0])
	if m := b.Unbacked("$55.49 and $999.99"); len(m) != 1 || m[0] != "$999.99" {
		t.Fatalf("unbacked = %v", m)
	}
}

func TestMCPSearchAsksTheEngineSocket(t *testing.T) {
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("orbit-test-%d.sock", os.Getpid()))
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(sock) })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			req, err := ReadSearchRequest(conn)
			resp := SearchResponse{Result: map[string]any{"total": 1, "echo": req.Q}}
			if err != nil {
				resp = SearchResponse{Error: err.Error()}
			}
			b, _ := json.Marshal(resp)
			_, _ = conn.Write(append(b, '\n'))
			_ = conn.Close()
		}
	}()
	root, state := workdir(t), t.TempDir()
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"q":"heat pump","kind":"report"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"q":"x","agent":"other"}}}`,
	}, "\n")
	var out bytes.Buffer
	if err := ServeMCP(strings.NewReader(in), &out, root, state, sock, "", "test"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"name":"search"`) {
		t.Fatalf("tools/list = %s", out.String())
	}
	if !strings.Contains(lines[1], `\"echo\":\"heat pump\"`) || strings.Contains(lines[1], "isError") {
		t.Errorf("search = %s", lines[1])
	}
	if !strings.Contains(lines[2], `"isError":true`) || !strings.Contains(lines[2], "agent") {
		t.Errorf("a search naming an agent must be refused: %s", lines[2])
	}
	if ledger, _ := os.ReadFile(LedgerPath(state)); len(ledger) != 0 {
		t.Fatalf("a search result backs no figure, but the ledger has %s", ledger)
	}

	out.Reset()
	if err := ServeMCP(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), &out, root, state, "", "", "test"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `"name":"search"`) {
		t.Fatal("search listed without a socket")
	}
}

func TestMCPRepositoryToolsAskTheEngineSocket(t *testing.T) {
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("orbit-test-repos-%d.sock", os.Getpid()))
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(sock) })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			req, err := ReadRepoRequest(conn)
			resp := RepoResponse{Text: req.Tool + " " + string(req.Args)}
			if err != nil {
				resp = RepoResponse{Error: err.Error()}
			}
			b, _ := json.Marshal(resp)
			_, _ = conn.Write(append(b, '\n'))
			_ = conn.Close()
		}
	}()
	root, state := workdir(t), t.TempDir()
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"git_log","arguments":{"max":3}}}`,
	}, "\n")
	var out bytes.Buffer
	if err := ServeMCP(strings.NewReader(in), &out, root, state, "", sock, "test"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for _, name := range []string{"git_log", "git_show", "git_diff", "git_blame", "pr_list", "pr_view"} {
		if !strings.Contains(lines[0], `"name":"`+name+`"`) {
			t.Fatalf("tools/list lacks %s: %s", name, lines[0])
		}
	}
	if !strings.Contains(lines[1], `git_log {\"max\":3}`) || strings.Contains(lines[1], "isError") {
		t.Fatalf("git_log = %s", lines[1])
	}
	if ledger, _ := os.ReadFile(LedgerPath(state)); len(ledger) != 0 {
		t.Fatalf("a repository answer backs no figure, but the ledger has %s", ledger)
	}
	if _, err := Repo(sock, RepoRequest{Tool: "exec", Args: json.RawMessage(`{}`)}); err == nil || !strings.Contains(err.Error(), "unknown repository tool") {
		t.Fatalf("an unknown tool = %v", err)
	}
	if got := RepoToolNames(); got[0] != "mcp__orbit__git_log" || len(got) != len(RepoTools) {
		t.Fatalf("names = %v", got)
	}
}

func TestHookAddsPinnedEntriesToEveryPrompt(t *testing.T) {
	state := t.TempDir()
	o := HookOptions{Root: workdir(t), State: state}
	prompt := `{"hook_event_name":"UserPromptSubmit","prompt":"hi"}`
	if v := hook(t, o, prompt); len(v) != 0 {
		t.Fatalf("no pinned file = %v, want nothing added", v)
	}
	if err := os.WriteFile(PinnedPath(state), []byte("Pinned rules:\n- No em dashes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		v := hook(t, o, prompt)
		out, _ := v["hookSpecificOutput"].(map[string]any)
		if out["hookEventName"] != "UserPromptSubmit" || out["additionalContext"] != "Pinned rules:\n- No em dashes" {
			t.Fatalf("hook = %v", v)
		}
	}
	if err := os.WriteFile(PinnedPath(state), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if v := hook(t, o, prompt); len(v) != 0 {
		t.Fatalf("empty pinned file = %v", v)
	}
}
