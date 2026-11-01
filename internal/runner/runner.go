// Package runner is the boundary between orbit and a model backend. A runner
// takes one fully built request (prompt, workdir, tools, output schema) and
// returns the structured output the schema describes. Everything an agent
// means (charter, memory, inbox, rules) is resolved before a runner sees it,
// so swapping Claude Code for Codex or a plain API call changes nothing
// above this package.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kensac/orbit/internal/spec"
)

// Input is one fetched source, already written to Workdir/Path.
type Input struct {
	Name      string
	Path      string // relative to Workdir
	MediaType string
	Content   []byte
}

// Request is one run's call into a backend.
type Request struct {
	RunID string
	// Workdir is the run's private directory and the CLI's cwd. Inputs are
	// under it; nothing else on the host is.
	Workdir string
	// System is orbit's contract: what the output means and how to behave.
	System string
	// Prompt is the run's content: charter, wake reason, memory, inbox.
	Prompt string
	Inputs []Input
	// Tools the backend may use. Empty means none: the agent reads its
	// inputs from the prompt (API runners) or with read-only file tools the
	// CLI runner always grants inside Workdir.
	Tools        []string
	OutputSchema json.RawMessage
}

// Event is one line of the backend's stream, kept as the run's transcript.
type Event struct {
	Kind    string
	Payload []byte
}

// Result is a completed backend call.
type Result struct {
	// Output is the JSON the schema describes, unvalidated: the engine
	// validates it, so every runner is held to the same check.
	Output    json.RawMessage
	Usage     json.RawMessage
	CostUSD   *float64
	SessionID string
	Model     string
}

// Runner is a model backend.
type Runner interface {
	Kind() string
	Model() string
	Run(ctx context.Context, req Request, emit func(Event)) (*Result, error)
}

// ErrBackend marks a failure the backend reported about itself (auth, rate
// limit, refusal), as opposed to orbit failing to run it.
var ErrBackend = errors.New("backend error")

// New builds the runner a spec names. Credentials are read from the
// environment here, at construction, so a missing one fails the run before
// any input is fetched.
func New(name string, rs spec.RunnerSpec, getenv func(string) (string, bool)) (Runner, error) {
	switch rs.Kind {
	case "claude-code":
		return newClaudeCode(rs, getenv)
	case "codex":
		return newCodex(rs, getenv)
	case "anthropic":
		return newAnthropic(rs, getenv)
	}
	return nil, fmt.Errorf("runner %q: unknown kind %q", name, rs.Kind)
}

// childEnv is the environment a CLI backend runs with: an allowlist, never
// orbit's own environment. orbit's environment holds every source and
// channel credential; the model's process gets only what it needs to reach
// its own backend.
func childEnv(home string, getenv func(string) (string, bool), names ...string) []string {
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	for _, n := range names {
		if v, ok := getenv(n); ok && v != "" {
			env = append(env, n+"="+v)
		}
	}
	return env
}

// firstLine trims a backend message for an error string.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
