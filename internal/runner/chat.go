package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/workspace"
)

// A chat run is a plain Claude Code session (docs/ARCHITECTURE.md, Chat
// agents): no --json-schema, the reply is the turn's final message (the
// result line's text), and orbit's own tools (memory, questions, actions)
// are served by orbit's process over the control channel as an SDK MCP
// server, so they work the same whether the CLI runs here or in a sandbox.
// Verified against the real CLI 2.1.316: a server declared
// {"type":"sdk","name":"orbit"} in --mcp-config is initialized, listed and
// called through mcp_message control requests, its tools named
// mcp__orbit__<name>.

// ChatServer names orbit's SDK MCP server; its tools are mcp__orbit__<name>.
const ChatServer = "orbit"

// ChatTool is one tool orbit serves a chat run.
type ChatTool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolName is a chat tool's name as the model sees it.
func ToolName(name string) string { return "mcp__" + ChatServer + "__" + name }

// Chat makes a claude-code run a chat run.
type Chat struct {
	Tools []ChatTool
	// Call runs one of Tools; the text goes back to the model, isError
	// marking a failure it should see as one.
	Call func(ctx context.Context, name string, args json.RawMessage) (text string, isError bool)
	// Sandbox, when set, runs the CLI in a sandboxd container over the
	// workdir instead of on this host.
	Sandbox *Sandbox
	// Push are workdir paths (files) this run added, copied into a live
	// sandbox before its turn; a new sandbox is packed whole.
	Push []string
	// Interrupt, when closed, interrupts the turn (a person pressed stop):
	// the CLI ends it with a result, and the run reports Interrupted.
	Interrupt <-chan struct{}
	// Fresh is a workspace a person changed since the last turn: a live
	// process (its sandbox's copy of the workspace, or a cwd the restore
	// replaced) is not reused, and the session resumes in a new one
	// started from the workdir as it is now.
	Fresh bool
	// Steer carries the person's messages for the turn while it runs
	// (steer.go); nil for none.
	Steer <-chan SteerMessage
	// Steered is told each steer message the turn took: from then on the
	// turn answers it.
	Steered func(id string)
	// Level is the run's permission level (spec.Levels, resolved by the engine), and Ceiling the widest its conversation may set: a process whose ceiling is full is launched so that it can be raised to it.
	Level, Ceiling string
	// CurrentLevel reports the process's current logical level while it is live.
	CurrentLevel func() string
	// ConnectorPrompts keeps explicit connector approvals available at the locked level.
	ConnectorPrompts bool
	// BeginPlanExit defers level changes until an ExitPlanMode request completes.
	BeginPlanExit func(toolUseID string)
	// WaitPlanExit blocks a later tool permission until deferred level changes apply.
	WaitPlanExit func(context.Context) bool
	// Choice is why the run is at Level, recorded with every application of it.
	Choice LevelChoice
	// Changes carries a new level for the turn in progress, or nil when it cannot change.
	Changes <-chan LevelChange
	// Prompt answers the CLI's permission requests during the turn; nil denies each one.
	Prompt Prompt
	// RecordPermission persists each decision before the CLI receives it.
	RecordPermission func(claudeproto.PermissionRequest, claudeproto.Permission) error
}

