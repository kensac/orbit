package api_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/workspace"
)

// wsCall is a workspace request with headers, decoded.
func wsCall(t *testing.T, method, u, tok string, body []byte, headers ...string) (int, http.Header, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, u, bytes.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	code, h, b := do(t, req)
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	return code, h, v
}

func chatWorkspace(t *testing.T, v *env, tok string) (conv, wsURL string) {
	t.Helper()
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/chat/conversations", tok, `{"title":"edit","wake":false}`)
	conv = r["conversation_id"].(string)
	_, r = call(t, "GET", base+"/conversations/"+conv+"/workspace", tok, "")
	return conv, base + "/workspaces/" + r["workspace"].(map[string]any)["id"].(string)
}

func multipartFiles(t *testing.T, files ...string) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for i := 0; i+1 < len(files); i += 2 {
		w, err := mw.CreateFormFile("file", files[i])
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(files[i+1]))
	}
	_ = mw.Close()
	return &b, mw.FormDataContentType()
}

// fastWorkspace lifts the workspace's rates for a test that makes many
// requests at once.
func fastWorkspace(a *api.Server, _ *http.Server) {
	a.WorkspaceReadRate, a.WorkspaceWriteRate = 1e6, 1e6
}

func TestWorkspaceWrites(t *testing.T) {
	v := newEnv(t, fastWorkspace)
	withChatAgent(t, v)
	k := v.person("kanishk")
	conv, ws := chatWorkspace(t, v, k)
	q := func(p string) string { return "?path=" + url.QueryEscape(p) }

	// Create, then edit only with the version that was read.
	code, _, r := wsCall(t, "PUT", ws+"/files"+q("notes/a.md"), k, []byte("one"), "If-None-Match", "*")
	if code != 201 || r["entry"].(map[string]any)["version"] != workspace.Version([]byte("one")) {
		t.Fatalf("create = %d %v", code, r)
	}
	v1 := workspace.Version([]byte("one"))
	if code, _, r := wsCall(t, "PUT", ws+"/files"+q("notes/a.md"), k, []byte("x"), "If-None-Match", "*"); code != 409 || r["check"] != "exists" {
		t.Fatalf("create over = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "PUT", ws+"/files"+q("notes/a.md"), k, []byte("x")); code != 428 || r["check"] != "version_required" {
		t.Fatalf("no If-Match = %d %v", code, r)
	}
	code, _, r = wsCall(t, "PUT", ws+"/files"+q("notes/a.md"), k, []byte("x"), "If-Match", `"sha256:00"`)
	if cur, _ := r["current"].(map[string]any); code != 412 || r["check"] != "version" || cur["version"] != v1 {
		t.Fatalf("stale If-Match = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "PUT", ws+"/files"+q("gone.md"), k, []byte("x"), "If-Match", `"`+v1+`"`); code != 412 || r["current"] != nil {
		t.Fatalf("If-Match on a missing file = %d %v", code, r)
	}
	code, h, r := wsCall(t, "PUT", ws+"/files"+q("notes/a.md"), k, []byte("two"), "If-Match", `"`+v1+`"`, "X-Request-Id", "req-edit")
	if code != 200 || h.Get("X-Request-Id") != "req-edit" {
		t.Fatalf("edit = %d %v", code, r)
	}
	if _, _, b := raw(t, "GET", ws+"/files"+q("notes/a.md"), k); !strings.Contains(string(b), `"text":"two"`) {
		t.Fatalf("read back = %s", b)
	}
	for p, check := range map[string]string{".orbit/x": "path", "notes": "path", "../x": "path", "": "path"} {
		if code, _, r := wsCall(t, "PUT", ws+"/files"+q(p), k, []byte("x"), "If-None-Match", "*"); code != 400 || r["check"] != check {
			t.Fatalf("PUT %q = %d %v", p, code, r)
		}
	}
	if code, _, r := wsCall(t, "PUT", ws+"/files"+q("big.txt"), k, bytes.Repeat([]byte("x"), api.EditorWriteMax+1)); code != 413 || r["check"] != "too_large" {
		t.Fatalf("too large = %d %v", code, r)
	}

	// mkdir, rename (never over a path), trash and restore.
	if code, _, r := wsCall(t, "POST", ws+"/mkdir", k, []byte(`{"path":"empty/dir"}`)); code != 201 || r["entry"].(map[string]any)["kind"] != "dir" {
		t.Fatalf("mkdir = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "POST", ws+"/mkdir", k, []byte(`{"path":"empty/dir"}`)); code != 409 || r["check"] != "exists" {
		t.Fatalf("mkdir twice = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "POST", ws+"/mkdir", k, []byte(`{"path":"x","extra":1}`)); code != 400 || r["check"] != "request" {
		t.Fatalf("unknown field = %d %v", code, r)
	}
	v2 := workspace.Version([]byte("two"))
	if code, _, r := wsCall(t, "POST", ws+"/rename", k, []byte(`{"from":"notes/a.md","to":"b.md","version":"`+v1+`"}`)); code != 412 {
		t.Fatalf("rename at a stale version = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "POST", ws+"/rename", k, []byte(`{"from":"notes/a.md","to":"empty"}`)); code != 409 || r["check"] != "exists" {
		t.Fatalf("rename over = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "POST", ws+"/rename", k, []byte(`{"from":"notes/a.md","to":"b.md","version":"`+v2+`"}`)); code != 200 || r["entry"].(map[string]any)["path"] != "b.md" {
		t.Fatalf("rename = %d %v", code, r)
	}
	code, _, r = wsCall(t, "POST", ws+"/trash", k, []byte(`{"path":"b.md"}`))
	item, _ := r["trash"].(map[string]any)
	if code != 200 || item["path"] != "b.md" || item["kind"] != "file" || item["bytes"] != float64(3) || item["by"] != "kanishk" || item["restored_at"] != nil {
		t.Fatalf("trash = %d %v", code, r)
	}
	if code, _, _ := wsCall(t, "GET", ws+"/stat"+q("b.md"), k, nil); code != 404 {
		t.Fatalf("a trashed path is gone from the tree: %d", code)
	}
	if code, _, r := wsCall(t, "GET", ws+"/trash", k, nil); code != 200 || len(r["items"].([]any)) != 1 || r["next_cursor"] != nil {
		t.Fatalf("trash list = %d %v", code, r)
	}
	id := item["id"].(string)
	if code, _, r := wsCall(t, "POST", ws+"/trash/"+id+"/restore", k, nil); code != 200 || r["entry"].(map[string]any)["version"] != v2 {
		t.Fatalf("restore = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "POST", ws+"/trash/"+id+"/restore", k, nil); code != 409 || r["check"] != "exists" {
		t.Fatalf("restore over = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "POST", ws+"/trash/"+id+"/restore", k, []byte(`{"to":"copy/b.md"}`)); code != 200 || r["entry"].(map[string]any)["path"] != "copy/b.md" {
		t.Fatalf("restore elsewhere = %d %v", code, r)
	}
	if _, _, r := wsCall(t, "GET", ws+"/trash", k, nil); r["items"].([]any)[0].(map[string]any)["restored_at"] == nil {
		t.Fatalf("restored_at = %v", r)
	}
	if code, _, _ := wsCall(t, "POST", ws+"/trash/00000000-0000-0000-0000-000000000000/restore", k, nil); code != 404 {
		t.Fatalf("no such item = %d", code)
	}

	// Uploads: many files into one snapshot; overwrite trashes the old one.
	body, ct := multipartFiles(t, "one.txt", "1", "two.txt", "2")
	code, _, r = wsCall(t, "POST", ws+"/upload?dir=up", k, body.Bytes(), "Content-Type", ct)
	if code != 201 || len(r["entries"].([]any)) != 2 || r["entries"].([]any)[0].(map[string]any)["path"] != "up/one.txt" {
		t.Fatalf("upload = %d %v", code, r)
	}
	body, ct = multipartFiles(t, "one.txt", "uno")
	if code, _, r := wsCall(t, "POST", ws+"/upload?dir=up", k, body.Bytes(), "Content-Type", ct); code != 409 || r["check"] != "exists" {
		t.Fatalf("upload over = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "POST", ws+"/upload?dir=up&overwrite=1", k, body.Bytes(), "Content-Type", ct); code != 201 {
		t.Fatalf("overwrite = %d %v", code, r)
	}
	if _, _, r := wsCall(t, "GET", ws+"/trash", k, nil); r["items"].([]any)[0].(map[string]any)["path"] != "up/one.txt" {
		t.Fatalf("the overwritten file is in the trash: %v", r)
	}

	// A signed upload: one PUT, no token, at most its bytes.
	code, _, r = wsCall(t, "POST", ws+"/upload/url", k, []byte(`{"path":"media/clip.bin","bytes":10,"media_type":"video/mp4"}`))
	signed, _ := r["url"].(string)
	if code != 200 || r["method"] != "PUT" || r["max_bytes"] != float64(10) {
		t.Fatalf("upload url = %d %v", code, r)
	}
	signed = strings.Replace(signed, "http://orbit.test", v.srv.URL, 1)
	if code, _, _ := wsCall(t, "PUT", strings.Replace(signed, "b=10", "b=99", 1), "", []byte("0123")); code != 403 {
		t.Fatalf("tampered = %d", code)
	}
	if code, _, _ := wsCall(t, "PUT", signed, "", []byte("0123456789x")); code != 413 {
		t.Fatalf("over its bytes = %d", code)
	}
	if code, _, r := wsCall(t, "PUT", signed, "", []byte("0123456789")); code != 201 || r["entry"].(map[string]any)["bytes"] != float64(10) {
		t.Fatalf("signed put = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "PUT", signed, "", []byte("0123456789")); code != 409 {
		t.Fatalf("second put = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "POST", ws+"/upload/url", k, []byte(`{"path":"media/clip.bin","bytes":10}`)); code != 409 || r["check"] != "exists" {
		t.Fatalf("url for an existing path = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "POST", ws+"/upload/url", k, []byte(`{"path":"huge.bin","bytes":`+"300000000"+`}`)); code != 413 || r["check"] != "too_large" {
		t.Fatalf("larger than a snapshot = %d %v", code, r)
	}

	// The audit: every write, newest first, with its request id.
	_, _, r = wsCall(t, "GET", ws+"/events?limit=500", k, nil)
	var ops []string
	sawRequest := false
	for _, e := range r["events"].([]any) {
		e := e.(map[string]any)
		ops = append(ops, e["op"].(string))
		if e["request_id"] == "req-edit" && e["op"] == "write" && e["version_before"] == v1 && e["version_after"] == v2 {
			sawRequest = true
		}
	}
	if got := strings.Join(ops, ","); got != "upload,upload,trash,upload,upload,restore,restore,trash,rename,mkdir,write,write" || !sawRequest {
		t.Fatalf("events = %s %v", got, r)
	}
	_, _, r = wsCall(t, "GET", ws+"/events?limit=5", k, nil)
	if next, _ := r["next_before"].(string); next == "" {
		t.Fatalf("paged events = %v", r)
	} else if _, _, r := wsCall(t, "GET", ws+"/events?limit=500&before="+next, k, nil); len(r["events"].([]any)) != 7 {
		t.Fatalf("next page = %v", r)
	}

	// A write while a run of the conversation is running is refused.
	_, r2 := call(t, "POST", v.srv.URL+"/api/v1/conversations/"+conv+"/messages", k, `{"body":"go","wake":true}`)
	if r2["run_id"] == nil {
		t.Fatalf("message = %v", r2)
	}
	if c, err := v.s.ClaimRun(ctx, "w"); err != nil || c == nil {
		t.Fatalf("claim = %v %v", c, err)
	}
	if code, _, r := wsCall(t, "POST", ws+"/mkdir", k, []byte(`{"path":"later"}`)); code != 409 || r["check"] != "busy" {
		t.Fatalf("busy = %d %v", code, r)
	}
}

func TestWorkspaceWritesNeedTheWorkspaceGroup(t *testing.T) {
	v := newEnv(t, func(a *api.Server, _ *http.Server) { a.WorkspaceGroup = "workspace" })
	withChatAgent(t, v)
	outside := v.person("guest")
	c := v.is.Valid(clientID, group, "kanishk")
	c.Groups = append(c.Groups, "workspace")
	inside := v.is.Mint(c)
	conv, ws := chatWorkspace(t, v, outside)
	_, r := call(t, "GET", v.srv.URL+"/api/v1/conversations/"+conv+"/workspace", outside, "")
	if r["workspace"].(map[string]any)["capabilities"].(map[string]any)["write"] != false {
		t.Fatalf("outside the group, write is false: %v", r)
	}
	if code, _, r := wsCall(t, "POST", ws+"/mkdir", outside, []byte(`{"path":"x"}`)); code != 403 || r["check"] != "group" {
		t.Fatalf("outside = %d %v", code, r)
	}
	if code, _, _ := wsCall(t, "GET", ws+"/tree", outside, nil); code != 200 {
		t.Fatalf("reads stay open = %d", code)
	}
	_, r = call(t, "GET", v.srv.URL+"/api/v1/conversations/"+conv+"/workspace", inside, "")
	if r["workspace"].(map[string]any)["capabilities"].(map[string]any)["write"] != true {
		t.Fatalf("inside the group, write is true: %v", r)
	}
	if code, _, r := wsCall(t, "POST", ws+"/mkdir", inside, []byte(`{"path":"x"}`)); code != 201 {
		t.Fatalf("inside = %d %v", code, r)
	}
}
