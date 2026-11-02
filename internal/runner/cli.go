package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/kensac/orbit/internal/claudeproto"
)

// maxLine bounds one stream line. A tool result echoing a large input file
// arrives as one line; 16 MB is twice the default source cap.
const maxLine = 16 << 20

// runCLI starts a backend CLI with prompt on stdin, hands each stdout line
// to onLine, and returns stderr's tail for error reporting. The process runs
// in its own process group (claudeproto.Command.Exec) so a timeout stops its
// children too: SIGTERM, then SIGKILL after claudeproto.CloseGrace.
func runCLI(ctx context.Context, bin string, args, env []string, dir, prompt string, onLine func([]byte)) (string, error) {
	cmd := claudeproto.Command{Bin: bin, Args: args, Env: env, Dir: dir}.Exec()
	cmd.Stdin = strings.NewReader(prompt)
	stderr := &claudeproto.Tail{}
	cmd.Stderr = stderr
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
			claudeproto.TerminateGroup(cmd.Process.Pid, done, claudeproto.CloseGrace)
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

func exitDetail(err error, stderr string) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && strings.TrimSpace(stderr) != "" {
		return fmt.Errorf("%w: %s", err, claudeproto.LastLines(stderr, 3))
	}
	return err
}