// SandboxClient is sandboxd's four operations (*sandbox.Client).
type SandboxClient interface {
	CreateProfile(ctx context.Context, runID, profile string, archive io.Reader) (sandbox.CreateResponse, error)
	Attach(ctx context.Context, id string, ar sandbox.AttachRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	Archive(ctx context.Context, id string) (io.ReadCloser, error)
	Destroy(ctx context.Context, id string) error
}

// Sandbox is where a sandboxed chat run's CLI runs, and how its workspace
// crosses the boundary. Only Env, the backend's one credential (build.SandboxEnv), enters it.
type Sandbox struct {
	Client SandboxClient
	Label  string
	Env    map[string]string
	// Profile is the sandbox's kind (spec.SandboxProfiles): a
	// "workstation" session gets every tool and a home kept in the
	// workspace.
	Profile string
	// Pack is the workspace a new sandbox starts from.
	Pack func() ([]byte, error)
	// PackFiles is a tar of some workdir files, for a live sandbox.
	PackFiles func(paths []string) ([]byte, error)
	// Unpack takes the workspace back after every turn.
	Unpack func(archive io.Reader) error
	// Binary is the CLI inside the sandbox; "claude" when empty.
	Binary string
	// Events records the run's sandbox steps (its creation).
	Events func(kind string, v any)
}

// SandboxConfigDir is the CLI's config directory inside a sandbox, in the
// workspace so a session's transcript is read back with it.
const SandboxConfigDir = sandbox.WorkDir + "/.orbit/claude"

// SandboxHome is a workstation sandbox's HOME, in the workspace so what the
// session installs or configures there is kept between sandboxes.
const SandboxHome = sandbox.WorkDir + "/.orbit/home"

// SandboxPlugin is where a sandboxed run's skills plugin is placed.
const SandboxPlugin = sandbox.WorkDir + "/.orbit/plugin"

// workstation is the profile whose session has Claude Code's full tool set.
const workstation = "workstation"

// chatSettings are a chat run's --settings. Local runs keep orbit's deny
// rules and the CLI's own workdir limit; a sandbox is the boundary for a
// sandboxed run, so the CLI there may do anything inside it.
func chatSettings(keepDays int, hooks map[string]any, sandboxed bool, ask []string) string {
	perms := map[string]any{"defaultMode": "dontAsk", "deny": []string{
		"Read(//proc/**)", "Read(//sys/**)", "Read(//run/**)", "Read(//etc/**)", "Read(//config/**)", "Read(~/**)",
	}}
	if len(ask) > 0 {
		// dontAsk would deny an ask rule unasked; in default mode it reaches orbit's prompt, which denies anything else asked about, as dontAsk does.
		perms["defaultMode"], perms["ask"] = "default", ask
	}
	if sandboxed {
		perms = map[string]any{"defaultMode": "bypassPermissions"}
		if len(ask) > 0 {
			perms["ask"] = ask
		}
	}
	s := map[string]any{
		"permissions":         perms,
		"disableAllHooks":     len(hooks) == 0,
		"includeCoAuthoredBy": false,
		"cleanupPeriodDays":   keepDays,
		"autoMemoryEnabled":   false,
	}
	if len(hooks) > 0 {
		s["hooks"] = hooks
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// chatArgs are a chat run's flags, without the transport's.
func (c *claudeCode) chatArgs(req Request, home string, keepDays int) []string {
	sandboxed := req.Chat.Sandbox != nil
	full := sandboxed && req.Chat.Sandbox.Profile == workstation
	tools := append([]string{}, req.Tools...)
	if !sandboxed {
		for _, t := range readTools {
			if !contains(tools, t) {
				tools = append(tools, t)
			}
		}
	}
	allowed := append([]string{}, req.Tools...)
	for _, t := range req.Chat.Tools {
		allowed = append(allowed, ToolName(t.Name))
	}
	var hooks map[string]any
	if req.Harness != nil && !sandboxed {
		hooks = harnessHooks(req.Harness, req.Workdir, home)
	}
	allowed = append(allowed, req.Connectors.Allowed()...)
	servers := map[string]any{ChatServer: map[string]any{"type": "sdk", "name": ChatServer}}
	req.Connectors.addServers(servers)
	mcp, _ := json.Marshal(map[string]any{"mcpServers": servers})
	if req.SkillsPlugin != "" && !full {
		tools = append(tools, "Skill")
		allowed = append(allowed, "Skill")
	}
	if full {
		// Every built-in tool: no --tools list, nothing to allow by name.
		allowed = allowed[len(req.Tools):]
	}
	args := []string{"--append-system-prompt", req.System}
	if !full {
		args = append(args, "--tools", strings.Join(tools, ","))
	}
	args = append(args,
		"--strict-mcp-config", "--mcp-config", string(mcp),
		"--setting-sources", "",
		"--settings", chatSettings(keepDays, hooks, sandboxed, req.Connectors.Ask()),
	)
	webDeny := []string(nil)
	if !sandboxed {
		allowed, webDeny = webRules(allowed, req.WebAllow)
	}
	args = append(args, "--allowedTools", strings.Join(allowed, ","))
	if len(webDeny) > 0 {
		args = append(args, "--disallowedTools", strings.Join(webDeny, ","))
	}
	mode := startModeForLevel(req.Chat.Level, len(req.Connectors.Ask()) > 0)
	args = append(args, "--permission-mode", mode)
	if req.Chat.Ceiling == spec.LevelFull && sandboxed {
		// Without it the CLI refuses set_permission_mode bypassPermissions, so a conversation could not be raised to full live.
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	if req.SkillsPlugin != "" {
		dir := req.SkillsPlugin
		if sandboxed {
			dir = SandboxPlugin
		}
		args = append(args, "--plugin-dir", dir)
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
	return args
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// chatProc is a CLI a chat run talks to: on this host, or in a sandbox.
type chatProc interface {
	Turn(ctx context.Context, prompt string, emit func(claudeproto.Line)) (*claudeproto.Result, error)
	Interrupt(ctx context.Context, cancelQueued bool) (*claudeproto.InterruptReceipt, error)
	SendUUID(text, uuid string) error
	CancelQueued(ctx context.Context, uuid string) (bool, error)
	Capable(name string) bool
	SetPermissionMode(ctx context.Context, mode string) error
	Close(grace time.Duration)
	Kill()
	Exited() <-chan struct{}
}

// chatTools routes a process's control requests to the turn in progress: a
// live process serves many runs, each with its own tools (orbit's, for a
// chat run) and connectors, and push copies a file into its sandbox.
//
// The connectors of the last turn stay listed between turns (the CLI may list a server at any time, and a reused process has the same ones, its arguments name their tools), but only a turn in progress has them called.
type chatTools struct {
	mu      sync.Mutex
	chat    *Chat
	ws      Workspace
	conns   *Connectors
	active  bool
	push    func(context.Context, []string) error
	observe func(claudeproto.Line)
}

func (t *chatTools) set(c *Chat, ws Workspace) {
	t.mu.Lock()
	t.chat, t.ws = c, ws
	t.mu.Unlock()
}

func (t *chatTools) setObserver(observe func(claudeproto.Line)) {
	t.mu.Lock()
	t.observe = observe
	t.mu.Unlock()
}

func (t *chatTools) observeLine(line claudeproto.Line) {
	t.mu.Lock()
	observe := t.observe
	t.mu.Unlock()
	if observe != nil {
		observe(line)
	}
}

// connectors routes the turn's connectors; nil ends the turn's calls, the last ones staying listed.
func (t *chatTools) connectors(conns *Connectors) {
	t.mu.Lock()
	t.active = conns != nil
	if conns != nil {
		t.conns = conns
	}
	t.mu.Unlock()
}

func (t *chatTools) current() (*Chat, Workspace, *Connectors, func(context.Context, []string) error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	conns := t.conns
	if conns != nil && !t.active {
		conns = &Connectors{Servers: conns.Servers}
	}
	return t.chat, t.ws, conns, t.push
}

// Workspace is the turn's workspace as orbit's tools reach it while the CLI
// runs: the sandbox's /work, or the workdir on this host.
type Workspace interface {
	// Put unpacks a tar under dir (relative to the workspace).
	Put(ctx context.Context, dir string, tar []byte) error
	// Get packs dir as a tar, entries relative to it.
	Get(ctx context.Context, dir string) ([]byte, error)
}

type workspaceKey struct{}

// WorkspaceOf is the workspace of the turn a chat tool call came from.
func WorkspaceOf(ctx context.Context) Workspace {
	ws, _ := ctx.Value(workspaceKey{}).(Workspace)
	return ws
}

// LocalWorkspace is a workspace directory on this host.
type LocalWorkspace string

// Put unpacks tar under dir.
func (l LocalWorkspace) Put(_ context.Context, dir string, tar []byte) error {
	_, err := workspace.Unpack(strings.NewReader(string(tar)), workspace.Into(filepath.Join(string(l), filepath.FromSlash(dir))))
	return err
}

// Get packs dir.
func (l LocalWorkspace) Get(_ context.Context, dir string) ([]byte, error) {
	p := filepath.Join(string(l), filepath.FromSlash(dir))
	if _, err := os.Stat(p); err != nil {
		return nil, err
	}
	tree, _, err := workspace.Pack(workspace.Tree{Dir: p})
	return tree, err
}

func (t *chatTools) handler() claudeproto.Handler {
	return claudeproto.Handler{Name: "orbit",
		Observe: t.observeLine,
		MCP: func(ctx context.Context, req claudeproto.MCPMessage) (json.RawMessage, error) {
			c, ws, conns, push := t.current()
			if req.ServerName == ChatServer {
				if ws != nil {
					ctx = context.WithValue(ctx, workspaceKey{}, ws)
				}
				return serveMCP(ctx, c, req)
			}
			if conns == nil {
				return nil, fmt.Errorf("orbit serves no MCP server %q to this run", req.ServerName)
			}
			return conns.serve(ctx, req.ServerName, req.Message, push)
		},
		CanUseTool: func(ctx context.Context, req claudeproto.PermissionRequest) (claudeproto.Permission, error) {
			c, _, conns, _ := t.current()
			if c == nil {
				if conns == nil {
					return claudeproto.Permission{}, errors.New("orbit does not handle can_use_tool requests in this run")
				}
				return conns.permission(ctx, req)
			}
			if req.Tool != "ExitPlanMode" && c.WaitPlanExit != nil && !c.WaitPlanExit(ctx) {
				return claudeproto.Permission{Message: "orbit stopped waiting for a plan approval"}, nil
			}
			if server, tool, ok := splitToolName(req.Tool); ok {
				if _, found := conns.find(server, tool); found {
					return conns.permission(ctx, req)
				}
			}
			if req.Tool == "ExitPlanMode" && c.BeginPlanExit != nil {
				c.BeginPlanExit(req.ToolUseID)
			}
			decide := noPrompt
			if c.Prompt != nil {
				decide = func(ctx context.Context, req claudeproto.PermissionRequest) claudeproto.Permission {
					level := c.Level
					if c.CurrentLevel != nil {
						level = c.CurrentLevel()
					}
					if spec.LevelRank(level) <= spec.LevelRank(spec.LevelLocked) {
						return noPrompt(ctx, req)
					}
					permission := c.Prompt(ctx, req)
					if c.CurrentLevel != nil && spec.LevelRank(c.CurrentLevel()) <= spec.LevelRank(spec.LevelLocked) {
						return noPrompt(ctx, req)
					}
					return permission
				}
			}
			return claudeproto.Recorded(decide, c.RecordPermission)(ctx, req)
		},
	}
}

// serveMCP answers one JSON-RPC message for orbit's SDK server, as the
// Agent SDK's in-process servers do.
func serveMCP(ctx context.Context, c *Chat, req claudeproto.MCPMessage) (json.RawMessage, error) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(req.Message, &m); err != nil {
		return nil, err
	}
	reply := func(result any) (json.RawMessage, error) {
		return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	}
	if len(m.ID) == 0 {
		// A notification: no reply, but the control request is answered.
		return json.RawMessage(`{"jsonrpc":"2.0","result":{}}`), nil
	}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		return reply(map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": ChatServer, "version": "1"}})
	case "tools/list":
		tools := []map[string]any{}
		if c != nil {
			for _, t := range c.Tools {
				tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
			}
		}
		return reply(map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, err
		}
		if c == nil || c.Call == nil {
			return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "orbit's tools are not available now"}}, "isError": true})
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage(`{}`)
		}
		text, isErr := c.Call(ctx, p.Name, p.Arguments)
		return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr})
	}
	return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID,
		"error": map[string]any{"code": -32601, "message": "method not found: " + m.Method}})
}

