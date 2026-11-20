package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kensac/orbit/internal/sandbox"
)

func (v *boxEnv) workspaceID(t *testing.T) string {
	t.Helper()
	code, out := call(t, http.MethodGet, v.srv.URL+"/api/v1/agents/box/workspace", v.person("kanishk"), "")
	ws, _ := out["workspace"].(map[string]any)
	if code != 200 || ws == nil {
		t.Fatalf("workspace: %d %v", code, out)
	}
	if ws["capabilities"].(map[string]any)["accounts"] != true {
		t.Fatalf("a box workspace without accounts: %v", ws)
	}
	return ws["id"].(string)
}

func (v *boxEnv) lastTTY() sandbox.TTYRequest {
	v.fake.mu.Lock()
	defer v.fake.mu.Unlock()
	return v.fake.ttys[len(v.fake.ttys)-1]
}

// TestAccounts walks an account's life: created on a stopped box with its
// login terminal, a second one refused while terminals run and then made
// with a restart, its login read from the box, renamed, and terminals that
// run under it without the subscription token.
func TestAccounts(t *testing.T) {
	v := newBoxEnv(t)
	ws := v.workspaceID(t)
	base := v.srv.URL + "/api/v1/workspaces/" + ws + "/accounts"
	for body, want := range map[string]int{
		`{"label":"","cols":80,"rows":24}`:                                 400,
		`{"label":"work","cols":0,"rows":24}`:                              400,
		`{"label":"work","cols":80,"rows":24,"token":"x"}`:                 400,
		`{"label":"` + strings.Repeat("x", 101) + `","cols":80,"rows":24}`: 400,
	} {
		if code, out := call(t, http.MethodPost, base, v.person("kanishk"), body); code != want {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}

	code, out := call(t, http.MethodPost, base, v.person("kanishk"), `{"label":"personal","cols":100,"rows":30}`)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	acct := out["account"].(map[string]any)
	login := out["terminal"].(map[string]any)
	slot := acct["slot"].(string)
	if !sandbox.ValidSlot(slot) || acct["dir"] != "/home/agent/.claude-accounts/"+slot || acct["logged_in"] != false || acct["label"] != "personal" || acct["created_by"] != "kanishk" {
		t.Fatalf("account %v", acct)
	}
	if login["preset"] != "login" || login["account_id"] != acct["id"] || login["title"] != "Log in: personal" || login["state"] != "running" {
		t.Fatalf("login terminal %v", login)
	}
	v.fake.mu.Lock()
	mounts, prepared, recreates := v.fake.mounts["box"], v.fake.prepared, v.fake.recreates
	v.fake.mu.Unlock()
	if len(mounts) != 1 || mounts[0] != slot || len(prepared) != 1 || recreates != 0 {
		t.Fatalf("a stopped box: mounts %v prepared %v recreates %d", mounts, prepared, recreates)
	}
	if code, out := call(t, http.MethodPost, base, v.person("kanishk"), `{"label":"personal","cols":80,"rows":24,"restart":true}`); code != 409 || out["check"] != "exists" {
		t.Fatalf("a duplicate label: %d %v", code, out)
	}

	// The box runs now, with the login terminal: a second account needs a
	// restart, which ends that terminal.
	if code, out := call(t, http.MethodPost, base, v.person("kanishk"), `{"label":"work","cols":80,"rows":24}`); code != 409 || out["check"] != "busy" {
		t.Fatalf("an account while terminals run: %d %v", code, out)
	}
	code, out = call(t, http.MethodPost, base, v.person("kanishk"), `{"label":"work","cols":80,"rows":24,"restart":true}`)
	if code != 201 {
		t.Fatalf("create with restart: %d %v", code, out)
	}
	work := out["account"].(map[string]any)
	_, got := call(t, http.MethodGet, v.srv.URL+"/api/v1/terminals/"+login["id"].(string), v.person("kanishk"), "")
	if tm := got["terminal"].(map[string]any); tm["state"] != "exited" || tm["end_reason"] != "box_restarted" {
		t.Fatalf("a terminal across a box restart: %v", tm)
	}
	v.fake.mu.Lock()
	mounts, recreates = append([]string(nil), v.fake.mounts["box"]...), v.fake.recreates
	v.fake.mu.Unlock()
	if len(mounts) != 2 || recreates != 1 {
		t.Fatalf("restart: mounts %v recreates %d", mounts, recreates)
	}

	// Listing reads each login from the running box; nothing else is
	// shown, and the expiries are the CLI's milliseconds as times.
	v.fake.mu.Lock()
	v.fake.logins[slot] = sandbox.AccountStatus{LoggedIn: true, Email: "k@example.com", OrgID: "o-1", OrgName: "K's Organization",
		SubscriptionType: "max", RefreshExpiresAt: time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli()}
	seen, pct := time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC), 12.5
	v.fake.usage[slot] = sandbox.AccountUsage{Source: sandbox.UsageStatusline, ObservedAt: &seen,
		Windows: []sandbox.UsageWindow{{Window: "five_hour", UsedPercentage: &pct, ResetsAt: &seen}}}
	v.fake.mu.Unlock()
	code, out = call(t, http.MethodGet, base, v.person("kanishk"), "")
	as := out["accounts"].([]any)
	if code != 200 || len(as) != 2 {
		t.Fatalf("list: %d %v", code, out)
	}
	first := as[0].(map[string]any)
	exp, _ := time.Parse(time.RFC3339, first["refresh_expires_at"].(string))
	if first["logged_in"] != true || first["email"] != "k@example.com" || first["subscription_type"] != "max" ||
		!exp.Equal(time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)) || first["checked_at"] == nil || first["access_expires_at"] != nil {
		t.Fatalf("a logged-in account: %v", first)
	}
	usage, _ := first["usage"].(map[string]any)
	if usage == nil || usage["source"] != "statusline" || usage["observed_at"] != "2026-11-02T07:00:00Z" ||
		usage["windows"].([]any)[0].(map[string]any)["used_percentage"] != 12.5 || as[1].(map[string]any)["usage"] != nil {
		t.Fatalf("usage %v / %v", first["usage"], as[1].(map[string]any)["usage"])
	}
	// A later read that finds no login keeps who it was.
	v.fake.mu.Lock()
	v.fake.logins[slot] = sandbox.AccountStatus{}
	v.fake.mu.Unlock()
	_, out = call(t, http.MethodGet, base+"/"+first["id"].(string), v.person("kanishk"), "")
	if a := out["account"].(map[string]any); a["logged_in"] != false || a["email"] != "k@example.com" || a["refresh_expires_at"] != nil {
		t.Fatalf("a logged-out account: %v", a)
	}

	if code, out := call(t, http.MethodPatch, base+"/"+work["id"].(string), v.person("kanishk"), `{"label":"personal"}`); code != 409 || out["check"] != "exists" {
		t.Fatalf("rename onto another label: %d %v", code, out)
	}
	if code, out := call(t, http.MethodPatch, base+"/"+work["id"].(string), v.person("kanishk"), `{"label":"job"}`); code != 200 || out["account"].(map[string]any)["label"] != "job" {
		t.Fatalf("rename: %d %v", code, out)
	}

	// A terminal under an account attaches with its slot and no token; one
	// without carries the token as before.
	acctTerm := v.newTerminal(t, `{"preset":"claude","cols":80,"rows":24,"account":"`+work["id"].(string)+`"}`)
	if acctTerm["account_id"] != work["id"] {
		t.Fatalf("terminal %v", acctTerm)
	}
	ticket, u := v.ticket(t, acctTerm["id"].(string))
	c, _, err := dial(t, u, ticket, portfolio)
	if err != nil {
		t.Fatal(err)
	}
	if m := readText(t, c); m["type"] != "ready" {
		t.Fatalf("first frame %v", m)
	}
	if tr := v.lastTTY(); tr.Account != work["slot"] || len(tr.Env) != 0 || tr.Preset != "claude" {
		t.Fatalf("an account's tty request %+v", tr)
	}
	c.Close(websocket.StatusNormalClosure, "")
	plain := v.newTerminal(t, `{"preset":"claude","cols":80,"rows":24}`)
	if plain["account_id"] != nil {
		t.Fatalf("terminal %v", plain)
	}
	ticket, u = v.ticket(t, plain["id"].(string))
	c, _, err = dial(t, u, ticket, portfolio)
	if err != nil {
		t.Fatal(err)
	}
	readText(t, c)
	if tr := v.lastTTY(); tr.Account != "" || tr.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "sub-token" {
		t.Fatalf("the box's own claude tty %+v", tr)
	}
	c.Close(websocket.StatusNormalClosure, "")

	// A new login for an account, and the login preset only through it.
	code, out = call(t, http.MethodPost, base+"/"+first["id"].(string)+"/login", v.person("kanishk"), `{"cols":80,"rows":24}`)
	if code != 201 || out["terminal"].(map[string]any)["preset"] != "login" {
		t.Fatalf("login: %d %v", code, out)
	}
	relogin := out["terminal"].(map[string]any)
	if code, _ := call(t, http.MethodPost, v.srv.URL+"/api/v1/workspaces/"+ws+"/terminals", v.person("kanishk"), `{"preset":"login","cols":80,"rows":24}`); code != 400 {
		t.Fatalf("a login terminal through the terminals route: %d", code)
	}
	// The login ending reads the account again before the exit frame.
	v.fake.mu.Lock()
	v.fake.logins[slot] = sandbox.AccountStatus{LoggedIn: true, Email: "k@example.com", SubscriptionType: "pro"}
	v.fake.mu.Unlock()
	ticket, u = v.ticket(t, relogin["id"].(string))
	c, _, err = dial(t, u, ticket, portfolio)
	if err != nil {
		t.Fatal(err)
	}
	readText(t, c)
	if tr := v.lastTTY(); tr.Preset != "login" || tr.Account != slot || len(tr.Env) != 0 {
		t.Fatalf("login tty %+v", tr)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("exit")); err != nil {
		t.Fatal(err)
	}
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no exit frame: %v", err)
		}
		if typ == websocket.MessageText && strings.Contains(string(b), `"exit"`) {
			break
		}
	}
	a, err := v.s.GetAccount(ctx, first["id"].(string))
	if err != nil || !a.LoggedIn || a.SubscriptionType == nil || *a.SubscriptionType != "pro" || a.LastUsedAt == nil {
		t.Fatalf("after the login: %+v %v", a, err)
	}

	// Another workspace's account, an unknown one, and one the running box
	// does not mount yet.
	if code, out := call(t, http.MethodPost, v.srv.URL+"/api/v1/workspaces/"+ws+"/terminals", v.person("kanishk"),
		`{"preset":"shell","cols":80,"rows":24,"account":"00000000-0000-0000-0000-000000000000"}`); code != 404 || out["check"] != "not_found" {
		t.Fatalf("an unknown account: %d %v", code, out)
	}
	late, err := v.s.CreateAccount(ctx, ws, "late", "kanishk")
	if err != nil {
		t.Fatal(err)
	}
	if code, out := call(t, http.MethodPost, v.srv.URL+"/api/v1/workspaces/"+ws+"/terminals", v.person("kanishk"),
		`{"preset":"shell","cols":80,"rows":24,"account":"`+late.ID+`"}`); code != 409 || out["check"] != "restart" {
		t.Fatalf("an unmounted account: %d %v", code, out)
	}
}

