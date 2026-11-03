package api_test

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestWorkspaceSearch(t *testing.T) {
	v := newEnv(t, fastWorkspace)
	withChatAgent(t, v)
	k := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/chat/conversations", k, `{"body":"hi","wake":true}`)
	conv := r["conversation_id"].(string)
	long := strings.Repeat("x", 300) + "needle" + strings.Repeat("y", 300)
	finishRun(t, v, conv, map[string][]byte{
		"src/a.go":         []byte("package a\n\nfunc Needle() {}\n// needle and needle\n"),
		"src/b.txt":        []byte("héllo needle\r\n"),
		"long.txt":         []byte(long + "\n"),
		"unicode.txt":      []byte("a😀needle\n"),
		"bin.dat":          []byte("needle\x00"),
		"big.txt":          append(bytes.Repeat([]byte("z"), 2<<20), []byte("needle")...),
		".gitignore":       []byte("build/\n"),
		"build/out.txt":    []byte("needle\n"),
		"build/nested.txt": []byte("needle\n"),
		".orbit/claude/x":  []byte("needle"),
		"inputs/run/a.txt": []byte("needle"),
	})
	_, r = call(t, "GET", base+"/conversations/"+conv+"/workspace", k, "")
	ws := base + "/workspaces/" + r["workspace"].(map[string]any)["id"].(string)
	search := func(q string) (int, map[string]any) {
		t.Helper()
		code, _, r := wsCall(t, "GET", ws+"/search?"+q, k, nil)
		return code, r
	}
	where := func(r map[string]any) string {
		var out []string
		for _, m := range r["matches"].([]any) {
			m := m.(map[string]any)
			out = append(out, fmt.Sprintf("%s:%v:%v-%v", m["path"], m["line"], m["column"], m["end_column"]))
		}
		return strings.Join(out, " ")
	}

	// Case-insensitive by default, in tree order (directories first); binary,
	// too large, ignored and orbit's own files are not searched.
	code, r := search("q=needle")
	if code != 200 || r["truncated"] != false || r["files_searched"] != 5.0 {
		t.Fatalf("search = %d %v", code, r)
	}
	if got := where(r); got != "src/a.go:3:6-12 src/a.go:4:4-10 src/a.go:4:15-21 src/b.txt:1:7-13 long.txt:1:301-307 unicode.txt:1:4-10" {
		t.Fatalf("matches = %s", got)
	}
	ms := r["matches"].([]any)
	lp := ms[4].(map[string]any)
	if len(lp["preview"].(string)) != 200 || !strings.Contains(lp["preview"].(string), "needle") ||
		lp["preview_column"].(float64)+float64(strings.Index(lp["preview"].(string), "needle")) != 301 {
		t.Fatalf("long preview = %v", lp)
	}
	// A column counts UTF-16 units: é is one and the emoji is two. The CR is
	// not in the preview.
	if b := ms[3].(map[string]any); b["preview"] != "héllo needle" || b["preview_column"] != 1.0 {
		t.Fatalf("b.txt = %v", b)
	}
	if code, r := search("q=needle&path=unicode.txt"); code != 200 || where(r) != "unicode.txt:1:4-10" {
		t.Fatalf("UTF-16 columns = %d %v", code, r)
	}
	if _, r := search("q=Needle&case=1"); where(r) != "src/a.go:3:6-12" {
		t.Fatalf("case = %s", where(r))
	}
	if _, r := search("q=" + url.QueryEscape(`func \w+\(`) + "&regex=1"); where(r) != "src/a.go:3:1-13" {
		t.Fatalf("regex = %s", where(r))
	}
	if _, r := search("q=" + url.QueryEscape(`\w+(`)); len(r["matches"].([]any)) != 0 {
		t.Fatalf("literal = %v", r)
	}
	if _, r := search("q=needle&path=src"); r["files_searched"] != 2.0 || len(r["matches"].([]any)) != 4 {
		t.Fatalf("subtree = %v", r)
	}
	if _, r := search("q=needle&path=build/out.txt"); len(r["matches"].([]any)) != 1 {
		t.Fatalf("an ignored file named directly = %v", r)
	}
	if _, r := search("q=needle&path=build"); r["files_searched"] != 2.0 || len(r["matches"].([]any)) != 2 {
		t.Fatalf("an ignored directory named directly = %v", r)
	}
	if _, r := search("q=needle&limit=2"); len(r["matches"].([]any)) != 2 || r["truncated"] != true {
		t.Fatalf("limit = %v", r)
	}
	for _, bad := range []string{"q=", "q=" + strings.Repeat("a", 257), "q=(&regex=1", "q=x&path=../a", "q=x&limit=x"} {
		if code, r := search(bad); code != 400 {
			t.Fatalf("%s = %d %v", bad, code, r)
		}
	}
	if code, r := search("q=x&path=nope"); code != 404 || r["check"] != "not_found" {
		t.Fatalf("no such path = %d %v", code, r)
	}
}
