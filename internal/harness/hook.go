package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/status"
)

// Claude Code runs a command hook with the event as JSON on stdin and reads
// a JSON decision from stdout. For PreToolUse the decision is
// hookSpecificOutput {hookEventName, permissionDecision allow|deny|ask,
// permissionDecisionReason}; a deny's reason is shown to the model, which
// may try again (sdk.d.ts: PreToolUseHookInput, BaseHookInput,
// PreToolUseHookSpecificOutput, SyncHookJSONOutput). Checked against the
// real CLI 2.1.316: hooks given with --settings run with --setting-sources ""
// and disableAllHooks false, and a deny on the StructuredOutput tool sends
// the model back to submit again.

// HookOptions is what orbit configured the hook with.
type HookOptions struct {
	// Root is the run's workdir: reads must stay under it.
	Root string
	// Figures checks the money amounts of the run's output (StructuredOutput).
	Figures bool
	// State is the directory the tool server and the hook share (the
	// ledger, the denial counts), given by orbit as a flag rather than read
	// from HOME, so nothing in the environment can move it.
	State string
	// Status is the run's status socket (status.SocketPath): every event is
	// posted there, or spooled under State when it does not answer.
	Status string
}

// MaxFigureDenials is how often one prompt's output is sent back for
// figures the check cannot trace. Two corrections suffice for a model with
// the query and calc tools; a third would turn a figure it legitimately
// took from its charter or memory into a failed run. After it the output
// passes and the engine notes the figures in the report.
const MaxFigureDenials = 2

type hookInput struct {
	Event     string          `json:"hook_event_name"`
	SessionID string          `json:"session_id"`
	PromptID  string          `json:"prompt_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// Hook reads one event, writes one decision, then reports the event to
// the run's status listener.
//
// The decision fails closed: an event it cannot read is denied, so a bug
// here never opens the sandbox. Every event that is not a PreToolUse gets
// `{}` (no decision) before anything else happens, and the report comes
// after the decision is written and can neither change it nor the exit
// status: the CLI reads a hook's exit code 2 as "block" (on Stop: keep
// working; on PreToolUse: deny), so a failure while reporting status must
// never surface as one. Reporting is best effort; status falls back to the
// run's row when it is lost.
func Hook(in io.Reader, out io.Writer, o HookOptions) error {
	b, err := io.ReadAll(io.LimitReader(in, maxLine))
	var ev hookInput
	if err == nil {
		err = json.Unmarshal(b, &ev)
	}
	if err != nil {
		return write(out, deny("PreToolUse", "orbit hook could not read the event: "+err.Error()))
	}
	werr := write(out, decide(ev, o))
	report(b, o)
	return werr
}

// decide is the hook's decision on ev: a PreToolUse guard, or none.
func decide(ev hookInput, o HookOptions) map[string]any {
	if ev.Event != "PreToolUse" {
		return map[string]any{}
	}
	switch ev.ToolName {
	case "Read", "Glob", "Grep":
		if reason := pathGuard(o.Root, ev.ToolName, ev.ToolInput); reason != "" {
			return deny(ev.Event, reason)
		}
	case "StructuredOutput":
		if o.Figures {
			if reason := figureCheck(o, ev); reason != "" {
				return deny(ev.Event, reason)
			}
		}
	}
	return map[string]any{}
}

// report posts the event to the run's status listener. Nothing it does
// reaches the hook's output or exit status.
func report(input []byte, o HookOptions) {
	if o.Status == "" {
		return
	}
	defer func() { _ = recover() }()
	ev, err := status.FromHook(input, time.Now())
	if err != nil {
		return
	}
	spool := ""
	if o.State != "" {
		spool = status.SpoolPath(o.State)
	}
	_ = status.Post(o.Status, spool, ev)
}

// maxLine bounds the event read: a StructuredOutput input is the whole
// report, well under this.
const maxLine = 16 << 20

func deny(event, reason string) map[string]any {
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": event, "permissionDecision": "deny", "permissionDecisionReason": reason,
	}}
}

func write(out io.Writer, v any) error { return json.NewEncoder(out).Encode(v) }

// pathGuard denies a file tool whose path leaves root, after symlinks.
// Claude Code already confines these tools to the cwd; this is the second
// wall, enforced by code orbit tests.
func pathGuard(root, tool string, input json.RawMessage) string {
	var in struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Pattern  string `json:"pattern"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "orbit: unreadable " + tool + " input"
	}
	paths := []string{in.FilePath, in.Path}
	if tool == "Glob" {
		paths = append(paths, globBase(in.Pattern))
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if !insideLoose(root, p) {
			return fmt.Sprintf("orbit: %s is outside the working directory; only your inputs there can be read", p)
		}
	}
	return ""
}

