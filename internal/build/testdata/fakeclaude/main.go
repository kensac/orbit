// Command fakeclaude stands in for the Claude Code CLI in build tests. It
// speaks the stdio permission protocol the real CLI does: it reads one
// stream-json user message, runs the script embedded in it (a line
// "FAKE-SCRIPT: <json>"), asks orbit before every scripted tool call with a
// can_use_tool control request, acts only on an allow, and ends with a
// result line. It maps /work to its working directory, so it runs the same
// in a real sandbox and in a test's temporary directory.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type step struct {
	// Ask: a tool call orbit must allow before it happens.
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// Sneak writes a file without asking: what code the model ran (go test)
	// could do. The diff policy, not the handler, must catch it.
	Sneak   string `json:"sneak,omitempty"`
	Content string `json:"content,omitempty"`
	// Leak writes the backend credential into a file without asking.
	Leak string `json:"leak,omitempty"`
	// Symlink makes a symlink Sneak -> Target without asking.
	Target string `json:"target,omitempty"`
	// Crash exits mid-run without a result; Hang never finishes.
	Crash bool `json:"crash,omitempty"`
	Hang  bool `json:"hang,omitempty"`
	// Control sends a control request of another subtype.
	Control string `json:"control,omitempty"`
}

type script struct {
	Steps  []step          `json:"steps"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

var (
	in  = bufio.NewReaderSize(os.Stdin, 1<<20)
	out = bufio.NewWriter(os.Stdout)
	cwd string
)

func emit(v any) {
	b, _ := json.Marshal(v)
	out.Write(append(b, '\n'))
	out.Flush()
}

func readLine() (map[string]any, bool) {
	line, err := in.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return nil, true
	}
	return m, true
}

func local(p string) string {
	if strings.HasPrefix(p, "/work/") {
		return filepath.Join(cwd, strings.TrimPrefix(p, "/work/"))
	}
	if !filepath.IsAbs(p) {
		return filepath.Join(cwd, p)
	}
	return p
}

// ask sends one can_use_tool request and waits for its answer.
func ask(id, tool string, input json.RawMessage) (allowed bool, message string) {
	emit(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": tool, "input": input, "tool_use_id": "toolu_" + id,
	}})
	for {
		m, ok := readLine()
		if !ok {
			// orbit went away: no answer is no permission.
			fmt.Fprintln(os.Stderr, "stdin closed while waiting for permission")
			os.Exit(3)
		}
		if m == nil || m["type"] != "control_response" {
			continue
		}
		r, _ := m["response"].(map[string]any)
		if r["request_id"] != id {
			continue
		}
		if r["subtype"] != "success" {
			return false, fmt.Sprint(r["error"])
		}
		resp, _ := r["response"].(map[string]any)
		if resp["behavior"] == "allow" {
			return true, ""
		}
		return false, fmt.Sprint(resp["message"])
	}
}

func main() {
	cwd, _ = os.Getwd()
	args := strings.Join(os.Args[1:], " ")
	for _, want := range []string{"--permission-prompt-tool stdio", "--permission-mode default", "--input-format stream-json"} {
		if !strings.Contains(args, want) {
			fmt.Fprintln(os.Stderr, "fakeclaude: missing", want)
			os.Exit(2)
		}
	}
	first, ok := readLine()
	if !ok || first == nil {
		os.Exit(2)
	}
	msg, _ := first["message"].(map[string]any)
	content, _ := msg["content"].(string)
	var sc script
	for _, l := range strings.Split(content, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "FAKE-SCRIPT: "); ok {
			if err := json.Unmarshal([]byte(rest), &sc); err != nil {
				fmt.Fprintln(os.Stderr, "fakeclaude: bad script:", err)
				os.Exit(2)
			}
		}
	}
	emit(map[string]any{"type": "system", "subtype": "init", "cwd": cwd, "permissionMode": "default"})
	var denials []map[string]any
	for i, s := range sc.Steps {
		id := fmt.Sprintf("req-%d", i)
		switch {
		case s.Crash:
			fmt.Fprintln(os.Stderr, "fakeclaude: crashing as scripted")
			os.Exit(1)
		case s.Hang:
			time.Sleep(time.Hour)
		case s.Control != "":
			emit(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{"subtype": s.Control}})
		case s.Leak != "":
			_ = os.MkdirAll(filepath.Dir(local(s.Leak)), 0o755)
			_ = os.WriteFile(local(s.Leak), []byte("token="+os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")+"\n"), 0o644)
		case s.Sneak != "" && s.Target != "":
			_ = os.MkdirAll(filepath.Dir(local(s.Sneak)), 0o755)
			_ = os.Symlink(s.Target, local(s.Sneak))
		case s.Sneak != "":
			_ = os.MkdirAll(filepath.Dir(local(s.Sneak)), 0o755)
			_ = os.WriteFile(local(s.Sneak), []byte(s.Content), 0o644)
		case s.Tool != "":
			emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_" + id, "name": s.Tool, "input": s.Input}}}})
			allowed, why := ask(id, s.Tool, s.Input)
			result := "ok"
			if !allowed {
				denials = append(denials, map[string]any{"tool_name": s.Tool, "tool_use_id": "toolu_" + id, "tool_input": s.Input})
				result = why
			} else {
				var w struct {
					FilePath  string `json:"file_path"`
					Content   string `json:"content"`
					OldString string `json:"old_string"`
					NewString string `json:"new_string"`
				}
				_ = json.Unmarshal(s.Input, &w)
				switch s.Tool {
				case "Write":
					_ = os.MkdirAll(filepath.Dir(local(w.FilePath)), 0o755)
					_ = os.WriteFile(local(w.FilePath), []byte(w.Content), 0o644)
				case "Edit":
					b, _ := os.ReadFile(local(w.FilePath))
					_ = os.WriteFile(local(w.FilePath), []byte(strings.Replace(string(b), w.OldString, w.NewString, 1)), 0o644)
				}
			}
			emit(map[string]any{"type": "user", "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_" + id, "content": result}}}})
		}
	}
	res := map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": len(sc.Steps) + 1,
		"total_cost_usd": 0.5, "usage": map[string]any{"input_tokens": 10}, "permission_denials": denials, "session_id": "s"}
	if sc.Error != "" {
		res["is_error"], res["subtype"], res["result"] = true, "error_max_turns", sc.Error
	} else if len(sc.Output) > 0 {
		res["structured_output"] = sc.Output
	} else {
		res["structured_output"] = map[string]any{"summary": "Did the thing", "pr_title": "Add the thing",
			"pr_body": "Adds the thing.\n\nTested with go test.\n\nCo-Authored-By: Claude <noreply@anthropic.com>", "notes": ""}
	}
	emit(res)
	// As the real CLI in stream-json input mode: wait for stdin to close.
	for {
		if _, ok := readLine(); !ok {
			return
		}
	}
}