// runChat runs one chat turn: in the conversation's live process when one
// holds the session, else in a process (here or in a sandbox) started for
// it, kept live after when the session says so.
func (c *claudeCode) runChat(ctx context.Context, req Request, emit func(Event)) (*Result, error) {
	sess := req.Session
	home := filepath.Join(req.Workdir, ".home")
	keepDays := 1
	if sess != nil {
		home, keepDays = sess.Home, max(sess.KeepDays, 1)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	if sess != nil && sess.Attached != nil {
		emit = attachOnce(emit, sess.Attached)
	}
	extra := c.chatArgs(req, home, keepDays)
	env := c.env(home)
	resume := ""
	if sess != nil {
		resume = sess.Resume
	}
	// A live process moves between permission levels with set_permission_mode.
	key := hashArgs(c.bin, req.Workdir, withoutMode(extra), env)
	level, choice := req.Chat.Level, req.Chat.Choice

	var p *liveProc
	mode, parent := "fresh", resume
	if sess != nil && sess.Live != nil {
		if req.Chat.Fresh {
			sess.Live.discard(sess.Key)
		} else if p = sess.Live.take(sess.Key, key, resume); p != nil {
			mode = "live"
			targetMode := modeForLevel(level, len(req.Connectors.Ask()) > 0)
			if !p.lv.needs(level, targetMode) {
				emit(levelEvent(level, p.lv.applied(), p.lv.reportedMode(), "turn", "", choice))
			} else if err := p.lv.apply(ctx, p.proc, level, targetMode, "turn", choice, emit); err != nil {
				sess.Live.drop(p)
				p, mode = nil, "fresh"
			}
		}
	}
	if p == nil {
		if resume != "" {
			mode = "resumed"
		}
		tools := &chatTools{}
		tools.connectors(req.Connectors)
		// Every chat process has the stdio permission prompt, so a level that asks can be set without a new process; at locked or full the CLI sends none but ExitPlanMode's.
		args, err := claudeproto.Args(claudeproto.Transport{Stream: true, PermissionPrompt: true, Resume: resume, Ephemeral: sess == nil}, extra)
		if err != nil {
			return nil, err
		}
		var proc chatProc
		if req.Chat.Sandbox != nil {
			var sb *sandboxProc
			sb, err = startSandbox(ctx, req.Chat.Sandbox, args, tools.handler(), sess)
			if sb != nil {
				tools.push = sb.push
				proc = sb
			}
		} else {
			proc, err = claudeproto.Start(claudeproto.Command{Bin: c.bin, Args: args, Env: env, Dir: req.Workdir}, tools.handler())
		}
		if err != nil {
			return nil, err
		}
		p = &liveProc{proc: proc, tools: tools, busy: true}
		if sess != nil {
			p.key = sess.Key
		}
		p.hash = key
		first := level
		if level == spec.LevelAuto {
			first = spec.LevelAsk
		}
		firstMode := modeForLevel(first, len(req.Connectors.Ask()) > 0)
		p.lv.set(first, first, firstMode)
		if level != first {
			if err := p.lv.apply(ctx, p.proc, level, modeForLevel(level, len(req.Connectors.Ask()) > 0), "start", choice, emit); err != nil {
				dropOrKill(sess, p)
				return nil, fmt.Errorf("set the permission level: %w", err)
			}
		} else {
			emit(levelEvent(level, level, firstMode, "start", "", choice))
		}
	} else if sb, ok := p.proc.(*sandboxProc); ok && len(req.Chat.Push) > 0 {
		if err := sb.push(ctx, req.Chat.Push); err != nil {
			dropOrKill(sess, p)
			return nil, err
		}
	}
	if sess != nil && sess.Started != nil {
		if err := sess.Started(mode, parent); err != nil {
			dropOrKill(sess, p)
			return nil, err
		}
	}
	var ws Workspace = LocalWorkspace(req.Workdir)
	if sb, ok := p.proc.(*sandboxProc); ok {
		ws = sb
	}
	req.Chat.CurrentLevel = p.lv.safeLevel
	req.Chat.ConnectorPrompts = len(req.Connectors.Ask()) > 0
	req.Chat.BeginPlanExit = p.lv.beginPlanExit
	req.Chat.WaitPlanExit = p.lv.waitPlanExitDone
	p.tools.setObserver(p.lv.observe)
	p.tools.set(req.Chat, ws)
	p.tools.connectors(req.Connectors)
	defer func() { p.tools.set(nil, nil); p.tools.connectors(nil) }()

	var (
		imu     sync.Mutex
		stopped bool
		receipt *claudeproto.InterruptReceipt
	)
	turnCtx, endTurn := context.WithCancel(ctx)
	defer endTurn()
	if req.Chat.Interrupt != nil {
		go func() {
			select {
			case <-req.Chat.Interrupt:
				imu.Lock()
				stopped = true
				imu.Unlock()
				ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeproto.RequestTimeout)
				defer cancel()
				// Stop means stop: steer messages still queued go with the turn.
				r, err := p.proc.Interrupt(ictx, p.proc.Capable(claudeproto.CapInterruptCancelQueued))
				if err != nil {
					endTurn()
				}
				imu.Lock()
				receipt = r
				imu.Unlock()
			case <-turnCtx.Done():
			}
		}()
	}
	var steer *steering
	if req.Chat.Steer != nil {
		steer = newSteering(p.proc, req.Chat, emit)
		go steer.feed(turnCtx, req.Chat.Steer)
	}
	changed := watchLevel(ctx, turnCtx, p, req.Chat, emit)
	res, err := p.proc.Turn(turnCtx, req.Prompt, func(l claudeproto.Line) {
		if steer != nil {
			steer.observe(l)
		}
		emit(Event{Kind: l.Kind, Payload: l.Raw})
	})
	endTurn()
	<-changed
	imu.Lock()
	interrupted, rec := stopped, receipt
	imu.Unlock()
	// Every steer the turn did not take is withdrawn; one that may still
	// run means this process cannot be kept.
	clean := true
	if steer != nil {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claudeproto.RequestTimeout)
		clean = err != nil || steer.finish(wctx, rec)
		cancel()
	}
	if sb, ok := p.proc.(*sandboxProc); ok {
		// The workspace comes back after every turn, a failed one too:
		// what the agent wrote is the person's.
		if serr := sb.sync(context.WithoutCancel(ctx)); serr != nil && err == nil {
			err = fmt.Errorf("read the workspace back from the sandbox: %w", serr)
		}
	}
	if err != nil {
		dropOrKill(sess, p)
		if interrupted {
			return &Result{Interrupted: true}, nil
		}
		return nil, err
	}
	out := &Result{Reply: res.Result, Tokens: res.Tokens(), CostUSD: res.TotalCostUSD, SessionID: res.SessionID, Interrupted: interrupted}
	usage, _ := json.Marshal(map[string]any{"usage": res.Usage, "model_usage": res.ModelUsage, "num_turns": res.NumTurns})
	out.Usage = usage
	out.Output, _ = json.Marshal(map[string]any{"reply": res.Result})
	if sess != nil {
		sessionCost(out, sess.BaseCostUSD)
	}
	if res.IsError && !interrupted {
		dropOrKill(sess, p)
		return out, fmt.Errorf("%w: claude-code %s/%s: %s", ErrBackend, res.Subtype, res.TerminalReason, firstLine(res.Failure()))
	}
	if !clean {
		dropOrKill(sess, p)
		return out, nil
	}
	if interrupted || sess == nil || sess.Live == nil || res.SessionID == "" || (!sess.Open && mode != "live") {
		p.proc.Close(claudeproto.CloseGrace)
		return out, nil
	}
	p.session = res.SessionID
	sess.Live.keep(p, sess.LiveIdle)
	return out, nil
}

