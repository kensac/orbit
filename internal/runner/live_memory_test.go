package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/claudeproto"
)

type pressureProcess struct {
	closed chan struct{}
	killed chan struct{}
	exited chan struct{}
	once   sync.Once
}

func newPressureProcess() *pressureProcess {
	return &pressureProcess{closed: make(chan struct{}), killed: make(chan struct{}), exited: make(chan struct{})}
}

func (p *pressureProcess) Turn(context.Context, string, func(claudeproto.Line)) (*claudeproto.Result, error) {
	return nil, nil
}
func (p *pressureProcess) SetModel(context.Context, string) error  { return nil }
func (p *pressureProcess) SetEffort(context.Context, string) error { return nil }
func (p *pressureProcess) Close(time.Duration) {
	p.once.Do(func() { close(p.closed); close(p.exited) })
}
func (p *pressureProcess) Kill() {
	p.once.Do(func() { close(p.killed); close(p.exited) })
}
func (p *pressureProcess) Exited() <-chan struct{} { return p.exited }
func (p *pressureProcess) Interrupt(context.Context, bool) (*claudeproto.InterruptReceipt, error) {
	return nil, nil
}
func (p *pressureProcess) SendUUID(string, string) error { return nil }
func (p *pressureProcess) CancelQueued(context.Context, string) (bool, error) {
	return false, nil
}
func (p *pressureProcess) Capable(string) bool { return false }
func (p *pressureProcess) LastLine() time.Time { return time.Now() }
func (p *pressureProcess) SetPermissionMode(context.Context, string) error {
	return nil
}

func TestLiveClosesIdleProcessesOnMemoryPressure(t *testing.T) {
	live := newLive(4, func() bool { return true }, 5*time.Millisecond)
	defer live.Close()

	idle := &liveProc{key: "idle", proc: newPressureProcess(), busy: true}
	active := &liveProc{key: "active", proc: newPressureProcess(), busy: true}
	live.keep(idle, time.Hour)
	live.keep(active, time.Hour)
	live.mu.Lock()
	active.busy = true
	live.mu.Unlock()

	eventually(t, "the idle process to close under pressure", func() bool {
		select {
		case <-idle.proc.Exited():
			return true
		default:
			return false
		}
	})
	select {
	case <-active.proc.Exited():
		t.Fatal("memory pressure closed an active process")
	default:
	}
	if live.Len() != 1 {
		t.Fatalf("live processes = %d, want only the active process", live.Len())
	}
}

func TestLiveHardExpiryKillsAnActiveProcess(t *testing.T) {
	live := newLive(4, nil, 0)
	defer live.Close()
	proc := newPressureProcess()
	p := &liveProc{key: "credential-bound", proc: proc, busy: true}
	live.setHardExpiry(p, time.Now().Add(50*time.Millisecond))
	live.mu.Lock()
	live.procs[p.key] = p
	live.mu.Unlock()

	eventually(t, "the credential-bound process to be killed at expiry", func() bool {
		select {
		case <-proc.killed:
			return true
		default:
			return false
		}
	})
	if live.Len() != 0 {
		t.Fatalf("expired process remains registered: %d", live.Len())
	}
}

func TestMemoryPressureThresholds(t *testing.T) {
	tests := []struct {
		name      string
		current   uint64
		limit     uint64
		available uint64
		total     uint64
		cgroup    bool
		want      bool
	}{
		{name: "cgroup below threshold", current: 84, limit: 100, cgroup: true},
		{name: "cgroup at threshold", current: 85, limit: 100, cgroup: true, want: true},
		{name: "host above threshold", available: 2 << 30, total: 8 << 30, want: false},
		{name: "host below absolute threshold", available: 1 << 30, total: 32 << 30, want: true},
		{name: "host below percentage threshold", available: 700 << 20, total: 8 << 30, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := memoryPressure(tt.current, tt.limit, tt.available, tt.total, tt.cgroup)
			if got != tt.want {
				t.Fatalf("memoryPressure() = %v, want %v", got, tt.want)
			}
		})
	}
}
