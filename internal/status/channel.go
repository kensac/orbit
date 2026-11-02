package status

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The channel between `orbit hook` and the run's listener is local: the
// hook runs inside the model's process tree, so it reaches orbit through
// a unix socket only the orbit user can open, never the network. When the
// socket does not answer (no listener between a live process's turns, a
// loaded host), the hook appends the event to a spool file in the run's
// state directory, which the listener reads while it runs and once more
// when it closes. Delivery is at least once; the listener drops repeats.

// SocketPath is the listener's socket for the state directory state. A
// unix socket path is limited to 104 bytes on macOS and 108 on Linux, and
// a state directory under ORBIT_DATA_DIR can be longer, so the socket is
// in the temporary directory, named for a hash of the state directory: one
// name for every run of a live process, whose hook command is fixed when
// the process starts.
func SocketPath(state string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(state)))
	return filepath.Join(os.TempDir(), "orbit-hook-"+hex.EncodeToString(sum[:8])+".sock")
}

// SpoolPath is the spool file in the state directory.
func SpoolPath(state string) string { return filepath.Join(state, "status-spool.jsonl") }

// postTimeout bounds one post: connecting to a local socket and its ack
// take well under a millisecond, so a second means the listener is stuck,
// and the event is spooled instead. It is far inside the hook's own
// timeout (runner.hookTimeout, 10 s).
const postTimeout = time.Second

// ack is the listener's reply to a line it accepted.
const ack = "ok\n"

// Post sends ev to the listener at sock, or appends it to spool when the
// listener does not acknowledge it. It returns an error only when neither
// worked; the event is then lost, and status falls back to the run's row.
func Post(sock, spool string, ev Event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if sock != "" && send(sock, line) == nil {
		return nil
	}
	if spool == "" {
		return errors.New("status: no listener and no spool")
	}
	return appendSpool(spool, line)
}

func send(sock string, line []byte) error {
	c, err := net.DialTimeout("unix", sock, postTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(postTimeout)); err != nil {
		return err
	}
	if _, err := c.Write(line); err != nil {
		return err
	}
	got := make([]byte, len(ack))
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if string(got) != ack {
		return errors.New("status: listener did not acknowledge")
	}
	return nil
}

// appendSpool writes line with one append: under PIPE_BUF it lands whole
// even when hooks run concurrently.
func appendSpool(spool string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(spool), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(spool, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // the run's own state directory
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// spoolPoll is how often a listener reads the spool while it runs: a hook
// spools only when the socket failed, so this is the latency of a
// fallback, not of the normal path.
const spoolPoll = time.Second

// maxLine bounds one line read from the socket or the spool; an Event's
// fields are clipped far below it.
const maxLine = 64 << 10

// Listener receives a run's hook events and hands each to its handler, one
// at a time, in arrival order.
type Listener struct {
	ln     net.Listener
	spool  string
	handle func(Event)

	mu   sync.Mutex // serializes handle and seen
	seen map[string]bool

	spoolMu sync.Mutex // one drain at a time; guards offset
	offset  int64

	stop    chan struct{}
	done    sync.WaitGroup
	closing sync.Once
}

// Listen serves sock and reads spool, calling handle for every event. A
// stale socket file (a listener that died) is replaced.
func Listen(sock, spool string, handle func(Event)) (*Listener, error) {
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	// Only orbit's own user may post; the hook runs as that user.
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	l := &Listener{ln: ln, spool: spool, handle: handle, seen: map[string]bool{}, stop: make(chan struct{})}
	l.drain()
	l.done.Add(2)
	go l.accept()
	go l.poll()
	return l, nil
}

// Close stops the listener, then reads the spool one last time, so every
// event a hook spooled before Close returns is handled. It may be called
// more than once, and on a nil Listener.
func (l *Listener) Close() {
	if l == nil {
		return
	}
	l.closing.Do(func() {
		close(l.stop)
		_ = l.ln.Close()
		l.done.Wait()
		l.drain()
	})
}

func (l *Listener) accept() {
	defer l.done.Done()
	var conns sync.WaitGroup
	defer conns.Wait()
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		conns.Add(1)
		go func() {
			defer conns.Done()
			l.serve(c)
		}()
	}
}

// serve reads one connection's lines, acknowledging each once handled.
func (l *Listener) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(postTimeout))
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 4096), maxLine)
	for sc.Scan() {
		l.deliver(sc.Bytes())
		if _, err := io.WriteString(c, ack); err != nil {
			return
		}
	}
}

func (l *Listener) poll() {
	defer l.done.Done()
	t := time.NewTicker(spoolPoll)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.drain()
		}
	}
}

// drain handles the spool's complete lines after the last read. A line a
// hook is still writing has no newline yet, and is read next time.
func (l *Listener) drain() {
	if l.spool == "" {
		return
	}
	l.spoolMu.Lock()
	defer l.spoolMu.Unlock()
	f, err := os.Open(l.spool)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(l.offset, io.SeekStart); err != nil {
		return
	}
	b, _ := io.ReadAll(io.LimitReader(f, 16<<20))
	end := bytes.LastIndexByte(b, '\n')
	if end < 0 {
		return
	}
	l.offset += int64(end + 1)
	for _, line := range bytes.Split(b[:end], []byte{'\n'}) {
		l.deliver(line)
	}
}

// deliver parses and handles one line, once: a post whose ack was lost is
// spooled too, and arrives twice.
func (l *Listener) deliver(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var ev Event
	if json.Unmarshal(line, &ev) != nil || ev.Name == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := string(line)
	if l.seen[key] {
		return
	}
	l.seen[key] = true
	l.handle(ev)
}