// globBase is the literal directory part of a glob pattern.
func globBase(pattern string) string {
	if i := strings.IndexAny(pattern, "*?[{"); i >= 0 {
		pattern = pattern[:i]
	}
	if pattern == "" {
		return ""
	}
	if strings.HasSuffix(pattern, "/") {
		return pattern
	}
	return filepath.Dir(pattern)
}

// insideLoose reports whether p, relative to root or absolute, stays under
// root once the part of it that exists has its symlinks resolved.
func insideLoose(root, p string) bool {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	if strings.HasPrefix(p, "~") {
		return false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(realRoot, p)
	}
	p = filepath.Clean(p)
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return inside(realRoot, filepath.Join(real, rest))
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// figureCheck sends the output back when a money amount in it is in none
// of the run's inputs or orbit tool results, up to MaxFigureDenials times
// per prompt.
func figureCheck(o HookOptions, ev hookInput) string {
	backed := NewBacked()
	_ = filepath.WalkDir(filepath.Join(o.Root, "inputs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil { //nolint:gosec // under the run's own inputs directory
			backed.AddJSON(b)
		}
		return nil
	})
	// The ledger, not the transcript: a run without a session persists
	// no transcript, and the ledger holds exactly orbit's own results.
	if lines, err := readLines(LedgerPath(o.State)); err == nil {
		for _, l := range lines {
			var e struct {
				Result string `json:"result"`
			}
			if json.Unmarshal(l, &e) == nil {
				backed.AddText(e.Result)
			}
		}
	}
	missing := backed.Unbacked(ReportText(ev.ToolInput))
	if len(missing) == 0 {
		return ""
	}
	if !o.countDenial(ev.SessionID + "-" + ev.PromptID) {
		return ""
	}
	return fmt.Sprintf("orbit: these amounts are not in your inputs or any result of orbit's query or calc tools: %s. "+
		"Compute every amount you report with %s or %s (exact), then submit again.",
		strings.Join(missing, ", "), ToolQuery, ToolCalc)
}

// countDenial records one more denial for key and reports whether it is
// within MaxFigureDenials.
func (o HookOptions) countDenial(key string) bool {
	if o.State == "" {
		return false
	}
	dir := o.State
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	path := filepath.Join(dir, "figures-"+strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '.' {
			return '_'
		}
		return r
	}, key))
	n := 0
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // the hook's own state file
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if n >= MaxFigureDenials {
		return false
	}
	return os.WriteFile(path, []byte(strconv.Itoa(n+1)), 0o600) == nil
}

// StateDir is where the hook and the tool server keep state for a run
// whose CLI HOME is home: outside the workdir, under the ~ the CLI denies
// reading. The engine empties it when a session agent's run starts; other
// runs get a new HOME.
func StateDir(home string) string { return filepath.Join(home, ".orbit-harness") }

// LedgerPath is the file the tool server appends each result to, one JSON
// line each, for the figure check.
func LedgerPath(state string) string { return filepath.Join(state, "ledger.jsonl") }

func readLines(path string) ([][]byte, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(path) //nolint:gosec // orbit's own ledger under the CLI's HOME
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		out = append(out, append([]byte(nil), sc.Bytes()...))
	}
	return out, sc.Err()
}
