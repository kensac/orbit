package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kensac/orbit/internal/spec"
)

// claudeCode runs `claude -p` headless. Auth is CLAUDE_CODE_OAUTH_TOKEN (a
// subscription token from `claude setup-token`) or ANTHROPIC_API_KEY.
type claudeCode struct {
	bin    string
	rs     spec.RunnerSpec
	getenv func(string) (string, bool)
}

// readTools are granted to every Claude Code run so it can read its inputs.
// They are deliberately not passed as --allowedTools: unallowed, Claude Code
// permits them inside the working directory only and, headless, denies the
// rest, so the model cannot read orbit's own files or /proc.
var readTools = []string{"Read", "Glob", "Grep"}

// claudeAuthEnv are the credentials the CLI accepts; env hands it one.
var claudeAuthEnv = []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}

func newClaudeCode(rs spec.RunnerSpec, getenv func(string) (string, bool)) (Runner, error) {
	bin := rs.Binary
	if bin == "" {
		bin = "claude"
	}
	found := false
	for _, n := range claudeAuthEnv {
		if v, ok := getenv(n); ok && v != "" {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("claude-code: set CLAUDE_CODE_OAUTH_TOKEN (claude setup-token) or ANTHROPIC_API_KEY")
	}
	return &claudeCode{bin: bin, rs: rs, getenv: getenv}, nil
}

func (c *claudeCode) Kind() string  { return "claude-code" }
func (c *claudeCode) Model() string { return c.rs.Model }

// claudeSettings is passed with --settings. Deny rules win over everything,
// so these hold even for a tool an agent's spec allows. keepDays is the
// transcript retention: 1 for runs whose session is never persisted.
func claudeSettings(keepDays int) string {
	s := map[string]any{
		"permissions": map[string]any{
			"deny": []string{
				"Read(//proc/**)", "Read(//sys/**)", "Read(//run/**)", "Read(//etc/**)",
				"Read(//config/**)", "Read(~/**)",
			},
			"defaultMode": "dontAsk",
		},
		"disableAllHooks":     true,
		"includeCoAuthoredBy": false,
		"cleanupPeriodDays":   keepDays,
		// Auto memory is a model-written store under HOME that the CLI
		// loads into later sessions. An agent's memory is orbit's
		// memory_versions, kept and shown; a second, invisible one under a
		// persistent HOME would let one run steer the next unrecorded.
		"autoMemoryEnabled": false,
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// baseArgs is the invocation every run shares, without the prompt's
// transport or the session flags.
func (c *claudeCode) baseArgs(req Request, keepDays int) []string {
	tools := append(append([]string{}, readTools...), req.Tools...)
	args := []string{
		"-p",
		"--output-format", "stream-json", "--verbose",
		"--json-schema", string(req.OutputSchema),
		"--append-system-prompt", req.System,
		"--tools", strings.Join(tools, ","),
		"--strict-mcp-config",
		"--setting-sources", "",
		"--settings", claudeSettings(keepDays),
	}
	if len(req.Tools) > 0 {
		args = append(args, "--allowedTools", strings.Join(req.Tools, ","))
	}
	if c.rs.Model != "" {
		args = append(args, "--model", c.rs.Model)
	}
	if c.rs.MaxTurns > 0 {
		// Per user message, also in a live process: each turn of one
		// reported its own num_turns against the same limit.
		args = append(args, "--max-turns", strconv.Itoa(c.rs.MaxTurns))
	}
	if c.rs.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(c.rs.MaxBudgetUSD, 'f', -1, 64))
	}
	return args
}

// resumeArgs continue a session. Every resume forks (a new session id, the
// parent's transcript copied), so an attempt that is interrupted or fails
// leaves the session it continued exactly as the last good run left it.
func resumeArgs(id string) []string {
	if id == "" {
		return nil
	}
	return []string{"--resume", id, "--fork-session"}
}

// env gives the CLI one credential. With a subscription token present the
// API key is withheld: an API key the CLI is handed takes precedence over
// the subscription and is billed per token, so a stray key in orbit's
// environment must never turn subscription runs into API spend.
// CLAUDE_CONFIG_DIR is pinned to HOME's .claude, where SessionFile looks,
// whatever the CLI's default becomes.
func (c *claudeCode) env(home string) []string {
	oauth, apiKey := claudeAuthEnv[0], claudeAuthEnv[1]
	cred := apiKey
	if v, ok := c.getenv(oauth); ok && v != "" {
		cred = oauth
	}
	env := childEnv(home, c.getenv, cred)
	return append(env,
		"CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"),
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_TELEMETRY=1",
	)
}

