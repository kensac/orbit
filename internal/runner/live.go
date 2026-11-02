package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/kensac/orbit/internal/claudeproto"
)

// A live process is `claude -p --input-format stream-json`: one CLI process that takes user messages on stdin, one JSON line each, and answers each with the same stream a one-shot run prints, ending in one `result` line per message. That line carries the turn's own structured_output (checked against the real CLI 2.1.316: three turns with --json-schema gave three results, each with its own structured_output and num_turns, and the second turn saw the first). The protocol is internal/claudeproto's.
//
// The CLI may send control requests of its own (permission prompts, hook callbacks, SDK MCP messages) and waits for a control_response. orbit configures none of their sources, so any that arrives is answered with an error at once rather than leaving the turn blocked.

// MaxLive bounds live processes per orbit process. Each is a resident CLI
// (about 265 MB RSS for 2.1.316 on macOS, measured idle after a turn); at
// most one per agent can be busy, and the
// executors (ORBIT_WORKERS, default 2) bound how many are. The least
// recently used idle one is closed past this.
const MaxLive = 4

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
// that turn first, within claudeproto.CloseGrace. orbit calls it once no run is
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
	proc    liveProcess
	// tools serves a chat process's orbit tools to the turn in progress.
	tools *chatTools
	busy  bool
	used  time.Time
	timer *time.Timer
}

// stop ends the process: closed between turns (stdin closed, so it exits on its own, within claudeproto.CloseGrace), or killed after a failed turn.
func (p *liveProc) stop(kill bool) {
	if kill {
		p.proc.Kill()
		return
	}
	p.proc.Close(claudeproto.CloseGrace)
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
	case <-p.proc.Exited():
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

// liveProcess is a live process: a local CLI (*claudeproto.Process) or one in
// a sandbox (chat.go).
type liveProcess interface {
	Turn(ctx context.Context, prompt string, emit func(claudeproto.Line)) (*claudeproto.Result, error)
	Close(grace time.Duration)
	Kill()
	Exited() <-chan struct{}
	Interrupt(ctx context.Context) error
	SetPermissionMode(ctx context.Context, mode string) error
}

// closeIdle closes every idle live process, for a sandbox slot or memory.
func (l *Live) closeIdle() {
	l.mu.Lock()
	var idle []*liveProc
	for k, p := range l.procs {
		if !p.busy {
			idle = append(idle, p)
			delete(l.procs, k)
		}
	}
	l.mu.Unlock()
	for _, p := range idle {
		p.stop(false)
	}
}

func (l *Live) async(f func()) {
	l.wg.Add(1)
	go func() { defer l.wg.Done(); f() }()
}

// startLive starts a process. Its Conn reads for the process's whole life and refuses every control request (claudeproto.Handler with no callbacks), so the CLI never waits on orbit.
func startLive(bin string, args, env []string, dir string) (*liveProc, error) {
	proc, err := claudeproto.Start(claudeproto.Command{Bin: bin, Args: args, Env: env, Dir: dir}, claudeproto.Handler{})
	if err != nil {
		return nil, err
	}
	return &liveProc{proc: proc}, nil
}

// turn sends one user message and returns the result line that ends its answer. It never waits past ctx: the caller drops the process on any error, which kills it.
func (p *liveProc) turn(ctx context.Context, prompt string, emit func(Event)) ([]byte, error) {
	res, err := p.proc.Turn(ctx, prompt, func(l claudeproto.Line) { emit(Event{Kind: l.Kind, Payload: l.Raw}) })
	if err != nil {
		return nil, err
	}
	return res.Raw, nil
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
		full, err := claudeproto.Args(claudeproto.Transport{Stream: true, Resume: sess.Resume}, args)
		if err != nil {
			return nil, true, err
		}
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
	r, perr := claudeproto.ParseResult(final)
	if perr != nil || r.IsError || r.SessionID == "" || !r.HasOutput() {
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
