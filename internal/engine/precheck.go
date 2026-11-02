package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// precheckOutput bounds what a precheck's stdout and stderr each keep: a
// verdict and its reason, not a log.
const precheckOutput = 4 << 10

// precheckSkip is the exit status that means nothing to do, as grep's 1
// means no match; 0 runs the agent, and any other status is an error.
const precheckSkip = 1

// precheckResult is one precheck's run, recorded as the orbit.precheck
// event whatever it decided.
type precheckResult struct {
	Command         string `json:"command"`
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	Error           string `json:"error,omitempty"`
}

// capped keeps the first max bytes written to it.
type capped struct {
	max       int
	b         bytes.Buffer
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.b.Write(p[:room])
		}
		return len(p), nil
	}
	c.b.Write(p)
	return len(p), nil
}

// precheckEnv is everything a precheck's process gets: no orbit secret, no
// credential, only where it is and what run it decides for.
func precheckEnv(a *spec.Agent, runID, workdir, inputs string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + workdir,
		"LANG=C.UTF-8",
		"TZ=" + a.Timezone,
		"ORBIT_AGENT=" + a.Name,
		"ORBIT_RUN_ID=" + runID,
		"ORBIT_INPUTS=" + inputs,
	}
}

// runPrecheck runs a precheck's command with sh -c in dir, in its own
// process group so a timeout ends everything it started.
func runPrecheck(ctx context.Context, command, dir string, env []string, timeout time.Duration) precheckResult {
	res := precheckResult{Command: command, ExitCode: -1}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", command)
	cmd.Dir, cmd.Env = dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	out, errb := &capped{max: precheckOutput}, &capped{max: precheckOutput}
	cmd.Stdout, cmd.Stderr = out, errb
	start := time.Now()
	err := cmd.Run()
	res.DurationMS = time.Since(start).Milliseconds()
	res.Stdout, res.StdoutTruncated = out.b.String(), out.truncated
	res.Stderr, res.StderrTruncated = errb.b.String(), errb.truncated
	var ee *exec.ExitError
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		res.Error = err.Error()
	}
	return res
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// precheck decides one precheck gate: a skip, nil to go on, or an error
// that fails the run (a timeout, a status other than 0 and 1, a command
// that could not start), naming what it printed.
func (e *Engine) precheck(ctx context.Context, a *spec.Agent, g spec.Gate, runID, workdir, inputs string, emit func(string, any)) (*gateSkip, error) {
	timeout := time.Duration(g.Timeout)
	if timeout <= 0 {
		timeout = spec.DefaultPrecheckTimeout
	}
	res := runPrecheck(ctx, g.Command, workdir, precheckEnv(a, runID, workdir, inputs), timeout)
	emit("orbit.precheck", res)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case res.TimedOut:
		return nil, fmt.Errorf("precheck timed out after %s", timeout)
	case res.Error != "":
		return nil, fmt.Errorf("precheck could not run: %s", res.Error)
	case res.ExitCode == 0:
		return nil, nil
	case res.ExitCode == precheckSkip:
		why := "precheck exited 1"
		if l := firstLine(res.Stdout); l != "" {
			why += ": " + l
		}
		return &gateSkip{gate: "precheck", summary: why}, nil
	}
	why := fmt.Sprintf("precheck exited %d", res.ExitCode)
	if l := lastLine(res.Stderr); l != "" {
		why += ": " + l
	}
	return nil, errors.New(why)
}
