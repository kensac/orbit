package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/sandbox"
)

const (
	slotA = "0123456789ab"
	slotB = "ba9876543210"
)

// TestAccountTTYSpec pins an account's terminals: the shared tty script,
// CLAUDE_CONFIG_DIR on the session, the command through accountScript, the
// token marked removed in the session, and no credential in the exec.
func TestAccountTTYSpec(t *testing.T) {
	if ttyScript != `echo "$$"; s=$1; shift; exec tmux new-session -A -s "$s" "$@"` {
		t.Fatalf("tty script changed: %s", ttyScript)
	}
	tail := []string{";", "set-environment", "-t", "=login-1", "-r", "CLAUDE_CODE_OAUTH_TOKEN"}
	got := ttyExecSpec(sandbox.TTYRequest{Preset: "login", Session: "login-1", Cols: 80, Rows: 24, Account: slotA})
	want := append([]string{"sh", "-c", ttyScript, "orbit-tty", "login-1",
		"-e", "CLAUDE_CONFIG_DIR=/home/agent/.claude-accounts/" + slotA, "sh", "-c", accountScript, "orbit-account",
		"claude", "auth", "login"}, tail...)
	if !reflect.DeepEqual(got.Cmd, want) || !reflect.DeepEqual(got.Env, []string{"TERM=xterm-256color"}) {
		t.Fatalf("login exec %q env %q", got.Cmd, got.Env)
	}
	tail[3] = "=claude-1"
	got = ttyExecSpec(sandbox.TTYRequest{Preset: "claude", Session: "claude-1", Cols: 80, Rows: 24, Account: slotA})
	if !reflect.DeepEqual(got.Cmd[5:], append([]string{"-e", "CLAUDE_CONFIG_DIR=/home/agent/.claude-accounts/" + slotA,
		"sh", "-c", accountScript, "orbit-account", "claude"}, tail...)) {
		t.Fatalf("claude exec %q", got.Cmd)
	}
	tail[3] = "=shell-1"
	got = ttyExecSpec(sandbox.TTYRequest{Preset: "shell", Session: "shell-1", Cols: 80, Rows: 24, Account: slotA})
	if !reflect.DeepEqual(got.Cmd[5:], append([]string{"-e", "CLAUDE_CONFIG_DIR=/home/agent/.claude-accounts/" + slotA,
		"sh", "-c", accountScript, "orbit-account", "bash", "-l"}, tail...)) {
		t.Fatalf("shell exec %q", got.Cmd)
	}

	for name, r := range map[string]sandbox.TTYRequest{
		"login without an account": {Preset: "login", Session: "l-1", Cols: 80, Rows: 24},
		"a bad slot":               {Preset: "claude", Session: "c-1", Cols: 80, Rows: 24, Account: "../x"},
		"a token with an account": {Preset: "claude", Session: "c-1", Cols: 80, Rows: 24, Account: slotA,
			Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}},
	} {
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestAccountScriptStripsCredentials runs accountScript: the command it
// execs sees neither the subscription token nor any ANTHROPIC_ variable.
func TestAccountScriptStripsCredentials(t *testing.T) {
	cmd := exec.Command("sh", "-c", accountScript, "orbit-account", "env")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "CLAUDE_CODE_OAUTH_TOKEN=tok", "ANTHROPIC_API_KEY=k", "ANTHROPIC_BASE_URL=u", "KEEP=1"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Contains(s, "tok") || strings.Contains(s, "ANTHROPIC_") || !strings.Contains(s, "KEEP=1") {
		t.Fatalf("environment after the script:\n%s", s)
	}
}

func TestVerifyBoxAccounts(t *testing.T) {
	cfg := boxConfig()
	c := containerState{}
	c.Config.User = "10001:10001"
	c.Config.Labels = map[string]string{LabelBox: "orbit"}
	c.HostConfig.CapDrop = []string{"MKNOD", "NET_RAW"}
	c.HostConfig.NetworkMode = "orbit-sandbox"
	c.HostConfig.IpcMode = "private"
	c.HostConfig.Memory = BoxMemoryBytes
	p := int64(BoxPids)
	c.HostConfig.PidsLimit = &p
	c.Mounts = []mountState{{"volume", "orbit-box-orbit-home", "/home/agent"}, {"volume", "orbit-box-orbit-work", "/work"},
		{"volume", "orbit-box-orbit-acct-" + slotB, "/home/agent/.claude-accounts/" + slotB},
		{"volume", "orbit-box-orbit-acct-" + slotA, "/home/agent/.claude-accounts/" + slotA}}
	if err := verifyBox(cfg, "orbit", c); err != nil {
		t.Fatal(err)
	}
	if got := mountedAccounts(c); !reflect.DeepEqual(got, []string{slotA, slotB}) {
		t.Fatalf("mounted %v", got)
	}
	for name, m := range map[string]mountState{
		"another box's account": {"volume", "orbit-box-finance-acct-" + slotA, "/home/agent/.claude-accounts/" + slotA},
		"another slot's volume": {"volume", "orbit-box-orbit-acct-" + slotB, "/home/agent/.claude-accounts/" + slotA},
		"a bad slot":            {"volume", "orbit-box-orbit-acct-x", "/home/agent/.claude-accounts/x"},
		"a bind":                {"bind", "orbit-box-orbit-acct-" + slotA, "/home/agent/.claude-accounts/" + slotA},
	} {
		bad := c
		bad.Mounts = append(append([]mountState(nil), c.Mounts[:2]...), m)
		if verifyBox(cfg, "orbit", bad) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBoxAccountMounts(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg = boxConfig()
	h.srv.cfg.Docker = h.srv.d.sock
	ctx := context.Background()

	st, err := h.client.EnsureBox(ctx, "orbit", []string{slotB, slotA})
	if err != nil || !reflect.DeepEqual(st.Accounts, []string{slotA, slotB}) {
		t.Fatalf("ensure: %+v %v", st, err)
	}
	h.fd.mu.Lock()
	spec := h.fd.created[0]
	h.fd.mu.Unlock()
	if mustJSON(t, spec) != mustJSON(t, boxSpec(h.srv.cfg, "orbit", []string{slotA, slotB})) {
		t.Fatalf("created %s", mustJSON(t, spec))
	}
	m := spec.HostConfig.Mounts[2]
	if m.Source != "orbit-box-orbit-acct-"+slotA || m.Target != "/home/agent/.claude-accounts/"+slotA || m.VolumeOptions.Labels[LabelBox] != "orbit" {
		t.Fatalf("account mount %+v", m)
	}

	// A running box is not recreated for a new account; it says what it
	// mounts, and a recreate adds it, keeping the others.
	const slotC = "cccccccccccc"
	st2, err := h.client.EnsureBox(ctx, "orbit", []string{slotA, slotB, slotC})
	if err != nil || st2.ID != st.ID || !reflect.DeepEqual(st2.Accounts, []string{slotA, slotB}) {
		t.Fatalf("running ensure: %+v %v", st2, err)
	}
	st3, err := h.client.RecreateBox(ctx, "orbit", []string{slotC})
	if err != nil || st3.ID == st.ID || st3.State != sandbox.BoxRunning || !reflect.DeepEqual(st3.Accounts, []string{slotA, slotB, slotC}) {
		t.Fatalf("recreate: %+v %v", st3, err)
	}
	// A stopped box lacking an account is recreated with it on ensure.
	if _, err := h.client.StopBox(ctx, "orbit"); err != nil {
		t.Fatal(err)
	}
	const slotD = "dddddddddddd"
	st4, err := h.client.EnsureBox(ctx, "orbit", []string{slotD})
	if err != nil || st4.ID == st3.ID || len(st4.Accounts) != 4 {
		t.Fatalf("stopped ensure: %+v %v", st4, err)
	}
	// A stopped box that has every account asked for is only started.
	if _, err := h.client.StopBox(ctx, "orbit"); err != nil {
		t.Fatal(err)
	}
	if st5, err := h.client.EnsureBox(ctx, "orbit", nil); err != nil || st5.ID != st4.ID {
		t.Fatalf("restart: %+v %v", st5, err)
	}

	var se *sandbox.Error
	if _, err := h.client.AttachTTY(ctx, "orbit", sandbox.TTYRequest{Preset: "claude", Session: "c-1", Cols: 80, Rows: 24, Account: "eeeeeeeeeeee"}); !errors.As(err, &se) || se.Status != http.StatusConflict {
		t.Fatalf("a TTY for an unmounted account: %v", err)
	}
	if err := h.client.PrepareAccount(ctx, "orbit", "eeeeeeeeeeee"); !errors.As(err, &se) || se.Status != http.StatusConflict {
		t.Fatalf("prepare of an unmounted account: %v", err)
	}
	tooMany := make([]string, sandbox.MaxAccounts+1)
	for i := range tooMany {
		tooMany[i] = strings.Repeat(string("0123456789abcdef"[i%16]), 11) + string("0123456789abcdef"[i/16])
	}
	if _, err := h.client.EnsureBox(ctx, "orbit", tooMany); err == nil {
		t.Fatal("more accounts than a box mounts were accepted")
	}
}

// TestAccountScripts runs the prepare and status scripts for real against a
// home on this machine, through sandboxd and the fake docker.
func TestAccountScripts(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	h := newHarness(t)
	h.srv.cfg = boxConfig()
	h.srv.cfg.Docker = h.srv.d.sock
	home := t.TempDir()
	h.fd.home = home
	ctx := context.Background()
	if _, err := h.client.EnsureBox(ctx, "orbit", []string{slotA}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude-accounts", slotA)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := h.client.AccountStatus(ctx, "orbit", slotA)
	if err != nil || st != (sandbox.AccountStatus{}) {
		t.Fatalf("status before a login: %+v %v", st, err)
	}

	if err := h.client.PrepareAccount(ctx, "orbit", slotA); err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"settings.json", "CLAUDE.md", "skills", "commands", "agents"} {
		target, err := os.Readlink(filepath.Join(dir, e))
		if err != nil || target != "../../.claude/"+e {
			t.Errorf("%s: link %q %v", e, target, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "settings.json")); err != nil || strings.TrimSpace(string(b)) != "{}" {
		t.Fatalf("shared settings through the link: %q %v", b, err)
	}
	// A file the CLI wrote in place of a link stays; preparing again is
	// harmless.
	_ = os.Remove(filepath.Join(dir, "CLAUDE.md"))
	_ = os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("mine"), 0o644)
	if err := h.client.PrepareAccount(ctx, "orbit", slotA); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(b) != "mine" {
		t.Fatal("prepare replaced a file of the account's own")
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("account directory mode %v", fi.Mode().Perm())
	}

	creds := `{"claudeAiOauth": {"accessToken": "sk-ant-oat01-SECRET", "refreshToken": "sk-ant-ort01-SECRET", "expiresAt": 1793000000000,
		"refreshTokenExpiresAt": 1800000000000, "scopes": ["user:inference", "user:profile"], "subscriptionType": "max", "rateLimitTier": "default_claude_max_20x"}}`
	cfg := `{"oauthAccount": {"emailAddress": "k@example.com", "accountUuid": "u-1", "organizationUuid": "o-1", "organizationName": "K's Organization",
		"billingType": "stripe_subscription"}, "mcpServers": {"x": {"type": "http", "url": "https://x", "headers": {"Authorization": "Bearer SECRET"}}}}`
	_ = os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(creds), 0o600)
	_ = os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(cfg), 0o600)
	st, err = h.client.AccountStatus(ctx, "orbit", slotA)
	want := sandbox.AccountStatus{LoggedIn: true, Email: "k@example.com", AccountUUID: "u-1", OrgID: "o-1", OrgName: "K's Organization",
		SubscriptionType: "max", RateLimitTier: "default_claude_max_20x", AccessExpiresAt: 1793000000000, RefreshExpiresAt: 1800000000000}
	if err != nil || st != want {
		t.Fatalf("status %+v %v", st, err)
	}
	// Nothing of a token reaches sandboxd's answer, whatever the files hold.
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "SECRET") {
		t.Fatalf("a token left the box: %s", b)
	}
	// A CLI that changed a field's type reads as absent, not as an error.
	_ = os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth": {"refreshToken": "x", "expiresAt": "soon", "subscriptionType": 3}}`), 0o600)
	if st, err := h.client.AccountStatus(ctx, "orbit", slotA); err != nil || st != (sandbox.AccountStatus{LoggedIn: true, Email: "k@example.com",
		AccountUUID: "u-1", OrgID: "o-1", OrgName: "K's Organization"}) {
		t.Fatalf("drifted status %+v %v", st, err)
	}
	if _, err := parseAccountStatus([]byte(`{"logged_in": true, "access_token": "x"}`)); err == nil {
		t.Fatal("an unknown field in the status was passed on")
	}
}

