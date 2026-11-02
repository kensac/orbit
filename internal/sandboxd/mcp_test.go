package sandboxd

import (
	"archive/tar"
	"bytes"
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

// fakeClaude puts a claude (and a timeout) on the scripts' PATH that logs
// each call with its CLAUDE_CONFIG_DIR and whether it saw the token, and
// answers mcp list with list.
func fakeClaude(t *testing.T, h *harness, list string) (logFile string) {
	bin := t.TempDir()
	h.fd.bin = bin
	h.fd.env = []string{"CLAUDE_CODE_OAUTH_TOKEN=sub", "ANTHROPIC_BASE_URL=u"}
	logFile = filepath.Join(bin, "log")
	_ = os.WriteFile(filepath.Join(bin, "list"), []byte(list), 0o644)
	_ = os.WriteFile(filepath.Join(bin, "timeout"), []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755)
	_ = os.WriteFile(filepath.Join(bin, "claude"), []byte(`#!/bin/sh
printf '%s|%s|%s|%s\n' "$CLAUDE_CONFIG_DIR" "${CLAUDE_CODE_OAUTH_TOKEN:-}${ANTHROPIC_BASE_URL:-}" "$(pwd)" "$*" >>`+logFile+`
[ "$1 $2" = "mcp list" ] && cat `+filepath.Join(bin, "list")+`
exit 0
`), 0o755)
	return logFile
}

func calls(t *testing.T, logFile string) []string {
	b, _ := os.ReadFile(logFile)
	_ = os.Remove(logFile)
	var out []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func mcpHarness(t *testing.T) (*harness, string) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	h := newHarness(t)
	h.srv.cfg = boxConfig()
	h.srv.cfg.Docker = h.srv.d.sock
	home := t.TempDir()
	h.fd.home = home
	if _, err := h.client.EnsureBox(context.Background(), "orbit", []string{slotA}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude-accounts", slotA)
	_ = os.MkdirAll(dir, 0o700)
	return h, dir
}

const mcpListOut = "Checking MCP server health…\n\n" +
	"claude.ai Gmail: https://gmailmcp.googleapis.com/mcp/v1 - ✔ Connected\n" +
	"claude.ai Google Sheets: https://gws-mcp.kanishksachdev.com/mcp - ! Needs authentication\n" +
	"plugin:github:github: https://api.githubcopilot.com/mcp/ (HTTP) - ✘ Failed to connect — HTTP 400: bad request\n" +
	"owl: uv run --project /x - y serve - ✔ Connected\n" +
	"proj: https://p (SSE) - ⏸ Pending approval\n" +
	"odd: https://o - ? Something new\n"

func TestAccountMCPList(t *testing.T) {
	h, dir := mcpHarness(t)
	logFile := fakeClaude(t, h, mcpListOut)
	got, err := h.client.AccountMCP(context.Background(), "orbit", slotA)
	if err != nil {
		t.Fatal(err)
	}
	want := []sandbox.MCPServer{
		{Name: "claude.ai Gmail", Target: "https://gmailmcp.googleapis.com/mcp/v1", Status: "connected", Connector: true},
		{Name: "claude.ai Google Sheets", Target: "https://gws-mcp.kanishksachdev.com/mcp", Status: "needs_auth", Connector: true},
		{Name: "plugin:github:github", Target: "https://api.githubcopilot.com/mcp/", Transport: "HTTP", Status: "failed", Detail: "HTTP 400: bad request"},
		{Name: "owl", Target: "uv run --project /x - y serve", Status: "connected"},
		{Name: "proj", Target: "https://p", Transport: "SSE", Status: "pending"},
		{Name: "odd", Target: "https://o", Status: "unknown", Detail: "? Something new"},
	}
	if !reflect.DeepEqual(got, want) {
		b, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("parsed:\n%s", b)
	}
	cs := calls(t, logFile)
	if len(cs) != 1 || cs[0] != dir+"||"+h.fd.home+"|mcp list" {
		t.Fatalf("claude was run as %q", cs)
	}
}

func TestAccountMCPSync(t *testing.T) {
	h, dir := mcpHarness(t)
	logFile := fakeClaude(t, h, "")
	ctx := context.Background()
	orbit := json.RawMessage(`{"type":"http","url":"https://orbit.kanishksachdev.com/mcp"}`)
	docs := json.RawMessage(`{"type":"stdio","command":"docs-mcp","args":["--ro"]}`)
	if err := h.client.SyncAccountMCP(ctx, "orbit", slotA, map[string]json.RawMessage{"orbit": orbit, "docs": docs}); err != nil {
		t.Fatal(err)
	}
	cs := calls(t, logFile)
	wantAdds := map[string]bool{
		dir + "||" + h.fd.home + "|mcp add-json -s user docs " + string(docs):   true,
		dir + "||" + h.fd.home + "|mcp add-json -s user orbit " + string(orbit): true,
	}
	adds := 0
	for _, c := range cs {
		if strings.Contains(c, "add-json") {
			if !wantAdds[c] {
				t.Errorf("unexpected %q", c)
			}
			adds++
		}
	}
	if adds != 2 {
		t.Fatalf("calls %q", cs)
	}
	// Again unchanged: nothing runs. Then one changed and one dropped.
	if err := h.client.SyncAccountMCP(ctx, "orbit", slotA, map[string]json.RawMessage{"orbit": orbit, "docs": docs}); err != nil {
		t.Fatal(err)
	}
	if cs := calls(t, logFile); len(cs) != 0 {
		t.Fatalf("an unchanged sync ran %q", cs)
	}
	orbit2 := json.RawMessage(`{"type":"http","url":"https://orbit.kanishksachdev.com/mcp2"}`)
	if err := h.client.SyncAccountMCP(ctx, "orbit", slotA, map[string]json.RawMessage{"orbit": orbit2}); err != nil {
		t.Fatal(err)
	}
	cs = calls(t, logFile)
	joined := strings.Join(cs, "\n")
	if !strings.Contains(joined, "|mcp remove -s user docs") || !strings.Contains(joined, "|mcp add-json -s user orbit "+string(orbit2)) || strings.Contains(joined, "add-json -s user docs") {
		t.Fatalf("calls %q", cs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".orbit", "mcp-managed.json")); !strings.Contains(string(b), "mcp2") || strings.Contains(string(b), "docs") {
		t.Fatalf("managed record %s", b)
	}
	for name, m := range map[string]map[string]json.RawMessage{
		"bad name":   {"a b": orbit},
		"no type":    {"x": json.RawMessage(`{"url":"https://x"}`)},
		"env":        {"x": json.RawMessage(`{"type":"http","url":"https://x","headers":{"Authorization":"Bearer ${CLAUDE_CODE_OAUTH_TOKEN}"}}`)},
		"not object": {"x": json.RawMessage(`[1]`)},
	} {
		if err := sandbox.ValidateMCPServers(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMCPLoginTTY(t *testing.T) {
	got := ttyExecSpec(sandbox.TTYRequest{Preset: "mcp-login", Session: "mcp-login-1", Cols: 80, Rows: 24, Account: slotA, MCPServer: "claude.ai Google Sheets"})
	if !reflect.DeepEqual(got.Cmd[5:12], []string{"-e", "CLAUDE_CONFIG_DIR=/home/agent/.claude-accounts/" + slotA, "sh", "-c", accountScript, "orbit-account", "claude"}) ||
		!reflect.DeepEqual(got.Cmd[12:17], []string{"mcp", "login", "--no-browser", "claude.ai Google Sheets", ";"}) {
		t.Fatalf("mcp login exec %q", got.Cmd)
	}
	for name, r := range map[string]sandbox.TTYRequest{
		"no server":          {Preset: "mcp-login", Session: "m-1", Cols: 80, Rows: 24, Account: slotA},
		"no account":         {Preset: "mcp-login", Session: "m-1", Cols: 80, Rows: 24, MCPServer: "x"},
		"an option":          {Preset: "mcp-login", Session: "m-1", Cols: 80, Rows: 24, Account: slotA, MCPServer: "--help"},
		"a newline":          {Preset: "mcp-login", Session: "m-1", Cols: 80, Rows: 24, Account: slotA, MCPServer: "a\nb"},
		"a server elsewhere": {Preset: "claude", Session: "c-1", Cols: 80, Rows: 24, Account: slotA, MCPServer: "x"},
	} {
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func skillsTar(t *testing.T, files map[string]string, extra ...*tar.Header) *bytes.Buffer {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for n, c := range files {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: n, Size: int64(len(c)), Mode: 0o644})
		_, _ = tw.Write([]byte(c))
	}
	for _, h := range extra {
		_ = tw.WriteHeader(h)
	}
	_ = tw.Close()
	return &b
}

func TestSkillsSync(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar is not installed")
	}
	h, _ := mcpHarness(t)
	ctx := context.Background()
	skills := filepath.Join(h.fd.home, ".claude", "skills")
	_ = os.MkdirAll(filepath.Join(skills, "mine"), 0o755)
	_ = os.WriteFile(filepath.Join(skills, "mine", "SKILL.md"), []byte("the person's"), 0o644)
	err := h.client.SyncSkills(ctx, "orbit", skillsTar(t, map[string]string{
		"recheck/SKILL.md": "---\nname: recheck\n---\n", "recheck/scripts/run.sh": "echo",
		"mine/SKILL.md": "orbit's", "old/SKILL.md": "x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(skills, p)); return string(b) }
	if read("recheck/scripts/run.sh") != "echo" || read("mine/SKILL.md") != "the person's" || read("old/SKILL.md") != "x" {
		t.Fatalf("after the first sync: %q %q %q", read("recheck/scripts/run.sh"), read("mine/SKILL.md"), read("old/SKILL.md"))
	}
	if m := read(".orbit-managed"); m != "old\nrecheck\n" && m != "recheck\nold\n" {
		t.Fatalf("managed %q", m)
	}
	// A retired skill goes; a changed one is replaced whole.
	if err := h.client.SyncSkills(ctx, "orbit", skillsTar(t, map[string]string{"recheck/SKILL.md": "v2"})); err != nil {
		t.Fatal(err)
	}
	if read("recheck/SKILL.md") != "v2" || read("recheck/scripts/run.sh") != "" || read("old/SKILL.md") != "" || read("mine/SKILL.md") != "the person's" {
		t.Fatal("the second sync did not replace orbit's skills alone")
	}
	var se *sandbox.Error
	for name, tb := range map[string]*bytes.Buffer{
		"a symlink":   skillsTar(t, nil, &tar.Header{Typeflag: tar.TypeSymlink, Name: "x/SKILL.md", Linkname: "/etc/passwd"}),
		"out":         skillsTar(t, map[string]string{"x/../../.bashrc": "evil"}),
		"absolute":    skillsTar(t, map[string]string{"/x/SKILL.md": "a"}),
		"top file":    skillsTar(t, map[string]string{"SKILL.md": "a"}),
		"a bad skill": skillsTar(t, map[string]string{"X/SKILL.md": "a"}),
	} {
		if err := h.client.SyncSkills(ctx, "orbit", tb); !errors.As(err, &se) || se.Status != http.StatusBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
}
