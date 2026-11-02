package api_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/workspace"
)

func multipartFile(t *testing.T, name, ctype string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	h := map[string][]string{"Content-Disposition": {`form-data; name="file"; filename="` + name + `"`}, "Content-Type": {ctype}}
	w, err := mw.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(content)
	_ = mw.Close()
	return &b, mw.FormDataContentType()
}

func do(t *testing.T, req *http.Request) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func TestUploadsAndAttachments(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/a/conversations", kanishk, `{"title":"files"}`)
	conv := r["conversation_id"].(string)

	upload := func(name, ctype string, content []byte) map[string]any {
		t.Helper()
		body, ct := multipartFile(t, name, ctype, content)
		req, _ := http.NewRequest("POST", base+"/conversations/"+conv+"/uploads", body)
		req.Header.Set("Authorization", "Bearer "+kanishk)
		req.Header.Set("Content-Type", ct)
		code, _, b := do(t, req)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		if code != 201 {
			t.Fatalf("upload %s = %d %s", name, code, b)
		}
		return out["upload"].(map[string]any)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	img := upload("../../shot.png", "image/png", png)
	if img["name"] != "shot.png" || img["media_type"] != "image/png" || img["bytes"].(float64) != float64(len(png)) {
		t.Fatalf("image = %v", img)
	}
	again := upload("shot.png", "image/png", []byte("other"))
	if again["name"] != "shot (2).png" {
		t.Fatalf("a taken name is numbered: %v", again)
	}
	notes := upload("notes.txt", "text/plain", []byte("hello"))

	// Served: images inline, everything else as a download.
	get := func(id string) (int, http.Header, []byte) {
		req, _ := http.NewRequest("GET", base+"/conversations/"+conv+"/uploads/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+kanishk)
		return do(t, req)
	}
	if code, h, b := get(img["id"].(string)); code != 200 || !bytes.Equal(b, png) || !strings.HasPrefix(h.Get("Content-Disposition"), "inline") {
		t.Fatalf("get image = %d %v", code, h)
	}
	if code, h, _ := get(notes["id"].(string)); code != 200 || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("get notes = %d %v", code, h)
	}

	// A message carries them; the thread shows them on it.
	code, r := call(t, "POST", base+"/conversations/"+conv+"/messages", kanishk,
		`{"body":"","attachments":["`+img["id"].(string)+`","`+notes["id"].(string)+`"],"wake":false}`)
	if code != 201 {
		t.Fatalf("message = %d %v", code, r)
	}
	_, r = call(t, "GET", base+"/conversations/"+conv+"/thread", kanishk, "")
	items := r["items"].([]any)
	m := items[len(items)-1].(map[string]any)
	att := m["attachments"].([]any)
	if len(att) != 2 || att[0].(map[string]any)["name"] != "shot.png" || att[1].(map[string]any)["name"] != "notes.txt" {
		t.Fatalf("attachments = %v", m)
	}

	// An upload of another conversation is refused.
	_, r = call(t, "POST", base+"/agents/a/conversations", kanishk, `{"title":"other"}`)
	other := r["conversation_id"].(string)
	if code, r := call(t, "POST", base+"/conversations/"+other+"/messages", kanishk,
		`{"body":"x","attachments":["`+img["id"].(string)+`"]}`); code != 400 {
		t.Fatalf("foreign attachment = %d %v", code, r)
	}

	// A signed URL: one PUT, within its size, once.
	code, r = call(t, "POST", base+"/conversations/"+conv+"/uploads/url", kanishk, `{"name":"big.bin","media_type":"application/octet-stream","bytes":10}`)
	if code != 200 || r["method"] != "PUT" {
		t.Fatalf("url = %d %v", code, r)
	}
	u, _ := url.Parse(r["url"].(string))
	signed := v.srv.URL + u.Path + "?" + u.RawQuery
	put := func(target string, body string) int {
		req, _ := http.NewRequest("PUT", target, strings.NewReader(body))
		code, _, _ := do(t, req)
		return code
	}
	if code := put(signed, "01234567890"); code != 413 {
		t.Fatalf("too large = %d", code)
	}
	tampered := strings.Replace(signed, "b=10", "b=99", 1)
	if code := put(tampered, "0123"); code != 403 {
		t.Fatalf("tampered = %d", code)
	}
	if code := put(signed, "0123456789"); code != 201 {
		t.Fatalf("signed put = %d", code)
	}
	if code := put(signed, "0123456789"); code != 409 {
		t.Fatalf("second put = %d", code)
	}
}

// TestAnExpiredSignedUploadURLIs403: a URL signed correctly but past its expiry uploads nothing.
func TestAnExpiredSignedUploadURLIs403(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/a/conversations", kanishk, `{"title":"files"}`)
	conv := r["conversation_id"].(string)
	_, r = call(t, "POST", base+"/conversations/"+conv+"/uploads/url", kanishk, `{"name":"late.bin","bytes":4}`)
	u, _ := url.Parse(r["url"].(string))
	q := u.Query()
	q.Set("e", strconv.FormatInt(time.Now().Add(-time.Second).Unix(), 10))
	// Re-signed as the server signs (api.uploadSig), so only the expiry is wrong.
	key := hmac.New(sha256.New, []byte(token))
	key.Write([]byte("orbit upload url"))
	m := hmac.New(sha256.New, key.Sum(nil))
	for _, k := range []string{"c", "n", "t", "b", "e", "a", "k"} {
		m.Write([]byte(k + "=" + q.Get(k) + "\x00"))
	}
	q.Set("s", hex.EncodeToString(m.Sum(nil)))
	req, _ := http.NewRequest("PUT", v.srv.URL+u.Path+"?"+q.Encode(), strings.NewReader("late"))
	if code, _, b := do(t, req); code != 403 || !strings.Contains(string(b), "expired") {
		t.Fatalf("expired link = %d %s", code, b)
	}
	var n int
	if err := v.s.Pool.QueryRow(ctx, `SELECT count(*) FROM uploads`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("uploads stored = %d, %v", n, err)
	}
}

// TestStoredFilesStreamWithRanges: an upload and an artifact larger than one store slice come back whole, with Content-Length, and a Range request gets only its bytes.
func TestStoredFilesStreamWithRanges(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/a/conversations", kanishk, `{"title":"files"}`)
	conv := r["conversation_id"].(string)
	content := make([]byte, 3<<20+777)
	for i := range content {
		content[i] = byte(i % 251)
	}
	u, err := v.s.CreateUpload(ctx, store.NewUpload{ConversationID: conv, Name: "clip.mp4", MediaType: "video/mp4", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	run := enqueue(t, v)
	if err := v.s.PutArtifact(ctx, run, 1, "in", "inputs/big.json", "application/json", content); err != nil {
		t.Fatal(err)
	}
	arts, err := v.s.ListArtifacts(ctx, run)
	if err != nil || len(arts) != 1 {
		t.Fatalf("artifacts = %v, %v", arts, err)
	}
	for _, target := range []string{base + "/conversations/" + conv + "/uploads/" + u.ID, base + "/artifacts/" + arts[0].ID} {
		get := func(rng string) (int, http.Header, []byte) {
			req, _ := http.NewRequest("GET", target, nil)
			req.Header.Set("Authorization", "Bearer "+kanishk)
			if rng != "" {
				req.Header.Set("Range", rng)
			}
			return do(t, req)
		}
		code, h, b := get("")
		if code != 200 || !bytes.Equal(b, content) || h.Get("Content-Length") != strconv.Itoa(len(content)) || h.Get("Accept-Ranges") != "bytes" ||
			h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("GET %s = %d, %d bytes, %v", target, code, len(b), h)
		}
		lo, hi := 1<<20-5, 2<<20+5
		code, h, b = get("bytes=" + strconv.Itoa(lo) + "-" + strconv.Itoa(hi))
		if code != 206 || !bytes.Equal(b, content[lo:hi+1]) || h.Get("Content-Range") != "bytes "+strconv.Itoa(lo)+"-"+strconv.Itoa(hi)+"/"+strconv.Itoa(len(content)) {
			t.Fatalf("range of %s = %d, %d bytes, %v", target, code, len(b), h)
		}
		if code, _, _ := get("bytes=" + strconv.Itoa(len(content)) + "-"); code != 416 {
			t.Fatalf("a range past the end of %s = %d", target, code)
		}
	}
}

func TestWorkspaceFiles(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/a/conversations", kanishk, `{"title":"ws","body":"hi","wake":true}`)
	conv, run := r["conversation_id"].(string), r["run_id"].(string)
	if code, r := call(t, "GET", base+"/conversations/"+conv+"/files", kanishk, ""); code != 200 || len(r["files"].([]any)) != 0 {
		t.Fatalf("empty = %d %v", code, r)
	}
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "src"), 0o700)
	_ = os.MkdirAll(filepath.Join(dir, ".orbit", "claude"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, ".orbit", "claude", "x.jsonl"), []byte("{}"), 0o600)
	tree, n, err := workspace.Pack(workspace.Tree{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.s.PutWorkspace(ctx, conv, run, 1, n, tree); err != nil {
		t.Fatal(err)
	}
	code, r := call(t, "GET", base+"/conversations/"+conv+"/files", kanishk, "")
	files := r["files"].([]any)
	if code != 200 || len(files) != 1 || files[0].(map[string]any)["path"] != "src/main.go" || files[0].(map[string]any)["bytes"].(float64) != 13 {
		t.Fatalf("files = %d %v", code, r)
	}
	req, _ := http.NewRequest("GET", base+"/conversations/"+conv+"/files/src/main.go", nil)
	req.Header.Set("Authorization", "Bearer "+kanishk)
	code, h, b := do(t, req)
	if code != 200 || string(b) != "package main\n" || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") {
		t.Fatalf("file = %d %v %q", code, h, b)
	}
	req, _ = http.NewRequest("GET", base+"/conversations/"+conv+"/files/.orbit/claude/x.jsonl", nil)
	req.Header.Set("Authorization", "Bearer "+kanishk)
	if code, _, _ := do(t, req); code != 404 {
		t.Fatalf("orbit's state = %d", code)
	}
}
