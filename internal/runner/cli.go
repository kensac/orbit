package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// maxLine bounds one stream line. A tool result echoing a large input file
// arrives as one line; 16 MB is twice the default source cap.
const maxLine = 16 << 20

// runCLI starts a backend CLI with prompt on stdin, hands each stdout line
// to onLine, and returns stderr's tail for error reporting. The process runs
// in its own process group so a timeout kills its children too.
func runCLI(ctx context.Context, bin string, args, env []string, dir, prompt string, onLine func([]byte)) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start %s: %w", bin, err)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// SIGTERM the group, then SIGKILL if it lingers.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-done:
		}
	}()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) > 0 {
			onLine(append([]byte(nil), line...))
		}
	}
	scanErr := sc.Err()
	if scanErr != nil {
		// Drain so the process is not blocked writing to a full pipe.
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	close(done)
	if ctx.Err() != nil {
		return stderr.String(), ctx.Err()
	}
	if scanErr != nil {
		return stderr.String(), fmt.Errorf("read %s output: %w", bin, scanErr)
	}
	return stderr.String(), waitErr
}

// tailBuffer keeps the last 8 KB written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 8<<10 {
		t.b = t.b[len(t.b)-8<<10:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

// writeTemp writes content to a file in dir and returns its path.
func writeTemp(dir, name string, content []byte) (string, error) {
	f, err := os.CreateTemp(dir, name)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return "", err
	}
	return f.Name(), f.Close()
}

// eventKind names a stream line by its type and subtype fields.
func eventKind(line []byte) string {
	var h struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if json.Unmarshal(line, &h) != nil || h.Type == "" {
		return "raw"
	}
	if h.Subtype != "" {
		return h.Type + "." + h.Subtype
	}
	return h.Type
}

func exitDetail(err error, stderr string) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && strings.TrimSpace(stderr) != "" {
		return fmt.Errorf("%w: %s", err, firstLine(lastLines(stderr, 3)))
	}
	return err
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
