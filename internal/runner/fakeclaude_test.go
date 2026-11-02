package runner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Go fake of `claude -p --input-format stream-json` for the permission tests: the test binary run again with ORBIT_FAKE_CLAUDE naming its directory, through a wrapper script (the runner's environment is an allowlist, so the variable is set by the script). It reads scenario.json there, logs what it is sent to log, and answers:
//   - set_permission_mode: success, or an error for a mode in refuse; the mode is reported as a system.status line, as the real CLI does.
//   - a user turn: system.init with the current mode, then one can_use_tool request per entry of ask (waiting for each answer), then a result.
type fakeScenario struct {
	Refuse []string          `json:"refuse"`
	Ask    []json.RawMessage `json:"ask"`
	// Plan puts the session in plan mode before the asks, as the model's EnterPlanMode does; an allowed ExitPlanMode leaves it for default, as the real CLI does.
	Plan bool `json:"plan"`
}

func TestMain(m *testing.M) {
	if dir := os.Getenv("ORBIT_FAKE_CLAUDE"); dir != "" {
		fakeClaude(dir)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeClaude(dir string) {
	var sc fakeScenario
	if b, err := os.ReadFile(filepath.Join(dir, "scenario.json")); err == nil {
		_ = json.Unmarshal(b, &sc)
	}
	logf, _ := os.OpenFile(filepath.Join(dir, "log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	defer logf.Close()
	starts, _ := os.OpenFile(filepath.Join(dir, "starts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprintln(starts, strings.Join(os.Args[1:], " "))
	_ = starts.Close()
	mode := "default"
	for i, a := range os.Args {
		if a == "--permission-mode" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
		}
	}
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = out.Write(append(b, '\n'))
		_ = out.Flush()
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<24)
	// next reads the next line, answering host control requests on the way, and returns it.
	next := func() map[string]any {
		for in.Scan() {
			var m map[string]any
			if json.Unmarshal(in.Bytes(), &m) != nil {
				continue
			}
			if m["type"] != "control_request" {
				return m
			}
			req, _ := m["request"].(map[string]any)
			id := m["request_id"]
			if req["subtype"] == "set_permission_mode" {
				want, _ := req["mode"].(string)
				for _, r := range sc.Refuse {
					if r == want {
						fmt.Fprintln(logf, "refused", want)
						send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "Cannot set permission mode to " + want}})
						want = ""
						break
					}
				}
				if want != "" {
					mode = want
					fmt.Fprintln(logf, "mode", mode)
					send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{}}})
					send(map[string]any{"type": "system", "subtype": "status", "status": nil, "permissionMode": mode, "session_id": "fake-1"})
				}
				continue
			}
			send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{}}})
		}
		return nil
	}
	turn := 0
	for {
		m := next()
		if m == nil {
			return
		}
		if m["type"] != "user" {
			continue
		}
		turn++
		fmt.Fprintln(logf, "turn", turn, mode)
		send(map[string]any{"type": "system", "subtype": "init", "permissionMode": mode, "session_id": "fake-1"})
		if sc.Plan {
			mode = "plan"
			fmt.Fprintln(logf, "entered plan")
			send(map[string]any{"type": "system", "subtype": "status", "status": nil, "permissionMode": mode, "session_id": "fake-1"})
		}
		for i, ask := range sc.Ask {
			id := fmt.Sprintf("ask-%d-%d", turn, i)
			var req map[string]any
			_ = json.Unmarshal(ask, &req)
			req["subtype"] = "can_use_tool"
			send(map[string]any{"type": "control_request", "request_id": id, "request": req})
			for {
				r := next()
				if r == nil {
					return
				}
				if r["type"] == "control_response" {
					resp, _ := r["response"].(map[string]any)
					body, _ := json.Marshal(resp["response"])
					fmt.Fprintln(logf, "answer", resp["request_id"], string(body))
					if req["tool_name"] == "ExitPlanMode" && strings.Contains(string(body), `"allow"`) {
						mode = "default"
						fmt.Fprintln(logf, "left plan")
						send(map[string]any{"type": "system", "subtype": "status", "status": nil, "permissionMode": mode, "session_id": "fake-1"})
					}
					break
				}
			}
		}
		send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": fmt.Sprintf("reply %d", turn), "session_id": "fake-1", "total_cost_usd": 0.01})
	}
}

// fakeClaudeBin is a wrapper that runs the fake with scenario sc, and its directory.
func fakeClaudeBin(t *testing.T, sc fakeScenario) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	b, _ := json.Marshal(sc)
	if err := os.WriteFile(filepath.Join(dir, "scenario.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\nORBIT_FAKE_CLAUDE='" + dir + "' exec '" + self + "' \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}
