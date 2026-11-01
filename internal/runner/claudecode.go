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

// claudeAuthEnv are the only orbit environment variables the CLI sees.
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

// settings is passed with --settings. Deny rules win over everything, so
// these hold even for a tool an agent's spec allows.
func claudeSettings() string {
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
		"cleanupPeriodDays":   1,
	}
	b, _ := json.Marshal(s)
	return string(b)
}

func (c *claudeCode) Run(ctx context.Context, req Request, emit func(Event)) (*Result, error) {
	home := filepath.Join(req.Workdir, ".home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	tools := append(append([]string{}, readTools...), req.Tools...)
	args := []string{
		"-p",
		"--output-format", "stream-json", "--verbose",
		"--json-schema", string(req.OutputSchema),
		"--append-system-prompt", req.System,
		"--tools", strings.Join(tools, ","),
		"--strict-mcp-config",
		"--setting-sources", "",
		"--settings", claudeSettings(),
		"--no-session-persistence",
	}
	if len(req.Tools) > 0 {
		args = append(args, "--allowedTools", strings.Join(req.Tools, ","))
	}
	if c.rs.Model != "" {
		args = append(args, "--model", c.rs.Model)
	}
	if c.rs.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(c.rs.MaxTurns))
	}
	if c.rs.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(c.rs.MaxBudgetUSD, 'f', -1, 64))
	}
	env := childEnv(home, c.getenv, claudeAuthEnv...)
	env = append(env,
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_TELEMETRY=1",
	)

	var final []byte
	stderr, err := runCLI(ctx, c.bin, args, env, req.Workdir, req.Prompt, func(line []byte) {
		kind := eventKind(line)
		emit(Event{Kind: kind, Payload: line})
		if kind == "result" || strings.HasPrefix(kind, "result.") {
			final = line
		}
	})
	if final == nil {
		if err == nil {
			err = fmt.Errorf("claude exited without a result")
		}
		return nil, exitDetail(err, stderr)
	}
	return parseClaudeResult(final)
}

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
