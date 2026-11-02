package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A live process is `claude -p --input-format stream-json`: one CLI process
// that takes user messages on stdin, one JSON line each, and answers each
// with the same stream a one-shot run prints, ending in one `result` line
// per message. That line carries the turn's own structured_output (checked
// against the real CLI 2.1.316: three turns with --json-schema gave three
// results, each with its own structured_output and num_turns, and the
// second turn saw the first). The Agent SDK drives the CLI the same way:
// it writes {"type":"user","message":{"role":"user","content":...}} lines
// and treats a ResultMessage as the end of a response (claude-agent-sdk
// 0.2.176, client.py query and receive_response). It also sends an
// `initialize` control request, which only registers SDK-side hooks, MCP
// servers and agents (_internal/query.py initialize); orbit uses none, and
// the CLI answers user messages without it.
//
// The CLI may send control requests of its own (permission prompts, hook
// callbacks, SDK MCP messages) and waits for a control_response. orbit
// configures none of their sources, so any that arrives is answered with
// an error at once rather than leaving the turn blocked.

// MaxLive bounds live processes per orbit process. Each is a resident CLI
// (about 265 MB RSS for 2.1.316 on macOS, measured idle after a turn); at
// most one per agent can be busy, and the
// executors (ORBIT_WORKERS, default 2) bound how many are. The least
// recently used idle one is closed past this.
const MaxLive = 4

// liveCloseGrace is how long a closing process gets to exit after its stdin
// closes before its process group is killed, as runCLI does on a timeout.
const liveCloseGrace = 10 * time.Second

// Live holds the live processes, one per key (a conversation).
type Live struct {
	mu     sync.Mutex
	procs  map[string]*liveProc
	max    int
	closed bool
	wg     sync.WaitGroup
}

// NewLive returns an empty set of live processes bounded by max.
func NewLive(max int) *Live { return &Live{procs: map[string]*liveProc{}, max: max} }

// Len reports how many live processes are open.
func (l *Live) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.procs)
}

// Close ends every live process and waits for them to exit: stdin closed,
// so a process between turns exits on its own and one mid-turn finishes
// that turn first, within liveCloseGrace. orbit calls it once no run is
// in progress (the end of a drain), so none is mid-turn. Later runs fall
// back to one process per run.
func (l *Live) Close() {
	l.mu.Lock()
	l.closed = true
	procs := l.procs
	l.procs = map[string]*liveProc{}
	l.mu.Unlock()
	for _, p := range procs {
		l.async(func() { p.stop(false) })
	}
	l.wg.Wait()
}

type liveProc struct {
	key     string
	hash    string
	session string // the session the process holds, from its last result
	cmd     *exec.Cmd
	stdinMu sync.Mutex
	stdin   io.WriteCloser
	lines   chan []byte   // closed at end of output
	quit    chan struct{} // closed by stop
	exited  chan struct{} // closed once the process is reaped
	stderr  *tailBuffer
	busy    bool
	used    time.Time
	timer   *time.Timer
	once    sync.Once
}

// take returns the idle process for key if it runs this exact invocation
// and holds session want. One that does not match is closed: the agent's
// spec or session moved on.
func (l *Live) take(key, hash, want string) *liveProc {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.procs[key]
	if p == nil || p.busy {
		return nil
	}
	alive := true
	select {
	case <-p.exited:
		alive = false
	default:
	}
	if !alive || p.hash != hash || p.session != want || want == "" {
		delete(l.procs, key)
		l.async(func() { p.stop(false) })
		return nil
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	p.busy = true
	return p
}

// keep returns p to the set after a successful turn, closing it after idle.
func (l *Live) keep(p *liveProc, idle time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		l.async(func() { p.stop(false) })
		return
	}
	if old := l.procs[p.key]; old != nil && old != p {
		delete(l.procs, p.key)
		l.async(func() { old.stop(false) })
	}
	p.busy, p.used = false, time.Now()
	l.procs[p.key] = p
	p.timer = time.AfterFunc(idle, func() { l.expire(p) })
	for len(l.procs) > l.max {
		var lru *liveProc
		for _, q := range l.procs {
			if !q.busy && q != p && (lru == nil || q.used.Before(lru.used)) {
				lru = q
			}
		}
		if lru == nil {
			break
		}
		delete(l.procs, lru.key)
		l.async(func() { lru.stop(false) })
	}
}

// drop removes p after a failed turn and kills it: its session's transcript
// now ends in that turn, and the next run decides afresh.
func (l *Live) drop(p *liveProc) {
	l.mu.Lock()
	if l.procs[p.key] == p {
		delete(l.procs, p.key)
	}
	l.mu.Unlock()
	p.stop(true)
}

func (l *Live) expire(p *liveProc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.procs[p.key] != p || p.busy {
		return
	}
	delete(l.procs, p.key)
	l.async(func() { p.stop(false) })
}

func (l *Live) async(f func()) {
	l.wg.Add(1)
	go func() { defer l.wg.Done(); f() }()
}

// startLive starts a process. Its output is read for its whole life: a
// line waits for the turn that reads it, and stop releases the reader.
func startLive(bin string, args, env []string, dir string) (*liveProc, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p := &liveProc{
		cmd: cmd, lines: make(chan []byte), quit: make(chan struct{}), exited: make(chan struct{}),
		stderr: &tailBuffer{},
	}
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
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	p.stdin = stdin
	go p.read(stdout)
	return p, nil
}

