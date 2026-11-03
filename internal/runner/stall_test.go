package runner

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// stallRun is a second turn of a live process under a watchdog; the first
// turn answers at once.
func stallRun(t *testing.T, onTurn string, stall *Stall, timeout time.Duration) (dir string, live *Live, events []Event, err error) {
	t.Helper()
	bin, dir := liveCLI(t, onTurn)
	stripStdinCapture(t, bin)
	r, _ := New("c", spec.RunnerSpec{Kind: "claude-code", Binary: bin}, orbitEnv)
	live = NewLive(MaxLive)
	t.Cleanup(live.Close)
	home, work := t.TempDir(), t.TempDir()
	if _, err := liveRun(t, r, live, context.Background(), "", home, work, time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var got started
	s := session(home, "live-1", 0, &got)
	s.Live, s.Open, s.LiveIdle = live, true, time.Minute
	_, err = r.Run(ctx, Request{Workdir: work, Prompt: "p", OutputSchema: []byte(`{}`), Session: s, Stall: stall},
		func(ev Event) { events = append(events, ev) })
	return dir, live, events, err
}

func TestAStalledLiveTurnIsInterruptedThenKilled(t *testing.T) {
	// Turn 2 hangs without a line; the fake never reads the interrupt,
	// so the process is killed after the grace.
	stall := &Stall{After: 150 * time.Millisecond, Grace: 200 * time.Millisecond,
		ToolOpen: func(context.Context) (bool, error) { return false, nil }}
	start := time.Now()
	dir, live, events, err := stallRun(t, `if [ $n -eq 2 ]; then sleep 60 & wait; fi`, stall, 20*time.Second)
	if !errors.Is(err, ErrStalled) || !stall.Fired() {
		t.Fatalf("err = %v, fired %v", err, stall.Fired())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the stall took %s to end", d)
	}
	if n := len(events); n == 0 || events[n-1].Kind != "orbit.stalled" {
		t.Fatalf("events = %+v, want orbit.stalled last", events)
	}
	if live.Len() != 0 {
		t.Fatal("a stalled process is still held")
	}
	pid, perr := strconv.Atoi(lines(t, filepath.Join(dir, "pids"))[0])
	if perr != nil {
		t.Fatal(perr)
	}
	eventually(t, "the stalled process to be killed", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	// The interrupt was sent on stdin before the kill.
	if turns := lines(t, filepath.Join(dir, "turns")); len(turns) < 2 {
		t.Fatalf("stdin = %v", turns)
	}
}

func TestKeepAlivesAndOpenToolsAreNoStall(t *testing.T) {
	keepAlive := `if [ $n -eq 2 ]; then for i in 1 2 3 4 5 6 7 8; do echo '{"type":"keep_alive"}'; sleep 0.05; done; fi`
	stall := &Stall{After: 150 * time.Millisecond, Grace: 100 * time.Millisecond,
		ToolOpen: func(context.Context) (bool, error) { return false, nil }}
	if _, _, _, err := stallRun(t, keepAlive, stall, 20*time.Second); err != nil || stall.Fired() {
		t.Fatalf("a turn writing keep_alive: %v, fired %v", err, stall.Fired())
	}
	open := &Stall{After: 100 * time.Millisecond, Grace: 100 * time.Millisecond,
		ToolOpen: func(context.Context) (bool, error) { return true, nil }}
	if _, _, _, err := stallRun(t, `if [ $n -eq 2 ]; then sleep 0.6; fi`, open, 20*time.Second); err != nil || open.Fired() {
		t.Fatalf("a silent turn with a tool running: %v, fired %v", err, open.Fired())
	}
	unknown := &Stall{After: 100 * time.Millisecond, Grace: 100 * time.Millisecond,
		ToolOpen: func(context.Context) (bool, error) { return false, errors.New("database down") }}
	if _, _, _, err := stallRun(t, `if [ $n -eq 2 ]; then sleep 0.6; fi`, unknown, 20*time.Second); err != nil || unknown.Fired() {
		t.Fatalf("a silent turn whose tools are unknown: %v, fired %v", err, unknown.Fired())
	}
}
