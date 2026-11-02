package engine

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestEventEmitterSerializesCompleteWrites(t *testing.T) {
	const count = 32
	entered := make(chan struct{})
	release := make(chan struct{})
	var active atomic.Int32
	var ordered []int
	var payloads []string
	events := &eventEmitter{capture: true, persist: func(seq int, kind string, payload []byte) error {
		if active.Add(1) != 1 {
			t.Error("overlapping persistence calls")
		}
		defer active.Add(-1)
		if seq == 1 {
			close(entered)
			<-release
		}
		ordered = append(ordered, seq)
		payloads = append(payloads, string(payload))
		return nil
	}}
	var wg sync.WaitGroup
	wg.Go(func() { events.emit("assistant", []byte("first")) })
	<-entered
	started := make(chan struct{}, count-1)
	for i := 1; i < count; i++ {
		wg.Go(func() { started <- struct{}{}; events.emit("assistant", fmt.Appendf(nil, "payload-%d", i)) })
	}
	for range count - 1 {
		<-started
	}
	close(release)
	wg.Wait()
	if len(ordered) != count {
		t.Fatalf("got %d writes", len(ordered))
	}
	for i, seq := range ordered {
		if seq != i+1 {
			t.Fatalf("writes out of order: %v", ordered)
		}
	}
	messages := events.messages()
	if len(messages) != count {
		t.Fatalf("got %d captured messages", len(messages))
	}
	for i, b := range messages {
		if string(b) != payloads[i] {
			t.Fatalf("capture/write order mismatch at %d", i)
		}
	}
}

func TestEventEmitterFailedWriteKeepsSequenceAndWarnsOutsideLock(t *testing.T) {
	failure := errors.New("persistence failed")
	var seqs []int
	var warned []error
	var events *eventEmitter
	events = &eventEmitter{capture: true, persist: func(seq int, _ string, _ []byte) error {
		seqs = append(seqs, seq)
		if seq == 1 {
			return failure
		}
		return nil
	}, warn: func(err error) {
		warned = append(warned, err)
		// Diagnostic callbacks can inspect the snapshot and emit diagnostics; neither operation may run under the persistence lock.
		if len(events.messages()) != 1 {
			t.Error("failed event lost its transcript")
		}
		events.emit("diagnostic", []byte(`{}`))
	}}
	events.emit("user", []byte(`{"text":"hello"}`))
	events.emit("result", []byte(`{}`))
	if fmt.Sprint(seqs) != "[1 2 3]" || len(warned) != 1 || !errors.Is(warned[0], failure) {
		t.Fatalf("sequences %v warnings %v", seqs, warned)
	}
}

func TestEventEmitterSnapshotsOwnMessageBytes(t *testing.T) {
	events := &eventEmitter{capture: true, persist: func(int, string, []byte) error { return nil }}
	payload := []byte("original")
	events.emit("assistant", payload)
	copy(payload, []byte("modified"))
	first := events.messages()
	if len(first) != 1 || !bytes.Equal(first[0], []byte("original")) {
		t.Fatalf("payload changed: %q", first)
	}
	copy(first[0], []byte("changed!"))
	if string(events.messages()[0]) != "original" {
		t.Fatal("snapshot changed retained transcript")
	}
	events.emit("result", []byte("ignored"))
	if len(events.messages()) != 1 {
		t.Fatal("captured a non-message event")
	}
	uncaptured := &eventEmitter{persist: func(int, string, []byte) error { return nil }}
	uncaptured.emit("assistant", []byte("not retained"))
	if len(uncaptured.messages()) != 0 {
		t.Fatal("retained transcript when capture disabled")
	}
}

func TestEventEmitterConcurrentSnapshots(t *testing.T) {
	events := &eventEmitter{capture: true, persist: func(int, string, []byte) error { return nil }}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			<-start
			for range 16 {
				events.emit("assistant", []byte("message"))
			}
		})
	}
	wg.Go(func() {
		<-start
		for range 256 {
			snapshot := events.messages()
			if len(snapshot) > 0 {
				snapshot[0][0] = '?'
			}
		}
	})
	close(start)
	wg.Wait()
	snapshot := events.messages()
	if len(snapshot) != 256 {
		t.Fatalf("got %d messages", len(snapshot))
	}
	for _, b := range snapshot {
		if string(b) != "message" {
			t.Fatalf("snapshot mutation reached retained bytes: %q", b)
		}
	}
}
