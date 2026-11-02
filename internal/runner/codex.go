package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/spec"
)

// codex runs `codex exec` headless with a read-only sandbox. Auth is
// CODEX_API_KEY / OPENAI_API_KEY, or a ChatGPT login in CODEX_HOME.
type codex struct {
	bin    string
	rs     spec.RunnerSpec
	getenv func(string) (string, bool)
}

var codexAuthEnv = []string{"CODEX_API_KEY", "OPENAI_API_KEY", "CODEX_HOME"}

func newCodex(rs spec.RunnerSpec, getenv func(string) (string, bool)) (Runner, error) {
	bin := rs.Binary
	if bin == "" {
		bin = "codex"
	}
	found := false
	for _, n := range codexAuthEnv {
		if v, ok := getenv(n); ok && v != "" {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("codex: set CODEX_API_KEY, OPENAI_API_KEY or CODEX_HOME")
	}
	return &codex{bin: bin, rs: rs, getenv: getenv}, nil
}

func (c *codex) Kind() string  { return "codex" }
func (c *codex) Model() string { return c.rs.Model }

func (c *codex) Run(ctx context.Context, req Request, emit func(Event)) (*Result, error) {
	if len(req.Tools) > 0 {
		// Codex's tools are its sandboxed shell; it has no per-tool grant
		// to map an agent's list onto. Refusing beats silently ignoring.
		return nil, fmt.Errorf("codex: agent tools %v are not supported; codex runs read-only with no extra tools", req.Tools)
	}
	home := filepath.Join(req.Workdir, ".home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	schemaPath, err := writeTemp(home, "schema-*.json", req.OutputSchema)
	if err != nil {
		return nil, err
	}
	lastPath := filepath.Join(home, "last-message.json")
	args := []string{
		"exec", "--json", "--ephemeral", "--skip-git-repo-check",
		"-s", "read-only", "-C", req.Workdir,
		"--output-schema", schemaPath,
		"-o", lastPath,
	}
	if c.rs.Model != "" {
		args = append(args, "-m", c.rs.Model)
	}
	args = append(args, "-")
	// Codex has no separate system prompt flag; the contract leads the prompt.
	prompt := req.System + "\n\n" + req.Prompt

	var usage json.RawMessage
	var threadID, failure string
	stderr, runErr := runCLI(ctx, c.bin, args, childEnv(home, c.getenv, codexAuthEnv...), req.Workdir, prompt, func(line []byte) {
		kind := claudeproto.ParseLine(line).Kind
		emit(Event{Kind: kind, Payload: line})
		var e struct {
			ThreadID string          `json:"thread_id"`
			Usage    json.RawMessage `json:"usage"`
			Message  string          `json:"message"`
			Error    struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(line, &e)
		switch kind {
		case "thread.started":
			threadID = e.ThreadID
		case "turn.completed":
			usage = e.Usage
		case "turn.failed":
			failure = e.Error.Message
		case "error":
			if failure == "" {
				failure = e.Message
			}
		}
	})
	res := &Result{Usage: usage, SessionID: threadID}
	if failure != "" {
		return res, fmt.Errorf("%w: codex: %s", ErrBackend, firstLine(failure))
	}
	if runErr != nil {
		return res, exitDetail(runErr, stderr)
	}
	out, err := os.ReadFile(lastPath)
	if err != nil {
		return res, fmt.Errorf("%w: codex finished without a final message: %w", ErrBackend, err)
	}
	out = []byte(strings.TrimSpace(string(out)))
	if !json.Valid(out) {
		return res, fmt.Errorf("%w: codex final message is not JSON: %s", ErrBackend, firstLine(string(out)))
	}
	res.Output = out
	return res, nil
}
