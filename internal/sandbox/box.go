package sandbox

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// Boxes: an agent's persistent workstation (docs/workspace.md, The box).
// One container per agent, from the workstation image, with named volumes
// for its home and /work, kept until it is stopped. sandboxd exposes:
//
//	POST /v1/boxes/{name}                          body: EnsureRequest (optional); creates the box, or starts it; BoxStatus
//	GET  /v1/boxes/{name}                          BoxStatus (state "none" when there is none)
//	POST /v1/boxes/{name}/stop                     stops it (volumes kept); BoxStatus
//	POST /v1/boxes/{name}/recreate                 body: EnsureRequest; replaces the container (volumes kept); BoxStatus
//	POST /v1/boxes/{name}/tty                      body: TTYRequest; upgrades to a framed TTY stream
//	GET  /v1/boxes/{name}/sessions/{session}       {"exists": bool}
//	POST /v1/boxes/{name}/sessions/{session}/kill  ends that tmux session; 204
//	POST /v1/boxes/{name}/accounts/{slot}/prepare  links the shared profile into an account's directory; 204
//	GET  /v1/boxes/{name}/accounts/{slot}          AccountStatus, read from the account's own files
//
// As for sandboxes, no request names a docker option: a TTY's argv comes
// from a fixed preset (TTYShell, TTYClaude, TTYLogin), a tmux session name
// and an account's slot, and every other command is a fixed script.
const (
	PathBoxes = "/v1/boxes"
	// TTYUpgrade is the upgrade token of the TTY stream.
	TTYUpgrade = "orbit-box-tty"
	// BoxHome is the box user's HOME, a named volume of its own.
	BoxHome = "/home/agent"
	// AccountsRoot holds one directory per Claude account, each its own
	// named volume and the CLAUDE_CONFIG_DIR of the terminals that use it.
	AccountsRoot = BoxHome + "/.claude-accounts"
	// MaxAccounts bounds the accounts one box mounts.
	MaxAccounts = 16
)

var slotRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ValidSlot reports whether s can name an account's volume and directory:
// twelve lowercase hex digits, which orbit derives from the account's id.
func ValidSlot(s string) bool { return slotRE.MatchString(s) }

// AccountDir is an account's directory in the box.
func AccountDir(slot string) string { return AccountsRoot + "/" + slot }

// EnsureRequest names the accounts a box mounts. A stopped box missing one
// is recreated with it; a running one is reported as it is (BoxStatus
// Accounts), since adding a mount means replacing the container.
type EnsureRequest struct {
	Accounts []string `json:"accounts,omitempty"`
}

// Validate bounds the request.
func (r EnsureRequest) Validate() error {
	if len(r.Accounts) > MaxAccounts {
		return fmt.Errorf("%d accounts, more than %d", len(r.Accounts), MaxAccounts)
	}
	seen := map[string]bool{}
	for _, a := range r.Accounts {
		if !ValidSlot(a) || seen[a] {
			return fmt.Errorf("account %q: want twelve lowercase hex digits, each once", a)
		}
		seen[a] = true
	}
	return nil
}

// AccountStatus is what an account's own files say of its login, read in
// the box by a fixed script that selects these fields and nothing else, so
// no token ever leaves the box. Times are the CLI's, milliseconds since the
// epoch; the zero value is absent.
type AccountStatus struct {
	// LoggedIn is whether the account's credentials hold a refresh token.
	LoggedIn         bool   `json:"logged_in"`
	Email            string `json:"email,omitempty"`
	AccountUUID      string `json:"account_uuid,omitempty"`
	OrgID            string `json:"org_id,omitempty"`
	OrgName          string `json:"org_name,omitempty"`
	SubscriptionType string `json:"subscription_type,omitempty"`
	RateLimitTier    string `json:"rate_limit_tier,omitempty"`
	// AccessExpiresAt is the access token's expiry; the CLI refreshes it.
	AccessExpiresAt int64 `json:"access_expires_at,omitempty"`
	// RefreshExpiresAt is the refresh token's: past it, the account needs
	// a new login.
	RefreshExpiresAt int64 `json:"refresh_expires_at,omitempty"`
}

// Box states.
const (
	BoxNone    = "none"
	BoxRunning = "running"
	BoxStopped = "stopped"
)