func (c *claudeCode) Run(ctx context.Context, req Request, emit func(Event)) (*Result, error) {
	sess := req.Session
	home := filepath.Join(req.Workdir, ".home")
	keepDays := 1
	if sess != nil {
		home = sess.Home
		keepDays = max(sess.KeepDays, 1)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	args := c.baseArgs(req, keepDays)
	env := c.env(home)
	if sess == nil {
		args = append(args, "--no-session-persistence")
		final, err := c.oneShot(ctx, args, env, req, emit)
		if final == nil {
			return nil, err
		}
		return parseClaudeResult(final)
	}

	var final []byte
	var err error
	if sess.Live != nil {
		var used bool
		final, used, err = c.runLive(ctx, sess, args, env, req, emit)
		if !used {
			final, err = c.resumed(ctx, sess, args, env, req, emit)
		}
	} else {
		final, err = c.resumed(ctx, sess, args, env, req, emit)
	}
	if final == nil {
		return nil, err
	}
	res, perr := parseClaudeResult(final)
	if res != nil {
		sessionCost(res, sess.BaseCostUSD)
	}
	return res, perr
}

// sessionCost turns the CLI's cumulative session total into this run's
// share. The total is a running figure across turns and across resumes and
// forks of one session (sdk.d.ts, SDKResultMessage.total_cost_usd); a total
// below the base means the CLI started counting again, so it is all ours.
func sessionCost(res *Result, base float64) {
	if res.CostUSD == nil {
		return
	}
	total := *res.CostUSD
	res.SessionCostUSD = &total
	if total >= base {
		d := total - base
		res.CostUSD = &d
	}
}

// resumed runs one process for this run, continuing sess.Resume if set.
func (c *claudeCode) resumed(ctx context.Context, sess *Session, args, env []string, req Request, emit func(Event)) ([]byte, error) {
	mode := "fresh"
	if sess.Resume != "" {
		mode = "resumed"
	}
	if err := sess.Started(mode, sess.Resume); err != nil {
		return nil, err
	}
	return c.oneShot(ctx, append(args, resumeArgs(sess.Resume)...), env, req, emit)
}

// oneShot runs the CLI with the prompt on stdin and returns its result line,
// or nil and why there was none.
func (c *claudeCode) oneShot(ctx context.Context, args, env []string, req Request, emit func(Event)) ([]byte, error) {
	var final []byte
	stderr, err := runCLI(ctx, c.bin, args, env, req.Workdir, req.Prompt, func(line []byte) {
		kind := eventKind(line)
		emit(Event{Kind: kind, Payload: line})
		if isResult(kind) {
			final = line
		}
	})
	if final == nil {
		if err == nil {
			err = fmt.Errorf("claude exited without a result")
		}
		return nil, exitDetail(err, stderr)
	}
	return final, nil
}

func isResult(kind string) bool { return kind == "result" || strings.HasPrefix(kind, "result.") }

// parseClaudeResult reads the stream's final `result` line. is_error is the
// field to trust: a failed login reports subtype "success" with is_error.
func parseClaudeResult(line []byte) (*Result, error) {
	var r struct {
		IsError          bool            `json:"is_error"`
		Subtype          string          `json:"subtype"`
		Result           string          `json:"result"`
		TerminalReason   string          `json:"terminal_reason"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		TotalCostUSD     *float64        `json:"total_cost_usd"`
		Usage            json.RawMessage `json:"usage"`
		ModelUsage       json.RawMessage `json:"modelUsage"`
		SessionID        string          `json:"session_id"`
		NumTurns         int             `json:"num_turns"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, fmt.Errorf("parse claude result: %w", err)
	}
	usage, _ := json.Marshal(map[string]any{
		"usage": r.Usage, "model_usage": r.ModelUsage, "num_turns": r.NumTurns,
	})
	res := &Result{Usage: usage, CostUSD: r.TotalCostUSD, SessionID: r.SessionID}
	if r.IsError {
		return res, fmt.Errorf("%w: claude-code %s/%s: %s", ErrBackend, r.Subtype, r.TerminalReason, firstLine(r.Result))
	}
	if len(r.StructuredOutput) == 0 || string(r.StructuredOutput) == "null" {
		return res, fmt.Errorf("%w: claude-code finished (%s) without structured output: %s", ErrBackend, r.TerminalReason, firstLine(r.Result))
	}
	res.Output = r.StructuredOutput
	return res, nil
}