// TestAccountUsage runs the image's status line script as the CLI would,
// under an account, then reads its record through sandboxd; and polls with
// a fake curl that records how it was called.
func TestAccountUsage(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	h := newHarness(t)
	h.srv.cfg = boxConfig()
	h.srv.cfg.Docker = h.srv.d.sock
	home := t.TempDir()
	h.fd.home = home
	ctx := context.Background()
	if _, err := h.client.EnsureBox(ctx, "orbit", []string{slotA}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude-accounts", slotA)
	_ = os.MkdirAll(dir, 0o700)
	if u, err := h.client.AccountUsage(ctx, "orbit", slotA); err != nil || u.ObservedAt != nil || u.Source != sandbox.UsageStatusline {
		t.Fatalf("before the status line ran: %+v %v", u, err)
	}

	script, _ := filepath.Abs("../../deploy/workstation/orbit-statusline")
	line := exec.Command("sh", script)
	line.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CLAUDE_CONFIG_DIR=" + dir}
	line.Stdin = strings.NewReader(`{"model": {"display_name": "Opus"}, "rate_limits": {"five_hour": {"used_percentage": 12.5, "resets_at": 1793000000}, "seven_day": {"used_percentage": 40, "resets_at": 1793500000}}}`)
	out, err := line.Output()
	if err != nil || strings.TrimSpace(string(out)) != "Opus  5h 12.5%  7d 40%" {
		t.Fatalf("status line %q %v", out, err)
	}
	u, err := h.client.AccountUsage(ctx, "orbit", slotA)
	if err != nil || u.ObservedAt == nil || len(u.Windows) != 2 || u.Windows[0].Window != "five_hour" || *u.Windows[0].UsedPercentage != 12.5 {
		t.Fatalf("usage %+v %v", u, err)
	}
	// The person's own line, given the same input, replaces the default.
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".claude", "statusline-command"), []byte("#!/bin/sh\njq -r .model.display_name\n"), 0o755)
	line = exec.Command("sh", script)
	line.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CLAUDE_CONFIG_DIR=" + dir}
	line.Stdin = strings.NewReader(`{"model": {"display_name": "Mine"}}`)
	if out, err := line.Output(); err != nil || strings.TrimSpace(string(out)) != "Mine" {
		t.Fatalf("own status line %q %v", out, err)
	}

	// Not logged in: the poll refuses before calling anything.
	bin := t.TempDir()
	h.fd.bin = bin
	argsFile := filepath.Join(bin, "args")
	_ = os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\ncat >> "+argsFile+"\n"+
		"printf '%s' '{\"five_hour\": {\"utilization\": 7, \"resets_at\": \"2026-11-02T09:00:00Z\"}}'\n"), 0o755)
	var se *sandbox.Error
	if _, err := h.client.PollAccountUsage(ctx, "orbit", slotA); !errors.As(err, &se) || se.Status != http.StatusConflict {
		t.Fatalf("a poll without a login: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth": {"accessToken": "sk-ant-oat01-TOKEN_x", "refreshToken": "r"}}`), 0o600)
	u, err = h.client.PollAccountUsage(ctx, "orbit", slotA)
	if err != nil || u.Source != sandbox.UsagePoll || len(u.Windows) != 1 || *u.Windows[0].UsedPercentage != 7 {
		t.Fatalf("poll %+v %v", u, err)
	}
	b, _ := os.ReadFile(argsFile)
	args, cfg, _ := strings.Cut(string(b), "https://api.anthropic.com/api/oauth/usage\n")
	if strings.Contains(args, "TOKEN") || !strings.Contains(args, "anthropic-beta: oauth-2025-04-20") || strings.TrimSpace(cfg) != `header = "Authorization: Bearer sk-ant-oat01-TOKEN_x"` {
		t.Fatalf("curl was called with\n%s", b)
	}
	// curl failing (a 401 for an expired access token) is sandboxd's 502.
	_ = os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\necho 'curl: (22) The requested URL returned error: 401' >&2\nexit 22\n"), 0o755)
	if _, err := h.client.PollAccountUsage(ctx, "orbit", slotA); !errors.As(err, &se) || se.Status != http.StatusBadGateway || !strings.Contains(se.Message, "401") {
		t.Fatalf("a failed poll: %v", err)
	}
	// A token of an unexpected shape never reaches a curl config line.
	_ = os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth": {"accessToken": "a\"\nurl = \"http://evil"}}`), 0o600)
	if _, err := h.client.PollAccountUsage(ctx, "orbit", slotA); !errors.As(err, &se) || se.Status != http.StatusConflict {
		t.Fatalf("an odd token: %v", err)
	}
}