// BoxStatus is what sandboxd says of a box.
type BoxStatus struct {
	Name  string `json:"name"`
	ID    string `json:"id,omitempty"`
	State string `json:"state"`
	// Image is the image the container runs (a digest reference).
	Image string `json:"image,omitempty"`
	// Host and Arch are the docker host's name and architecture.
	Host      string     `json:"host,omitempty"`
	Arch      string     `json:"arch,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	// Accounts are the account slots the container mounts, sorted.
	Accounts []string `json:"accounts,omitempty"`
}

// TTY presets: the only commands a TTY may run, each a tmux session so it
// outlives the connection (docs/workspace.md, Terminals).
const (
	// TTYShell is `tmux new -A -s <session>`: a login shell.
	TTYShell = "shell"
	// TTYClaude is `tmux new -A -s <session> claude`: Claude Code's own TUI.
	TTYClaude = "claude"
	// TTYLogin is `claude auth login` under an account's directory: the
	// full login (a URL, then a pasted code), which writes the account's
	// credentials into its own volume.
	TTYLogin = "login"
)

// TTYRequest attaches a TTY to a tmux session in a box.
type TTYRequest struct {
	Preset  string `json:"preset"`
	Session string `json:"session"`
	// Cwd is relative to /work ("" is /work itself).
	Cwd  string `json:"cwd,omitempty"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	// Env is the allowed environment (AllowedEnv) for the attaching client.
	Env map[string]string `json:"env,omitempty"`
	// Account is an account's slot: the session runs with that account's
	// directory as CLAUDE_CONFIG_DIR and without the subscription token
	// or any ANTHROPIC_ variable, so its own login is used.
	Account string `json:"account,omitempty"`
}

// Terminal size bounds: what xterm.js sends on a phone up to a wide
// monitor, with room; larger is a client bug.
const (
	MaxCols = 1000
	MaxRows = 500
)

var sessionRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidSession reports whether s can name a tmux session (no ':' or '.',
// which tmux reads as window and pane separators).
func ValidSession(s string) bool { return sessionRE.MatchString(s) }

// boxNameRE is an agent name (spec.AgentNamePattern's shape): the box is
// named for its agent.
var boxNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ValidBoxName reports whether s can name a box.
func ValidBoxName(s string) bool { return boxNameRE.MatchString(s) }

// ValidCwd reports whether c is a clean path relative to /work, inside it.
func ValidCwd(c string) bool {
	if c == "" {
		return true
	}
	if strings.HasPrefix(c, "/") || strings.ContainsAny(c, "\x00\\") || len(c) > 1024 {
		return false
	}
	return path.Clean(c) == c && c != "." && c != ".." && !strings.HasPrefix(c, "../")
}

// Validate is the check sandboxd applies; the client applies it too.
func (r TTYRequest) Validate() error {
	if r.Preset != TTYShell && r.Preset != TTYClaude && r.Preset != TTYLogin {
		return fmt.Errorf("preset %q: want %s, %s or %s", r.Preset, TTYShell, TTYClaude, TTYLogin)
	}
	if r.Account != "" && !ValidSlot(r.Account) {
		return fmt.Errorf("account %q: want twelve lowercase hex digits", r.Account)
	}
	if r.Preset == TTYLogin && r.Account == "" {
		return errors.New("the login preset needs an account")
	}
	if !ValidSession(r.Session) {
		return fmt.Errorf("session %q: want lowercase letters, digits and dashes", r.Session)
	}
	if !ValidCwd(r.Cwd) {
		return fmt.Errorf("cwd %q: want a clean path inside /work", r.Cwd)
	}
	if err := validSize(r.Cols, r.Rows); err != nil {
		return err
	}
	for k, v := range r.Env {
		if !AllowedEnv[k] {
			return fmt.Errorf("env %q is not allowed", k)
		}
		if r.Account != "" {
			// An account's session uses its own login, never the token.
			return fmt.Errorf("env %q with an account: an account's terminal carries no credential", k)
		}
		if strings.IndexByte(v, 0) >= 0 || len(v) > MaxEnvValue {
			return fmt.Errorf("env %q has an invalid value", k)
		}
	}
	return nil
}

func validSize(cols, rows int) error {
	if cols < 1 || cols > MaxCols || rows < 1 || rows > MaxRows {
		return fmt.Errorf("size %dx%d: want 1 to %d columns and 1 to %d rows", cols, rows, MaxCols, MaxRows)
	}
	return nil
}

// Resize is a FrameResize payload.
type Resize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Validate bounds a resize as a request's size is.
func (r Resize) Validate() error { return validSize(r.Cols, r.Rows) }

// Frames of the TTY stream. orbit to sandboxd: FrameStdin (bytes for the
// terminal) and FrameResize (a JSON Resize). sandboxd to orbit: FrameStdout
// (the terminal's output) and, last, FrameExit (a JSON Exit). Both use
// WriteFrame's framing.
const (
	FrameStdin  byte = 1
	FrameResize byte = 4
)

// ErrNoBox is sandboxd's answer for a box that does not exist.
var ErrNoBox = errors.New("no such box")