// TestAccountMCP lists an account's servers, marking orbit's, and relays a
// connector's login through an mcp-login terminal.
func TestAccountMCP(t *testing.T) {
	v := newBoxEnv(t)
	ws := v.workspaceID(t)
	base := v.srv.URL + "/api/v1/workspaces/" + ws + "/accounts"
	_, out := call(t, http.MethodPost, base, v.person("kanishk"), `{"label":"personal","cols":80,"rows":24}`)
	acct := out["account"].(map[string]any)
	code, out := call(t, http.MethodGet, base+"/"+acct["id"].(string)+"/mcp", v.person("kanishk"), "")
	servers, _ := out["servers"].([]any)
	if code != 200 || len(servers) != 2 || out["checked_at"] == nil {
		t.Fatalf("mcp: %d %v", code, out)
	}
	gmail, orbit := servers[0].(map[string]any), servers[1].(map[string]any)
	if gmail["connector"] != true || gmail["status"] != "needs_auth" || gmail["managed"] != false || orbit["managed"] != true || orbit["transport"] != "HTTP" {
		t.Fatalf("servers %v", servers)
	}
	for body, want := range map[string]int{
		`{"server":"","cols":80,"rows":24}`:               400,
		`{"server":"--help","cols":80,"rows":24}`:         400,
		`{"server":"a\nb","cols":80,"rows":24}`:           400,
		`{"server":"claude.ai Gmail","cols":0,"rows":24}`: 400,
	} {
		if code, out := call(t, http.MethodPost, base+"/"+acct["id"].(string)+"/mcp/login", v.person("kanishk"), body); code != want {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	code, out = call(t, http.MethodPost, base+"/"+acct["id"].(string)+"/mcp/login", v.person("kanishk"), `{"server":"claude.ai Gmail","cols":80,"rows":24}`)
	term, _ := out["terminal"].(map[string]any)
	if code != 201 || term["preset"] != "mcp-login" || term["mcp_server"] != "claude.ai Gmail" || term["account_id"] != acct["id"] || term["title"] != "Connect: claude.ai Gmail" {
		t.Fatalf("mcp login: %d %v", code, out)
	}
	ticket, u := v.ticket(t, term["id"].(string))
	c, _, err := dial(t, u, ticket, portfolio)
	if err != nil {
		t.Fatal(err)
	}
	readText(t, c)
	if tr := v.lastTTY(); tr.Preset != "mcp-login" || tr.MCPServer != "claude.ai Gmail" || tr.Account != acct["slot"] || len(tr.Env) != 0 {
		t.Fatalf("tty %+v", tr)
	}
	c.Close(websocket.StatusNormalClosure, "")
}