// TestGHToken writes and clears the box's GitHub token through the real
// scripts, and refuses anything that is not a token.
func TestGHToken(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg = boxConfig()
	h.srv.cfg.Docker = h.srv.d.sock
	h.fd.home = t.TempDir()
	run := filepath.Join(t.TempDir(), "orbit")
	h.fd.run = run
	ctx := context.Background()
	var se *sandbox.Error
	if err := h.client.WriteGHToken(ctx, "orbit", "ghs_"+strings.Repeat("a", 36)); !errors.Is(err, sandbox.ErrNoBox) {
		t.Fatalf("no box: %v", err)
	}
	if _, err := h.client.EnsureBox(ctx, "orbit", nil); err != nil {
		t.Fatal(err)
	}
	if err := h.client.ClearGHToken(ctx, "orbit"); err != nil {
		t.Fatalf("clearing what is not there: %v", err)
	}
	// The script makes its directory (the box's sudo is not needed here,
	// where it is the test's own).
	_ = os.MkdirAll(run, 0o700)
	tok := "ghs_" + strings.Repeat("A1b", 12)
	if err := h.client.WriteGHToken(ctx, "orbit", tok); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(run, "gh-token"))
	fi, _ := os.Stat(filepath.Join(run, "gh-token"))
	if err != nil || string(b) != tok || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file %q %v %v", b, err, fi.Mode())
	}
	for _, bad := range []string{"", "not a token", "ghs_short", tok + "\nrm -rf /", strings.Repeat("x", 2000)} {
		if err := h.client.WriteGHToken(ctx, "orbit", bad); !errors.As(err, &se) || se.Status != http.StatusBadRequest {
			t.Errorf("%.20q: %v", bad, err)
		}
	}
	if err := h.client.ClearGHToken(ctx, "orbit"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(run, "gh-token")); !os.IsNotExist(err) {
		t.Fatalf("the token is still there: %v", err)
	}
	h.fd.mu.Lock()
	defer h.fd.mu.Unlock()
	for _, e := range h.fd.execs {
		if strings.Contains(strings.Join(append(e.Cmd, e.Env...), " "), tok) {
			t.Fatalf("the token was in an exec's argv or env: %v", e.Cmd)
		}
	}
}
