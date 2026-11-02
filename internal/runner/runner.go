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
	"time"

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
	// Session, when set, keeps the backend's conversation between runs
	// (claude-code only). Nil means a session that is never persisted.
	Session *Session
	// Harness, when set, gives a claude-code run orbit's tools (`orbit
	// mcp`) and hooks (`orbit hook`). Other backends ignore it.
	Harness *Harness
	// Chat, when set, makes a claude-code run a chat run (chat.go): no
	// OutputSchema, the reply its final message.
	Chat *Chat
}

// Harness is orbit's own code inside the CLI's process tree.
type Harness struct {
	// Bin is the orbit binary the CLI starts for tools and hooks.
	Bin string
	// Figures sends an output back when its money amounts cannot be traced
	// to the inputs or orbit's tool results (spec verify: [figures]).
	Figures bool
}

// Session is how one run uses a persistent Claude Code session. The engine
// decides whether to continue one; the runner decides how (a resumed copy,
// or a turn in a live process that already holds it).
type Session struct {
	// Home is the CLI's HOME, kept between runs: Claude Code stores the
	// session's transcript under it (see SessionFile).
	Home string
	// Resume is the session to continue; empty starts a fresh one.
	Resume string
	// BaseCostUSD is the backend's cumulative cost total saved with Resume.
	// The CLI reports a running total per session, so this run's cost is
	// the difference.
	BaseCostUSD float64
	// KeepDays is Claude Code's transcript retention (cleanupPeriodDays):
	// at least the resume window, so a session is never swept while it may
	// still be resumed.
	KeepDays int
	// Live, when set, may take the turn in a live process.
	Live *Live
	// Key names the live process (the conversation): one per key.
	Key string
	// Open starts a live process when none can take the turn, and keeps
	// it for LiveIdle after the turn.
	Open     bool
	LiveIdle time.Duration
	// Started is called once the runner knows how it will use the session
	// (mode fresh, resumed or live; parent the session continued), before
	// the prompt is sent. An error aborts the run.
	Started func(mode, parent string) error
	// Attached, when set, is called with the session the backend writes
	// to as soon as its stream names it (a fresh or forked session's new
	// id, or the live process's), so an attempt interrupted before its
	// result still says where its work is.
	Attached func(sessionID string)
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
	// SessionCostUSD is the backend's cumulative cost for the session, when
	// the run had one; CostUSD is then this run's share of it.
	SessionCostUSD *float64
	// Reply is a chat run's answer: the turn's final message.
	Reply string
	// Interrupted is a chat turn a person stopped (Chat.Interrupt).
	Interrupted bool
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
