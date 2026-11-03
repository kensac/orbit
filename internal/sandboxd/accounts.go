package sandboxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/sandbox"
)

// Claude accounts in a box (docs/workspace.md, Accounts). Each account is a
// directory under sandbox.AccountsRoot on its own named volume, used as
// CLAUDE_CONFIG_DIR: its credentials, its .claude.json (user MCP servers
// among them) and its sessions are its own, and the shared profile in
// ~/.claude (settings, CLAUDE.md, skills, commands, agents) is linked in.
// Every command here is a fixed script run as the box's user, its only
// inputs positional arguments sandboxd validated.

// boxRunTimeout bounds one fixed command in a box.
const boxRunTimeout = 60 * time.Second

// boxRunLimit bounds what sandboxd reads of a fixed command's output.
const boxRunLimit = 1 << 20

// prepareScript makes an account's directory the box user's (a new volume
// mounted where the image has no directory is root's, hence sudo, which the
// box allows) and links the shared profile into it. An entry the CLI
// replaced with a file of its own is left alone; a missing or linked one is
// linked again, so the step is safe to repeat.
const prepareScript = `set -eu
d=$HOME/.claude-accounts/$1
p=$HOME/.claude
[ -d "$d" ] || { echo "no directory $d: the box does not mount this account" >&2; exit 3; }
[ -O "$d" ] || sudo -n chown "$(id -u):$(id -g)" "$d"
chmod 0700 "$d"
mkdir -p "$p/skills" "$p/commands" "$p/agents"
[ -e "$p/settings.json" ] || [ -L "$p/settings.json" ] || printf '{}\n' > "$p/settings.json"
for e in settings.json CLAUDE.md skills commands agents; do
	if [ -L "$d/$e" ] || [ ! -e "$d/$e" ]; then ln -sfn "../../.claude/$e" "$d/$e"; fi
done`

// statusScript prints an account's login as sandbox.AccountStatus: jq
// selects named fields of the CLI's credentials and .claude.json in the
// box, so a token is never part of the output.
const statusScript = `d=$HOME/.claude-accounts/$1
[ -d "$d" ] || { echo "no directory $d: the box does not mount this account" >&2; exit 3; }
defs='def n: if type == "number" then floor else null end; def s: if type == "string" then .[0:200] else null end;'
cred=null
acct=null
if [ -s "$d/.credentials.json" ]; then
	cred=$(jq -c "$defs"' .claudeAiOauth | if type == "object" then {logged_in: ((.refreshToken | type) == "string"), subscription_type: (.subscriptionType | s), rate_limit_tier: (.rateLimitTier | s), access_expires_at: (.expiresAt | n), refresh_expires_at: (.refreshTokenExpiresAt | n)} else null end' "$d/.credentials.json" 2>/dev/null) || cred=null
fi
if [ -s "$d/.claude.json" ]; then
	acct=$(jq -c "$defs"' .oauthAccount | if type == "object" then {email: (.emailAddress | s), account_uuid: (.accountUuid | s), org_id: (.organizationUuid | s), org_name: (.organizationName | s)} else null end' "$d/.claude.json" 2>/dev/null) || acct=null
fi
jq -cn --argjson c "${cred:-null}" --argjson a "${acct:-null}" '{logged_in: false} + ($c // {}) + ($a // {}) | with_entries(select(.value != null))'`

// exitNoAccountDir is the scripts' exit: the box does not mount it.
const exitNoAccountDir = 3

