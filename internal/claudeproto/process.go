package claudeproto

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CloseGrace is how long a closing CLI gets to exit after its stdin closes, and again after SIGTERM, before its process group is killed. The CLI saves its session's running cost when it exits on SIGTERM (runner.TranscriptCost), which a SIGKILL loses.
const CloseGrace = 10 * time.Second

// Command is how to start a CLI. Env is the whole environment: nothing of the caller's is inherited (see Env).
type Command struct {
	Bin  string
	Args []string
	Env  []string
	Dir  string
}

// Process is a CLI this package started, in its own process group so that stopping it stops the tools and hooks it started too.
type Process struct {
	*Conn
	cmd    *exec.Cmd
	stderr *Tail
	exited chan struct{}
	once   sync.Once
}

// Start starts the CLI and its Conn.
func Start(c Command, h Handler) (*Process, error) {
	cmd := exec.Command(c.Bin, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	if cmd.Env == nil {
		// exec inherits the parent's environment for a nil Env; the model's process must never see orbit's.
		cmd.Env = []string{}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p := &Process{cmd: cmd, stderr: &Tail{}, exited: make(chan struct{})}
	cmd.Stderr = p.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Bin, err)
	}
	p.Conn = NewConn(stdout, stdin, h)
	go func() {
		// Wait closes stdout, so the stream ends first.
		<-p.Done()
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// Exited is closed once the process has been reaped.
func (p *Process) Exited() <-chan struct{} { return p.exited }

// Pid is the process's id, which is also its process group's.
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Stderr is the tail of what the process wrote to stderr.
func (p *Process) Stderr() string { return p.stderr.String() }

// Turn is Conn.Turn, with a stream that ends mid-turn reported as the process exiting, with its last stderr lines.
func (p *Process) Turn(ctx context.Context, prompt string, emit func(Line)) (*Result, error) {
	res, err := p.Conn.Turn(ctx, prompt, emit)
	if errors.Is(err, ErrEnded) {
		return nil, p.exitError("claude exited during the turn")
	}
	return res, err
}

// exitError waits briefly for the process to be reaped and names why it ended.
func (p *Process) exitError(what string) error {
	select {
	case <-p.exited:
	case <-time.After(CloseGrace):
	}
	err := errors.New(what)
	if tail := strings.TrimSpace(p.stderr.String()); tail != "" {
		err = fmt.Errorf("%w: %s", err, LastLines(tail, 3))
	}
	return err
}

// Close ends the process: stdin closed, so a CLI between turns exits on its own and one mid-turn finishes that turn; then, after grace, SIGTERM to the group, and SIGKILL after grace more. Only the first Close or Kill acts; later ones wait for it.
func (p *Process) Close(grace time.Duration) { p.stop(grace, grace) }

// Kill ends a process whose turn failed and whose session must not be added to: SIGTERM to the group at once, then SIGKILL after CloseGrace. SIGTERM rather than SIGKILL first, because the CLI saves its session's running cost when it exits on one (runner.TranscriptCost).
func (p *Process) Kill() { p.stop(0, CloseGrace) }

func (p *Process) stop(wait, term time.Duration) {
	p.once.Do(func() {
		_ = p.CloseInput()
		pid := p.cmd.Process.Pid
		if wait > 0 {
			select {
			case <-p.exited:
				return
			case <-time.After(wait):
			}
		}
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-p.exited:
		case <-time.After(term):
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-p.exited
		}
	})
	<-p.exited
}

// Tail keeps the last 8 KB written to it, for an error message. It is written by the process while a turn may read it, hence the lock.
type Tail struct {
	mu sync.Mutex
	b  []byte
}

func (t *Tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, b...)
	if len(t.b) > 8<<10 {
		t.b = append(t.b[:0], t.b[len(t.b)-8<<10:]...)
	}
	return len(b), nil
}

func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// LastLines is the last n lines of s joined with " | ", clipped to 500 bytes: enough of a CLI's stderr to say why it failed.
func LastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, " | ")
	if len(out) > 500 {
		out = out[:500]
	}
	return out
}