func (p *liveProc) read(stdout io.Reader) {
	defer func() {
		_, _ = io.Copy(io.Discard, stdout)
		close(p.lines)
		_ = p.cmd.Wait()
		close(p.exited)
	}()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		line = append([]byte(nil), line...)
		if eventKind(line) == "control_request" {
			p.refuseControl(line)
		}
		select {
		case p.lines <- line:
		case <-p.quit:
			return
		}
	}
}

// refuseControl answers a control request with an error, the reply the SDK
// gives a subtype it does not handle (_internal/query.py
// _handle_control_request), so the CLI never waits on orbit.
func (p *liveProc) refuseControl(line []byte) {
	var r struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if json.Unmarshal(line, &r) != nil || r.RequestID == "" {
		return
	}
	b, _ := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "error", "request_id": r.RequestID,
		"error": "orbit does not handle control requests (" + r.Request.Subtype + ")",
	}})
	_ = p.write(b)
}

func (p *liveProc) write(line []byte) error {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	_, err := p.stdin.Write(append(line, '\n'))
	return err
}

// stop ends the process: stdin closed (the CLI exits once its current turn
// ends), then the group killed after liveCloseGrace, or at once when kill.
func (p *liveProc) stop(kill bool) {
	p.once.Do(func() {
		close(p.quit)
		p.stdinMu.Lock()
		_ = p.stdin.Close()
		p.stdinMu.Unlock()
		pid := p.cmd.Process.Pid
		if !kill {
			select {
			case <-p.exited:
				return
			case <-time.After(liveCloseGrace):
			}
		}
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-p.exited:
		case <-time.After(liveCloseGrace):
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-p.exited
		}
	})
}

// turn sends one user message and returns the result line that ends its
// answer. It never waits past ctx: the caller drops the process on any
// error, which kills it.
func (p *liveProc) turn(ctx context.Context, prompt string, emit func(Event)) ([]byte, error) {
	msg, _ := json.Marshal(map[string]any{
		"type": "user", "message": map[string]any{"role": "user", "content": prompt},
		"parent_tool_use_id": nil, "session_id": "default",
	})
	if err := p.write(msg); err != nil {
		return nil, fmt.Errorf("claude live process: write the prompt: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case line, ok := <-p.lines:
			if !ok {
				select {
				case <-p.exited:
				case <-time.After(liveCloseGrace):
				}
				err := errors.New("claude exited during the turn")
				if tail := strings.TrimSpace(p.stderr.String()); tail != "" {
					err = fmt.Errorf("%w: %s", err, firstLine(lastLines(tail, 3)))
				}
				return nil, err
			}
			kind := eventKind(line)
			emit(Event{Kind: kind, Payload: line})
			if isResult(kind) {
				return line, nil
			}
		}
	}
}

// runLive takes the turn in a live process when one holds sess.Resume, or
// starts one when sess.Open. used is false when neither applies, and the
// caller runs a process for this run alone.
func (c *claudeCode) runLive(ctx context.Context, sess *Session, args, env []string, req Request, emit func(Event)) (final []byte, used bool, err error) {
	h := sha256.New()
	for _, part := range [][]string{{c.bin, req.Workdir}, args, env} {
		for _, a := range part {
			h.Write([]byte(a))
			h.Write([]byte{0})
		}
	}
	hash := hex.EncodeToString(h.Sum(nil))
	mode, parent := "live", sess.Resume
	p := sess.Live.take(sess.Key, hash, sess.Resume)
	if p == nil {
		if !sess.Open {
			return nil, false, nil
		}
		mode = "fresh"
		if sess.Resume != "" {
			mode = "resumed"
		}
		full := append(append(append([]string{}, args...), "--input-format", "stream-json"), resumeArgs(sess.Resume)...)
		if p, err = startLive(c.bin, full, env, req.Workdir); err != nil {
			return nil, true, err
		}
		p.key, p.hash, p.busy = sess.Key, hash, true
	}
	if err := sess.Started(mode, parent); err != nil {
		sess.Live.drop(p)
		return nil, true, err
	}
	final, err = p.turn(ctx, req.Prompt, emit)
	if err != nil {
		sess.Live.drop(p)
		return nil, true, err
	}
	var r struct {
		IsError          bool            `json:"is_error"`
		SessionID        string          `json:"session_id"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if json.Unmarshal(final, &r) != nil || r.IsError || r.SessionID == "" || len(r.StructuredOutput) == 0 || string(r.StructuredOutput) == "null" {
		sess.Live.drop(p)
		return final, true, nil
	}
	p.session = r.SessionID
	sess.Live.keep(p, sess.LiveIdle)
	return final, true, nil
}

// SessionFile is where Claude Code keeps a session's transcript:
// <config dir>/projects/<cwd with every character outside [A-Za-z0-9]
// replaced by "-">/<id>.jsonl, the config dir being $CLAUDE_CONFIG_DIR or
// $HOME/.claude (claude-agent-sdk 0.3.316, sdk.mjs: Et() is
// join(configDir, "projects"), Fu/nl the sanitizer). Names longer than 200
// characters get a hash suffix orbit does not reproduce, so ok is false for
// them. The CLI resolves symlinks in cwd first, as EvalSymlinks does.
func SessionFile(home, workdir, id string) (path string, ok bool) {
	if real, err := filepath.EvalSymlinks(workdir); err == nil {
		workdir = real
	}
	name := nonAlnum.ReplaceAllString(workdir, "-")
	if len(name) > 200 {
		return "", false
	}
	return filepath.Join(home, ".claude", "projects", name, id+".jsonl"), true
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)