// boxRun runs one fixed command in a box as its user, with env added to the
// container's environment and stdin (nil for none), and returns its stdout,
// its stderr (each at most boxRunLimit) and its exit code.
func (s *Server) boxRun(ctx context.Context, id string, argv, env []string, stdin []byte) (stdout, stderr []byte, code int, err error) {
	ctx, cancel := context.WithTimeout(ctx, boxRunTimeout)
	defer cancel()
	e := execSpec(sandbox.AttachRequest{Argv: argv})
	e.Env = append([]string(nil), env...)
	e.AttachStdin = stdin != nil
	execID, err := s.d.createExec(ctx, id, e)
	if err != nil {
		return nil, nil, -1, err
	}
	conn, br, err := s.d.startExec(ctx, execID)
	if err != nil {
		return nil, nil, -1, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if stdin != nil {
		go func() {
			_, _ = conn.Write(stdin)
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}()
	}
	var out, errb bytes.Buffer
	err = demux(br, func(typ byte, p []byte) error {
		b := &out
		if typ == sandbox.FrameStderr {
			b = &errb
		}
		if b.Len()+len(p) > boxRunLimit {
			return errors.New("output over the limit")
		}
		b.Write(p)
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("timed out after %s", boxRunTimeout)
		}
		return out.Bytes(), errb.Bytes(), -1, err
	}
	code, err = s.exitCode(execID)
	return out.Bytes(), errb.Bytes(), code, err
}

// accountBox is the running box and the account slot a request names, the
// box mounting it, or an error written.
func (s *Server) accountBox(w http.ResponseWriter, r *http.Request) (containerState, string, string, bool) {
	c, name, ok := s.runningBox(w, r)
	if !ok {
		return c, name, "", false
	}
	slot := r.PathValue("slot")
	if !sandbox.ValidSlot(slot) {
		writeError(w, http.StatusBadRequest, "account: twelve lowercase hex digits")
		return c, name, "", false
	}
	if !slices.Contains(mountedAccounts(c), slot) {
		writeError(w, http.StatusConflict, "the box does not mount account "+slot+": recreate it")
		return c, name, "", false
	}
	return c, name, slot, true
}

// scriptError is a fixed script's failure, its stderr's last line as the
// reason.
func scriptError(what string, code int, stderr []byte) string {
	msg := strings.TrimSpace(string(stderr))
	if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
		msg = msg[i+1:]
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return fmt.Sprintf("%s exited %d: %s", what, code, msg)
}

func (s *Server) accountPrepare(w http.ResponseWriter, r *http.Request) {
	c, name, slot, ok := s.accountBox(w, r)
	if !ok {
		return
	}
	_, stderr, code, err := s.boxRun(r.Context(), c.ID, []string{"sh", "-c", prepareScript, "orbit-account", slot}, nil, nil)
	switch {
	case err != nil:
		writeError(w, http.StatusBadGateway, "prepare: "+err.Error())
		return
	case code == exitNoAccountDir:
		writeError(w, http.StatusConflict, scriptError("prepare", code, stderr))
		return
	case code != 0:
		writeError(w, http.StatusBadGateway, scriptError("prepare", code, stderr))
		return
	}
	s.log.Info("sandboxd: account prepared", "box", name, "account", slot)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) accountStatus(w http.ResponseWriter, r *http.Request) {
	c, _, slot, ok := s.accountBox(w, r)
	if !ok {
		return
	}
	stdout, stderr, code, err := s.boxRun(r.Context(), c.ID, []string{"sh", "-c", statusScript, "orbit-account", slot}, nil, nil)
	switch {
	case err != nil:
		writeError(w, http.StatusBadGateway, "status: "+err.Error())
		return
	case code == exitNoAccountDir:
		writeError(w, http.StatusConflict, scriptError("status", code, stderr))
		return
	case code != 0:
		writeError(w, http.StatusBadGateway, scriptError("status", code, stderr))
		return
	}
	st, err := parseAccountStatus(stdout)
	if err != nil {
		writeError(w, http.StatusBadGateway, "status: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// parseAccountStatus reads statusScript's output strictly: a field it does
// not name means the script and this code disagree, and nothing unknown is
// passed on.
func parseAccountStatus(b []byte) (sandbox.AccountStatus, error) {
	var st sandbox.AccountStatus
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return st, fmt.Errorf("the status script printed %q: %w", truncate(b, 200), err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return st, errors.New("the status script printed more than one value")
	}
	return st, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

// usageScript prints the status line's record for an account (the box
// image's orbit-statusline writes it), nothing when there is none yet.
const usageScript = `d=$HOME/.claude-accounts/$1
[ -d "$d" ] || { echo "no directory $d: the box does not mount this account" >&2; exit 3; }
f=$d/.orbit/usage.json
[ -s "$f" ] || exit 0
head -c 65536 "$f"`

// pollScript asks /api/oauth/usage with the account's access token, which
// it reads and sends from inside the box (curl reads the header from its
// stdin, so it is not in an argv): the token never reaches sandboxd. It
// never refreshes a token, since a refresh rotates the refresh token under
// the CLI that owns it; an expired one answers 401 until the CLI next runs.
const pollScript = `d=$HOME/.claude-accounts/$1
[ -d "$d" ] || { echo "no directory $d: the box does not mount this account" >&2; exit 3; }
tok=$(jq -r '.claudeAiOauth.accessToken // empty' "$d/.credentials.json" 2>/dev/null)
[ -n "$tok" ] || { echo "the account is not logged in" >&2; exit 4; }
case $tok in *[!A-Za-z0-9._~+/=-]*) echo "the account's access token has an unexpected shape" >&2; exit 4 ;; esac
out=$(printf 'header = "Authorization: Bearer %s"\n' "$tok" |
	curl -sS --fail --max-time 20 --max-filesize 65536 -K - -H 'anthropic-beta: oauth-2025-04-20' -H 'Accept: application/json' \
		https://api.anthropic.com/api/oauth/usage) || exit 5
printf '%s' "$out"`

// exitNotLoggedIn is pollScript's exit for an account without a login.
const exitNotLoggedIn = 4

func (s *Server) accountUsage(w http.ResponseWriter, r *http.Request) {
	c, _, slot, ok := s.accountBox(w, r)
	if !ok {
		return
	}
	stdout, stderr, code, err := s.boxRun(r.Context(), c.ID, []string{"sh", "-c", usageScript, "orbit-account", slot}, nil, nil)
	switch {
	case err != nil:
		writeError(w, http.StatusBadGateway, "usage: "+err.Error())
		return
	case code == exitNoAccountDir:
		writeError(w, http.StatusConflict, scriptError("usage", code, stderr))
		return
	case code != 0:
		writeError(w, http.StatusBadGateway, scriptError("usage", code, stderr))
		return
	}
	u, err := sandbox.ParseStatuslineUsage(stdout)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) accountPoll(w http.ResponseWriter, r *http.Request) {
	c, name, slot, ok := s.accountBox(w, r)
	if !ok {
		return
	}
	stdout, stderr, code, err := s.boxRun(r.Context(), c.ID, []string{"sh", "-c", pollScript, "orbit-account", slot}, nil, nil)
	switch {
	case err != nil:
		writeError(w, http.StatusBadGateway, "poll: "+err.Error())
		return
	case code == exitNoAccountDir, code == exitNotLoggedIn:
		writeError(w, http.StatusConflict, scriptError("poll", code, stderr))
		return
	case code != 0:
		writeError(w, http.StatusBadGateway, scriptError("poll", code, stderr))
		return
	}
	u, err := sandbox.ParseOAuthUsage(stdout, s.now().UTC().Truncate(time.Second))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.log.Info("sandboxd: account usage polled", "box", name, "account", slot, "windows", len(u.Windows))
	writeJSON(w, http.StatusOK, u)
}
