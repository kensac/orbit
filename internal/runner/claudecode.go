package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kensac/orbit/internal/harness"
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
// hooks are orbit's own; with --setting-sources "" no other settings file
// is read, so they are the only hooks the CLI runs, and with none
// disableAllHooks stays on.
func claudeSettings(keepDays int, hooks map[string]any) string {
	s := map[string]any{
		"permissions": map[string]any{
			"deny": []string{
				"Read(//proc/**)", "Read(//sys/**)", "Read(//run/**)", "Read(//etc/**)",
				"Read(//config/**)", "Read(~/**)",
			},
			"defaultMode": "dontAsk",
		},
		"disableAllHooks":     len(hooks) == 0,
		"includeCoAuthoredBy": false,
		"cleanupPeriodDays":   keepDays,
		// Auto memory is a model-written store under HOME that the CLI
		// loads into later sessions. An agent's memory is orbit's
		// memory_versions, kept and shown; a second, invisible one under a
		// persistent HOME would let one run steer the next unrecorded.
		"autoMemoryEnabled": false,
	}
	if len(hooks) > 0 {
		s["hooks"] = hooks
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// hookTimeout bounds one orbit hook call, in seconds (the CLI's unit). The
// hook reads at most the run's inputs; a second is ample, ten leaves room
// for a loaded host.
const hookTimeout = 10

// harnessHooks are the hooks orbit configures: a path guard on the file
// tools, and, with Figures, the figure check on the structured output.
// Matchers are tool names joined by |.
func harnessHooks(h *Harness, workdir, home string) map[string]any {
	cmd := func(extra string) []any {
		return []any{map[string]any{"type": "command", "timeout": hookTimeout,
			"command": shellQuote(h.Bin) + " hook --root " + shellQuote(workdir) +
				" --state " + shellQuote(harness.StateDir(home)) + extra}}
	}
	pre := []any{map[string]any{"matcher": "Read|Glob|Grep", "hooks": cmd("")}}
	if h.Figures {
		pre = append(pre, map[string]any{"matcher": "StructuredOutput", "hooks": cmd(" --figures")})
	}
	return map[string]any{"PreToolUse": pre}
}

// harnessMCP is the --mcp-config that starts `orbit mcp` for the run.
func harnessMCP(h *Harness, workdir, home string) string {
	args := []string{"mcp", "--root", workdir, "--state", harness.StateDir(home)}
	if h.Search != "" {
		args = append(args, "--search", h.Search)
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		harness.MCPServer: map[string]any{"type": "stdio", "command": h.Bin, "args": args},
	}})
	return string(b)
}

// shellQuote quotes s for the sh -c the CLI runs a hook command with.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// baseArgs is the invocation every run shares, without the prompt's
// transport or the session flags.
func (c *claudeCode) baseArgs(req Request, home string, keepDays int) []string {
	tools := append(append([]string{}, readTools...), req.Tools...)
	allowed := append([]string{}, req.Tools...)
	var hooks map[string]any
	if req.Harness != nil {
		// MCP tools are not built-ins, so --tools does not list them; in
		// dontAsk mode a tool runs only when allowed by name.
		allowed = append(allowed, harness.ToolQuery, harness.ToolCalc)
		if req.Harness.Search != "" {
			allowed = append(allowed, harness.ToolSearch)
		}
		hooks = harnessHooks(req.Harness, req.Workdir, home)
	}
	args := []string{
		"-p",
		"--output-format", "stream-json", "--verbose",
		"--json-schema", string(req.OutputSchema),
		"--append-system-prompt", req.System,
		"--tools", strings.Join(tools, ","),
		"--strict-mcp-config",
		"--setting-sources", "",
		"--settings", claudeSettings(keepDays, hooks),
	}
	if req.Harness != nil {
		args = append(args, "--mcp-config", harnessMCP(req.Harness, req.Workdir, home))
	}
	if len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
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
	args := c.baseArgs(req, home, keepDays)
	env := c.env(home)
	if sess == nil {
		args = append(args, "--no-session-persistence")
		final, err := c.oneShot(ctx, args, env, req, emit)
		if final == nil {
			return nil, err
		}
		return parseClaudeResult(final)
	}

	if sess.Attached != nil {
		emit = attachOnce(emit, sess.Attached)
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

// attachOnce passes events through and calls attached with the first
// session id the stream names. Every line of Claude Code's stream-json
// carries session_id; the first is the system init line.
func attachOnce(emit func(Event), attached func(string)) func(Event) {
	seen := false
	return func(ev Event) {
		if !seen {
			var h struct {
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(ev.Payload, &h) == nil && h.SessionID != "" {
				seen = true
				attached(h.SessionID)
			}
		}
		emit(ev)
	}
}

// TranscriptCost is the running cost total Claude Code last saved in a
// session's transcript: the CLI appends a cost-state line when its process
// exits, a SIGTERM included, and restores it on --resume, so a resumed
// session's reported total includes it. A process killed with SIGKILL saves
// none, and what it spent is unknown (ok false). Verified on 2.1.316.
func TranscriptCost(home, workdir, id string) (total float64, ok bool) {
	path, found := SessionFile(home, workdir, id)
	if !found {
		return 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"cost-state"`)) {
			var c struct {
				Type         string   `json:"type"`
				TotalCostUSD *float64 `json:"totalCostUSD"`
			}
			if json.Unmarshal(line, &c) == nil && c.Type == "cost-state" && c.TotalCostUSD != nil {
				total, ok = *c.TotalCostUSD, true
			}
		}
		if err != nil {
			return total, ok
		}
	}
}
