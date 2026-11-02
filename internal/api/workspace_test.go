package api_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/workspace"
)

// withChatAgent adds the chat agent "chat" to v's configuration.
func withChatAgent(t *testing.T, v *env) {
	t.Helper()
	dir := v.e.ConfigDir
	if err := os.WriteFile(filepath.Join(dir, "agents", "chat.yaml"), []byte("name: chat\ncharter: x\nchat: true\nwake_on_message: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	specs := map[string]store.AgentSpec{}
	for _, name := range []string{"a", "chat"} {
		b, _ := json.Marshal(cfg.Agents[name])
		specs[name] = store.AgentSpec{SHA: cfg.Agents[name].SHA, JSON: b, Enabled: true}
	}
	if err := v.s.SyncAgents(ctx, specs); err != nil {
		t.Fatal(err)
	}
}

// putSnapshot stores files as the conversation's newest snapshot, written
// by run.
func putSnapshot(t *testing.T, v *env, conv, run string, files map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	for rel, b := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tree, n, err := workspace.Pack(workspace.Tree{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.s.PutWorkspace(ctx, conv, run, 1, n, tree); err != nil {
		t.Fatal(err)
	}
}

func raw(t *testing.T, method, u, tok string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	return do(t, req)
}

func TestWorkspaceIsFoundFromItsConversationAndAgent(t *testing.T) {
	v := newEnv(t, nil)
	withChatAgent(t, v)
	k := v.person("kanishk")
	base := v.srv.URL + "/api/v1"

	_, r := call(t, "POST", base+"/agents/a/conversations", k, `{"title":"plain"}`)
	plain := r["conversation_id"].(string)
	if code, r := call(t, "GET", base+"/conversations/"+plain+"/workspace", k, ""); code != 200 || r["workspace"] != nil {
		t.Fatalf("a non-chat agent's conversation = %d %v", code, r)
	}
	if _, r := call(t, "GET", base+"/conversations/"+plain, k, ""); r["conversation"].(map[string]any)["workspace_id"] != nil {
		t.Fatalf("workspace_id = %v", r)
	}
	if code, r := call(t, "GET", base+"/agents/a/workspace", k, ""); code != 200 || r["workspace"] != nil {
		t.Fatalf("agent a = %d %v", code, r)
	}
	if code, r := call(t, "GET", base+"/agents/nope/workspace", k, ""); code != 404 || r["check"] != "not_found" {
		t.Fatalf("no such agent = %d %v", code, r)
	}

	_, r = call(t, "POST", base+"/agents/chat/conversations", k, `{"title":"chat","wake":false}`)
	chat := r["conversation_id"].(string)
	_, r = call(t, "GET", base+"/conversations/"+chat, k, "")
	wid, _ := r["conversation"].(map[string]any)["workspace_id"].(string)
	if wid == "" {
		t.Fatalf("a chat conversation has a workspace_id: %v", r)
	}
	_, r = call(t, "GET", base+"/agents/chat/conversations", k, "")
	for _, c := range r["conversations"].([]any) {
		if c := c.(map[string]any); c["id"] == chat && c["workspace_id"] != wid {
			t.Fatalf("listed workspace_id = %v, want %s", c["workspace_id"], wid)
		}
	}
	code, r := call(t, "GET", base+"/conversations/"+chat+"/workspace", k, "")
	ws, _ := r["workspace"].(map[string]any)
	if code != 200 || ws["id"] != wid || ws["scope"] != "conversation" || ws["conversation_id"] != chat || ws["agent"] != "chat" ||
		ws["backend"] != "snapshot" || ws["cwd"] != "" || ws["as_of"] != nil || ws["box"] != nil {
		t.Fatalf("workspace = %d %v", code, r)
	}
	caps := ws["capabilities"].(map[string]any)
	if caps["read"] != true || caps["write"] != true || caps["terminals"] != false || caps["git"] != false || caps["watch"] != false ||
		len(caps["presets"].([]any)) != 0 {
		t.Fatalf("capabilities = %v", caps)
	}
	if code, r := call(t, "GET", base+"/workspaces/"+wid+"/tree", k, ""); code != 200 || len(r["entries"].([]any)) != 0 || r["path"] != "" {
		t.Fatalf("an empty workspace's tree = %d %v", code, r)
	}
	_, r = call(t, "GET", base+"/agents/chat/workspace", k, "")
	if dw, _ := r["workspace"].(map[string]any); dw == nil || dw["id"] == wid {
		t.Fatalf("the agent's workspace is its default conversation's, not this one: %v", r)
	}
	if code, r := call(t, "GET", base+"/workspaces/"+strings.Repeat("0", 8)+"-0000-0000-0000-000000000000/tree", k, ""); code != 404 || r["check"] != "not_found" {
		t.Fatalf("no such workspace = %d %v", code, r)
	}
	if code, r := call(t, "GET", base+"/workspaces/nope/stat", k, ""); code != 404 || r["check"] != "not_found" {
		t.Fatalf("not an id = %d %v", code, r)
	}
}

func TestWorkspaceReads(t *testing.T) {
	v := newEnv(t, nil)
	withChatAgent(t, v)
	k := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/chat/conversations", k, `{"body":"hi","wake":true}`)
	conv, run := r["conversation_id"].(string), r["run_id"].(string)
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)
	putSnapshot(t, v, conv, run, map[string][]byte{
		"src/main.go":       []byte("package main\r\n"),
		"src/zz.txt":        []byte("z"),
		"notes.md":          []byte("# notes\n"),
		"a.log":             []byte("log"),
		".gitignore":        []byte("*.log\n"),
		"bin.dat":           []byte("a\x00b"),
		"big.txt":           bytes.Repeat([]byte("x"), 2<<20+1),
		"pic.png":           png,
		".orbit/claude/x":   []byte("{}"),
		"inputs/run/a.json": []byte("{}"),
	})
	_, r = call(t, "GET", base+"/conversations/"+conv+"/workspace", k, "")
	ws := r["workspace"].(map[string]any)
	wid := ws["id"].(string)
	asOf, _ := ws["as_of"].(map[string]any)
	if asOf == nil || asOf["run_id"] != run || asOf["snapshot_id"] == nil {
		t.Fatalf("as_of = %v", ws)
	}
	wsURL := base + "/workspaces/" + wid

	code, r := call(t, "GET", wsURL+"/tree", k, "")
	var names []string
	for _, e := range r["entries"].([]any) {
		e := e.(map[string]any)
		names = append(names, e["name"].(string))
		if _, ok := e["version"]; ok {
			t.Fatalf("a tree entry has no version: %v", e)
		}
		switch e["name"] {
		case "src":
			if e["kind"] != "dir" || e["has_children"] != true || e["path"] != "src" {
				t.Fatalf("src = %v", e)
			}
		case "a.log":
			if e["ignored"] != true || e["has_children"] != nil || e["git"] != nil || e["mode"] != "0644" {
				t.Fatalf("a.log = %v", e)
			}
		case "notes.md":
			if e["ignored"] != false || e["bytes"] != float64(8) {
				t.Fatalf("notes.md = %v", e)
			}
		}
	}
	if code != 200 || strings.Join(names, ",") != "src,.gitignore,a.log,big.txt,bin.dat,notes.md,pic.png" {
		t.Fatalf("root = %d %v (orbit's own state hidden)", code, names)
	}
	// Paging: every entry once, in order.
	var paged []string
	cursor := ""
	for i := 0; i < 10; i++ {
		_, r := call(t, "GET", wsURL+"/tree?limit=3&cursor="+url.QueryEscape(cursor), k, "")
		for _, e := range r["entries"].([]any) {
			paged = append(paged, e.(map[string]any)["name"].(string))
		}
		next, _ := r["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if strings.Join(paged, ",") != strings.Join(names, ",") {
		t.Fatalf("paged = %v", paged)
	}
	if code, r := call(t, "GET", wsURL+"/tree?cursor=bad!", k, ""); code != 400 || r["check"] != "request" {
		t.Fatalf("bad cursor = %d %v", code, r)
	}
	if code, r := call(t, "GET", wsURL+"/tree?path=src", k, ""); code != 200 || len(r["entries"].([]any)) != 2 || r["path"] != "src" {
		t.Fatalf("src = %d %v", code, r)
	}
	if code, r := call(t, "GET", wsURL+"/tree?path=notes.md", k, ""); code != 400 || r["check"] != "path" {
		t.Fatalf("tree of a file = %d %v", code, r)
	}

	code, r = call(t, "GET", wsURL+"/stat?path=src/main.go", k, "")
	e := r["entry"].(map[string]any)
	if code != 200 || e["version"] != workspace.Version([]byte("package main\r\n")) || e["kind"] != "file" || e["name"] != "main.go" {
		t.Fatalf("stat = %d %v", code, r)
	}
	if code, r := call(t, "GET", wsURL+"/stat?path=src", k, ""); code != 200 || r["entry"].(map[string]any)["version"] != nil {
		t.Fatalf("a directory has no version: %d %v", code, r)
	}
	for _, bad := range []string{"../x", "/etc/passwd", "src/", "./src"} {
		if code, r := call(t, "GET", wsURL+"/stat?path="+url.QueryEscape(bad), k, ""); code != 400 || r["check"] != "path" {
			t.Fatalf("%s = %d %v", bad, code, r)
		}
	}
	for _, missing := range []string{"nope", ".orbit/claude/x", "inputs/run/a.json"} {
		if code, r := call(t, "GET", wsURL+"/stat?path="+url.QueryEscape(missing), k, ""); code != 404 || r["check"] != "not_found" {
			t.Fatalf("%s = %d %v", missing, code, r)
		}
	}

	// The editor's read: text byte for byte, its version as the ETag.
	code, h, b := raw(t, "GET", wsURL+"/files?path=src/main.go", k)
	var f struct {
		Entry  map[string]any `json:"entry"`
		Text   *string        `json:"text"`
		Reason *string        `json:"reason"`
	}
	_ = json.Unmarshal(b, &f)
	if code != 200 || f.Text == nil || *f.Text != "package main\r\n" || f.Reason != nil ||
		h.Get("ETag") != `"`+workspace.Version([]byte("package main\r\n"))+`"` {
		t.Fatalf("read = %d %s %v", code, b, h)
	}
	for p, reason := range map[string]string{"bin.dat": "binary", "big.txt": "too_large", "pic.png": "binary"} {
		_, _, b := raw(t, "GET", wsURL+"/files?path="+p, k)
		f.Text, f.Reason = nil, nil
		_ = json.Unmarshal(b, &f)
		if f.Text != nil || f.Reason == nil || *f.Reason != reason || f.Entry["version"] == nil {
			t.Fatalf("%s = %s", p, b)
		}
	}
	if code, r := call(t, "GET", wsURL+"/files?path=src", k, ""); code != 400 || r["check"] != "path" {
		t.Fatalf("read a directory = %d %v", code, r)
	}
	if code, r := call(t, "GET", wsURL+"/files?path=notes.md&ref=x", k, ""); code != 404 || r["check"] != "not_found" {
		t.Fatalf("a ref = %d %v", code, r)
	}

	// Downloads: never a page, an image inline only when asked.
	code, h, b = raw(t, "GET", wsURL+"/download?path=notes.md", k)
	if code != 200 || string(b) != "# notes\n" || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") ||
		h.Get("Content-Type") != "application/octet-stream" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download = %d %q %v", code, b, h)
	}
	if _, h, _ := raw(t, "GET", wsURL+"/download?path=pic.png&inline=1", k); h.Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "inline") {
		t.Fatalf("inline image = %v", h)
	}
	if _, h, _ := raw(t, "GET", wsURL+"/download?path=notes.md&inline=1", k); !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") {
		t.Fatalf("inline text = %v", h)
	}
	code, h, b = raw(t, "GET", wsURL+"/download?path=src", k)
	if code != 200 || h.Get("Content-Type") != "application/gzip" || !strings.Contains(h.Get("Content-Disposition"), "src.tar.gz") {
		t.Fatalf("directory download = %d %v", code, h)
	}
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for tr := tar.NewReader(gz); ; {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		c, _ := io.ReadAll(tr)
		got[h.Name] = string(c)
	}
	if got["src/main.go"] != "package main\r\n" || got["src/zz.txt"] != "z" {
		t.Fatalf("tar = %v", got)
	}
	code, _, b = raw(t, "GET", wsURL+"/download", k)
	if code != 200 || len(b) == 0 {
		t.Fatalf("whole workspace = %d", code)
	}
	gz, _ = gzip.NewReader(bytes.NewReader(b))
	for tr := tar.NewReader(gz); ; {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if strings.HasPrefix(h.Name, ".orbit") || strings.HasPrefix(h.Name, "inputs") {
			t.Fatalf("orbit's state in the download: %s", h.Name)
		}
	}

	if code, r := call(t, "GET", wsURL+"/events", k, ""); code != 200 || len(r["events"].([]any)) != 0 || r["next_before"] != nil {
		t.Fatalf("events = %d %v", code, r)
	}
	if code, r := call(t, "GET", wsURL+"/events?before=x", k, ""); code != 400 || r["check"] != "request" {
		t.Fatalf("bad before = %d %v", code, r)
	}
}

func TestWorkspaceOfAConversationThatIsNotAnID(t *testing.T) {
	v := newEnv(t, nil)
	if code, r := call(t, "GET", v.srv.URL+"/api/v1/conversations/nope/workspace", v.person("kanishk"), ""); code != 404 || r["check"] != "not_found" {
		t.Fatalf("not an id = %d %v", code, r)
	}
}
