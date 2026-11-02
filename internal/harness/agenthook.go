package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// AgentHookDef is one agent hook as orbit passes it to `orbit agent-hook` (spec.Hook, without the matcher, which the CLI applies).
type AgentHookDef struct {
	Event   string        `json:"event"`
	Say     string        `json:"say,omitempty"`
	Deny    string        `json:"deny,omitempty"`
	Run     []string      `json:"run,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`
}

// EncodeAgentHook is the flag value a hook command carries its definition in: base64 of its JSON, so no shell quoting can change it.
func EncodeAgentHook(d AgentHookDef) string {
	b, _ := json.Marshal(d)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeAgentHook reads EncodeAgentHook's output.
func DecodeAgentHook(s string) (AgentHookDef, error) {
	var d AgentHookDef
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &d)
	}
	return d, err
}

// agentHookOutputMax bounds what a run hook's command may print back to the CLI, and so into the model's context.
const agentHookOutputMax = 64 << 10

// AgentHook handles one event for one agent hook and returns the exit code the CLI reads (0, or 2 for a command's blocking error, whose stderr the CLI shows the model). A say or deny writes Claude Code's JSON decision; a run runs the command with the event on stdin and passes its stdout, stderr and exit code through.
func AgentHook(in io.Reader, out, errOut io.Writer, d AgentHookDef, root string) int {
	ev, err := io.ReadAll(io.LimitReader(in, maxLine))
	if err != nil {
		_, _ = fmt.Fprintln(errOut, "orbit agent-hook: read the event:", err)
		return 1
	}
	switch {
	case d.Deny != "":
		_ = write(out, deny("PreToolUse", d.Deny))
		return 0
	case d.Say != "":
		if d.Event == "Stop" {
			// Sent back once: the CLI marks a stop it already sent back, and a second block would loop the turn.
			var e struct {
				Active bool `json:"stop_hook_active"`
			}
			_ = json.Unmarshal(ev, &e)
			if e.Active {
				_ = write(out, map[string]any{})
				return 0
			}
			_ = write(out, map[string]any{"decision": "block", "reason": d.Say})
			return 0
		}
		_ = write(out, map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": d.Event, "additionalContext": d.Say}})
		return 0
	case len(d.Run) > 0:
		return runAgentHook(ev, out, errOut, d, root)
	}
	_, _ = fmt.Fprintln(errOut, "orbit agent-hook: the hook does nothing")
	return 1
}

// runAgentHook runs a hook's command: argv as given (no shell), cwd the run's workdir, an environment of PATH and the workdir as HOME only (the CLI's own environment, with its credential, is not passed on), the event on stdin, under its timeout.
func runAgentHook(ev []byte, out, errOut io.Writer, d AgentHookDef, root string) int {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.Run[0], d.Run[1:]...) //nolint:gosec // argv is the agent's configuration, never the model's
	cmd.Dir = root
	// The command and anything it starts are one process group, killed together at the timeout; WaitDelay stops a child holding the pipes from keeping the hook alive.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "LANG=C.UTF-8"}
	cmd.Stdin = bytes.NewReader(ev)
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	_, _ = out.Write(stdout.b)
	_, _ = errOut.Write(stderr.b)
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		_, _ = fmt.Fprintf(errOut, "orbit agent-hook: %s timed out after %s\n", d.Run[0], timeout)
		return 1
	case errors.As(err, &exit):
		return exit.ExitCode()
	case err != nil:
		_, _ = fmt.Fprintln(errOut, "orbit agent-hook:", err)
		return 1
	}
	return 0
}

// capped keeps the first agentHookOutputMax bytes written to it.
type capped struct{ b []byte }

func (c *capped) Write(p []byte) (int, error) {
	if room := agentHookOutputMax - len(c.b); room > 0 {
		c.b = append(c.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