func dropOrKill(sess *Session, p *liveProc) {
	if sess != nil && sess.Live != nil {
		sess.Live.drop(p)
		return
	}
	p.proc.Kill()
}

func hashArgs(bin, workdir string, args, env []string) string {
	h := sha256.New()
	for _, part := range [][]string{{bin, workdir}, args, env} {
		for _, a := range part {
			h.Write([]byte(a))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sandboxProc is a CLI in a sandboxd container: its stdio is an attach
// stream, and the container is destroyed when the process is closed.
type sandboxProc struct {
	*claudeproto.Conn
	sb     *Sandbox
	id     string
	exited chan struct{}
	stderr *claudeproto.Tail
	cancel context.CancelFunc
	once   sync.Once
}

// startSandbox creates a sandbox from the workspace and attaches the CLI.
func startSandbox(ctx context.Context, sb *Sandbox, args []string, h claudeproto.Handler, sess *Session) (*sandboxProc, error) {
	tree, err := sb.Pack()
	if err != nil {
		return nil, fmt.Errorf("pack the workspace: %w", err)
	}
	cr, err := sb.Client.CreateProfile(ctx, sb.Label, sb.Profile, strings.NewReader(string(tree)))
	var se *sandbox.Error
	if errors.As(err, &se) && se.Status == 429 && sess != nil && sess.Live != nil {
		// Every sandbox slot is taken; idle live sandboxes give theirs up.
		sess.Live.closeIdle()
		cr, err = sb.Client.CreateProfile(ctx, sb.Label, sb.Profile, strings.NewReader(string(tree)))
	}
	if err != nil {
		return nil, fmt.Errorf("create the sandbox: %w", err)
	}
	if sb.Events != nil {
		sb.Events("orbit.sandbox", map[string]any{"step": "create", "id": cr.ID, "expires_at": cr.ExpiresAt, "bytes": len(tree)})
	}
	bin := sb.Binary
	if bin == "" {
		bin = "claude"
	}
	// The config directory is set by the shell: the environment a caller
	// may give a sandboxed process is the credential alone
	// (sandbox.AllowedEnv).
	script := `CFG=` + SandboxConfigDir + `; mkdir -p "$CFG" && CLAUDE_CONFIG_DIR="$CFG" exec "$@"`
	if sb.Profile == workstation {
		script = `CFG=` + SandboxConfigDir + `; H=` + SandboxHome + `; mkdir -p "$CFG" "$H" && HOME="$H" CLAUDE_CONFIG_DIR="$CFG" PATH="$H/.local/bin:$H/go/bin:$H/.npm-global/bin:$PATH" NPM_CONFIG_PREFIX="$H/.npm-global" exec "$@"`
	}
	argv := append([]string{"sh", "-c", script, "sh", bin}, args...)
	ar := sandbox.AttachRequest{Argv: argv, Env: sb.Env}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	actx, cancel := context.WithCancel(context.Background())
	p := &sandboxProc{sb: sb, id: cr.ID, exited: make(chan struct{}), stderr: &claudeproto.Tail{}, cancel: cancel}
	go func() {
		_, aerr := sb.Client.Attach(actx, cr.ID, ar, inR, outW, p.stderr)
		_ = outW.CloseWithError(aerr)
		_ = inR.Close()
		close(p.exited)
	}()
	p.Conn = claudeproto.NewConn(outR, inW, h)
	return p, nil
}

func (p *sandboxProc) Exited() <-chan struct{} { return p.exited }

// Turn is Conn.Turn, naming the CLI's exit as a process's would be.
func (p *sandboxProc) Turn(ctx context.Context, prompt string, emit func(claudeproto.Line)) (*claudeproto.Result, error) {
	res, err := p.Conn.Turn(ctx, prompt, emit)
	if errors.Is(err, claudeproto.ErrEnded) {
		e := errors.New("claude exited during the turn (in the sandbox)")
		if tail := strings.TrimSpace(p.stderr.String()); tail != "" {
			e = fmt.Errorf("%w: %s", e, claudeproto.LastLines(tail, 3))
		}
		return nil, e
	}
	return res, err
}

// sync reads the sandbox's workspace back.
func (p *sandboxProc) sync(ctx context.Context) error {
	rc, err := p.sb.Client.Archive(ctx, p.id)
	if err != nil {
		return err
	}
	defer rc.Close()
	return p.sb.Unpack(rc)
}

// push copies workdir files into the running sandbox: a tar unpacked by
// tar inside it, over an attach of its own.
func (p *sandboxProc) push(ctx context.Context, paths []string) error {
	if p.sb.PackFiles == nil {
		return nil
	}
	tree, err := p.sb.PackFiles(paths)
	if err != nil {
		return err
	}
	var stderr claudeproto.Tail
	code, err := p.sb.Client.Attach(ctx, p.id, sandbox.AttachRequest{Argv: []string{"tar", "-xf", "-", "-C", sandbox.WorkDir}},
		strings.NewReader(string(tree)), io.Discard, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("tar exited %d: %s", code, claudeproto.LastLines(stderr.String(), 2))
	}
	return err
}

// Put unpacks a tar under dir in the running sandbox, with tar inside it.
func (p *sandboxProc) Put(ctx context.Context, dir string, tree []byte) error {
	var stderr claudeproto.Tail
	code, err := p.sb.Client.Attach(ctx, p.id, sandbox.AttachRequest{Argv: []string{"sh", "-c", `mkdir -p "$1" && tar -xf - -C "$1"`, "sh", sandbox.WorkDir + "/" + dir}},
		strings.NewReader(string(tree)), io.Discard, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("tar exited %d: %s", code, claudeproto.LastLines(stderr.String(), 2))
	}
	return err
}

// Get packs dir of the running sandbox.
func (p *sandboxProc) Get(ctx context.Context, dir string) ([]byte, error) {
	var out strings.Builder
	var stderr claudeproto.Tail
	code, err := p.sb.Client.Attach(ctx, p.id, sandbox.AttachRequest{Argv: []string{"tar", "-cf", "-", "-C", sandbox.WorkDir + "/" + dir, "."}},
		nil, &out, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("tar exited %d: %s", code, claudeproto.LastLines(stderr.String(), 2))
	}
	return []byte(out.String()), err
}

// Close ends the CLI (stdin closed, it exits after its turn), then removes
// the sandbox.
func (p *sandboxProc) Close(grace time.Duration) { p.stop(grace) }

// Kill removes the sandbox at once.
func (p *sandboxProc) Kill() { p.stop(0) }

func (p *sandboxProc) stop(grace time.Duration) {
	p.once.Do(func() {
		_ = p.CloseInput()
		if grace > 0 {
			select {
			case <-p.exited:
			case <-time.After(grace):
			}
		}
		p.cancel()
		// A sandbox left behind ends at its lifetime and is reaped.
		_ = p.sb.Client.Destroy(context.Background(), p.id)
	})
	<-p.exited
}
