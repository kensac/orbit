package claudeproto

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary is its own fake CLI: with fakeMode set it acts as one instead of running tests, so a fake needs no shell or script on disk.
const fakeMode = "CLAUDEPROTO_FAKE_CLI"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeMode); mode != "" {
		fakeCLI(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeCLI(mode string) {
	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	switch mode {
	case "turns":
		_ = out.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "s1"})
		n := 0
		for in.Scan() {
			var msg struct {
				Type    string `json:"type"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(in.Bytes(), &msg) != nil || msg.Type != "user" {
				continue
			}
			n++
			_ = out.Encode(map[string]any{"type": "assistant", "session_id": "s1"})
			_ = out.Encode(map[string]any{"type": "result", "subtype": "success", "session_id": "s1",
				"num_turns": n, "result": strings.Join(os.Environ(), "\n"),
				"structured_output": map[string]any{"echo": msg.Message.Content}})
		}
	case "crash":
		in.Scan()
		fmt.Fprintln(os.Stderr, "warming up")
		fmt.Fprintln(os.Stderr, "fatal: the model is unavailable")
		os.Exit(3)
	case "stubborn":
		signal.Ignore(syscall.SIGTERM)
		child := exec.Command("/bin/sleep", "60")
		_ = child.Start()
		_ = out.Encode(map[string]any{"type": "system", "subtype": "init", "pid": child.Process.Pid})
		for {
			time.Sleep(time.Hour)
		}
	}
}

func fake(t *testing.T, mode string, env ...string) *Process {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Start(Command{Bin: self, Env: append([]string{fakeMode + "=" + mode}, env...), Dir: t.TempDir()}, Handler{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close(time.Second) })
	return p
}

func TestAProcessTakesTurnsAndSeesOnlyItsEnvironment(t *testing.T) {
	t.Setenv("ORBIT_SECRET", "s3cret")
	p := fake(t, "turns", "HOME=/nowhere")
	for i, prompt := range []string{"one", "two"} {
		r, err := p.Turn(ctx(t), prompt, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.NumTurns != i+1 || string(r.StructuredOutput) != fmt.Sprintf(`{"echo":%q}`, prompt) {
			t.Fatalf("turn %d: %s", i, r.Raw)
		}
		env := strings.Split(r.Result, "\n")
		if strings.Contains(r.Result, "s3cret") || len(env) != 2 {
			t.Fatalf("the process saw %q, want only its own two variables", env)
		}
	}
	p.Close(5 * time.Second)
	select {
	case <-p.Exited():
	default:
		t.Fatal("closing stdin did not end the process")
	}
}

func TestANilEnvironmentInheritsNothing(t *testing.T) {
	t.Setenv("ORBIT_SECRET", "s3cret")
	p, err := Start(Command{Bin: "/usr/bin/env"}, Handler{})
	if err != nil {
		t.Fatal(err)
	}
	for {
		l, err := p.Next(ctx(t))
		if err != nil {
			break
		}
		if strings.Contains(string(l.Raw), "s3cret") {
			t.Fatalf("inherited %s", l.Raw)
		}
	}
	p.Close(time.Second)
}

func TestACrashMidTurnNamesStderr(t *testing.T) {
	p := fake(t, "crash")
	_, err := p.Turn(ctx(t), "hi", nil)
	if err == nil || err.Error() != "claude exited during the turn: warming up | fatal: the model is unavailable" {
		t.Fatalf("err = %v", err)
	}
}

func TestCloseKillsAProcessThatWillNotExit(t *testing.T) {
	p := fake(t, "stubborn")
	l, err := p.Next(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	var init struct {
		Pid int `json:"pid"`
	}
	_ = json.Unmarshal(l.Raw, &init)
	start := time.Now()
	p.Close(100 * time.Millisecond)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("close took %s", d)
	}
	// The group went with it: the tool the CLI started is gone too.
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(init.Pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the CLI's child %d outlived it", init.Pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := p.Turn(context.Background(), "x", nil); err == nil {
		t.Fatal("a turn on a closed process")
	}
}
