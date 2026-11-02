package build

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRealCLIPermissionProtocol runs the real Claude Code CLI, with the
// build's own flags and settings, against orbit's handler, on this host:
// ORBIT_REAL_CLAUDE=claude go test -run RealCLI ./internal/build/. It uses
// the CLI's own login (a keychain login works: HOME is the person's), so it
// never runs in CI. It checks what fakes cannot: that the CLI asks orbit
// for every Write and Bash call under the ask rules, honors an allow and a
// deny, and refuses deny-rule matches without asking.
func TestRealCLIPermissionProtocol(t *testing.T) {
	bin := os.Getenv("ORBIT_REAL_CLAUDE")
	if bin == "" {
		t.Skip("ORBIT_REAL_CLAUDE is not set")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "--quiet")
	policy := Policy{Root: dir, Protected: []string{"deploy/**"}, Commands: [][]string{{"ls"}, {"git", "status"}}}
	var mu sync.Mutex
	var decided []string
	perms := Permissions{Policy: policy, Record: func(r PermissionRequest, d Decision) error {
		mu.Lock()
		defer mu.Unlock()
		var in struct {
			FilePath string `json:"file_path"`
			Command  string `json:"command"`
		}
		_ = json.Unmarshal(r.Input, &in)
		decided = append(decided, r.Tool+" "+strings.TrimPrefix(in.FilePath, dir+"/")+in.Command+" "+map[bool]string{true: "allow", false: "deny"}[d.Allow])
		return nil
	}}
	var kinds []string
	cv := &conversation{perms: perms, emit: func(kind string, _ []byte) { kinds = append(kinds, kind) }}
	model := os.Getenv("ORBIT_REAL_CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	args := cliArgs(bin, model, 12, systemPrompt(policy, "build"))
	proc := func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, stdin, stdout, stderr
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	prompt := "This is a test of the permission system, not a real plan. Do exactly these steps, one tool call each, and continue after any refusal:\n" +
		"1. Write the file hello.txt containing hi.\n" +
		"2. Write the file .github/workflows/x.yml containing on: push.\n" +
		"3. Run the command: ls\n" +
		"4. Run the command: curl https://example.com\n" +
		"5. Write the file .git/orbit-test containing x.\n" +
		"6. Write the file deploy/x.txt containing x.\n" +
		"7. Run the command: git log\n" +
		"Then finish with a result whose summary lists which steps worked."
	res, err := cv.run(ctx, proc, prompt)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("decisions: %v", decided)
	t.Logf("denials: %s", res.PermissionDenials)
	t.Logf("output: %s", res.StructuredOutput)
	if b, err := os.ReadFile(filepath.Join(dir, "hello.txt")); err != nil || !strings.Contains(string(b), "hi") {
		t.Errorf("the allowed Write did not happen: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".github/workflows/x.yml")); err == nil {
		t.Error("the denied Write happened")
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy/x.txt")); err == nil {
		t.Error("the Write orbit denied happened")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git/orbit-test")); err == nil {
		t.Error("a write under .git happened")
	}
	joined := strings.Join(decided, "\n")
	for _, want := range []string{"Write hello.txt allow", "Write deploy/x.txt deny", "Bash ls allow", "Bash git log deny"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no decision %q", want)
		}
	}
	// Deny rules refuse before orbit is asked.
	for _, never := range []string{"curl", ".github", ".git/"} {
		if strings.Contains(joined, never) {
			t.Errorf("orbit was asked about %s, which a deny rule refuses first", never)
		}
		if !strings.Contains(string(res.PermissionDenials), never) {
			t.Errorf("the refusal of %s is not in permission_denials", never)
		}
	}
	if res.IsError || len(res.StructuredOutput) == 0 {
		t.Errorf("result: %s", res.raw)
	}
}
