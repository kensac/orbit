package api_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/workspace"
)

// wsEvent is one Server-Sent Event.
type wsEvent struct {
	Event, ID string
	Data      map[string]any
}

// openSSE connects to a stream and returns its events as they arrive.
func wsSSE(t *testing.T, u, tok, lastID string) (<-chan wsEvent, func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ch := make(chan wsEvent, 100)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		var ev wsEvent
		for sc.Scan() {
			l := sc.Text()
			switch {
			case strings.HasPrefix(l, "event: "):
				ev.Event = l[7:]
			case strings.HasPrefix(l, "id: "):
				ev.ID = l[4:]
			case strings.HasPrefix(l, "data: "):
				_ = json.Unmarshal([]byte(l[6:]), &ev.Data)
			case l == "" && ev.Event != "":
				ch <- ev
				ev = wsEvent{}
			}
		}
	}()
	return ch, func() { _ = resp.Body.Close() }
}

func nextEv(t *testing.T, ch <-chan wsEvent) wsEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("the stream ended")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event in 5 s")
	}
	return wsEvent{}
}

func fastChanges(a *api.Server, h *http.Server) {
	fastWorkspace(a, h)
	a.ChangesPoll = 20 * time.Millisecond
}

// finishRun claims the conversation's queued run, reads its messages,
// stores files as its snapshot and finishes it.
func finishRun(t *testing.T, v *env, conv string, files map[string][]byte) string {
	t.Helper()
	c, err := v.s.ClaimRun(ctx, "w")
	if err != nil || c == nil {
		t.Fatalf("claim = %v %v", c, err)
	}
	if _, err := v.s.StartRunContext(ctx, c.ID, c.Agent); err != nil {
		t.Fatal(err)
	}
	if files != nil {
		putSnapshot(t, v, conv, c.ID, files)
	}
	if err := v.s.FinishRun(ctx, c.ID, store.RunResult{Attempt: c.Attempt, Status: store.StatusSucceeded, Summary: "done"}); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func TestWorkspaceChangeStream(t *testing.T) {
	v := newEnv(t, fastChanges)
	withChatAgent(t, v)
	k := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/chat/conversations", k, `{"body":"hi","wake":true}`)
	conv := r["conversation_id"].(string)
	run := finishRun(t, v, conv, map[string][]byte{"src/a.go": []byte("a\n"), "notes.md": []byte("n\n")})
	_, r = call(t, "GET", base+"/conversations/"+conv+"/workspace", k, "")
	wsDoc := r["workspace"].(map[string]any)
	ws := base + "/workspaces/" + wsDoc["id"].(string)
	if wsDoc["capabilities"].(map[string]any)["watch"] != true {
		t.Fatalf("watch = %v", wsDoc)
	}
	q := func(p string) string { return "?path=" + url.QueryEscape(p) }

	// A first connection starts at the newest snapshot: ready, nothing replayed.
	ch, stop := wsSSE(t, ws+"/changes", k, "")
	ready := nextEv(t, ch)
	if ready.Event != "ready" || ready.Data["backend"] != "snapshot" || ready.Data["cursor"] != ready.ID || ready.ID == "" {
		t.Fatalf("ready = %+v", ready)
	}
	start := ready.ID
	// A person's write arrives, attributed.
	v1 := workspace.Version([]byte("n\n"))
	if code, _, r := wsCall(t, "PUT", ws+"/files"+q("notes.md"), k, []byte("N\n"), "If-Match", `"`+v1+`"`); code != 200 {
		t.Fatalf("write = %d %v", code, r)
	}
	ev := nextEv(t, ch)
	by := ev.Data["by"].(map[string]any)
	if ev.Event != "change" || ev.Data["path"] != "notes.md" || ev.Data["op"] != "modify" || ev.Data["kind"] != "file" ||
		ev.Data["version"] != workspace.Version([]byte("N\n")) || by["kind"] != "person" || by["person"] != "kanishk" || by["run_id"] != nil {
		t.Fatalf("change = %+v", ev)
	}
	// A rename, then a trash of a directory, then a restore.
	if code, _, r := wsCall(t, "POST", ws+"/rename", k, []byte(`{"from":"notes.md","to":"docs/notes.md"}`)); code != 200 {
		t.Fatalf("rename = %d %v", code, r)
	}
	var ops []string
	for range 2 {
		ev := nextEv(t, ch)
		ops = append(ops, ev.Data["op"].(string)+" "+ev.Data["path"].(string))
		if ev.Data["op"] == "rename" && ev.Data["from"] != "notes.md" {
			t.Fatalf("rename = %+v", ev)
		}
	}
	if strings.Join(ops, ",") != "create docs,rename docs/notes.md" {
		t.Fatalf("rename events = %v", ops)
	}
	_, _, r = wsCall(t, "POST", ws+"/trash", k, []byte(`{"path":"src"}`))
	item := r["trash"].(map[string]any)["id"].(string)
	ops = nil
	for range 2 {
		ev := nextEv(t, ch)
		ops = append(ops, ev.Data["op"].(string)+" "+ev.Data["path"].(string))
	}
	if strings.Join(ops, ",") != "trash src,trash src/a.go" {
		t.Fatalf("trash events = %v", ops)
	}
	if code, _, r := wsCall(t, "POST", ws+"/trash/"+item+"/restore", k, nil); code != 200 {
		t.Fatalf("restore = %d %v", code, r)
	}
	ops = nil
	var lastID string
	for range 2 {
		ev := nextEv(t, ch)
		ops = append(ops, ev.Data["op"].(string)+" "+ev.Data["path"].(string))
		lastID = ev.ID
	}
	if strings.Join(ops, ",") != "restore src,restore src/a.go" {
		t.Fatalf("restore events = %v", ops)
	}
	stop()

	// Resuming replays what came after the cursor, here from the start.
	ch, stop = wsSSE(t, ws+"/changes", k, start)
	if ev := nextEv(t, ch); ev.Event != "ready" || ev.ID != start {
		t.Fatalf("resumed ready = %+v", ev)
	}
	ops = nil
	for range 7 {
		ev := nextEv(t, ch)
		ops = append(ops, ev.Data["op"].(string)+" "+ev.Data["path"].(string))
	}
	if !strings.HasPrefix(strings.Join(ops, ","), "modify notes.md,create docs,rename docs/notes.md,trash src") {
		t.Fatalf("replay = %v", ops)
	}
	stop()

	// A subtree sees only its paths; a run's snapshot is the run's.
	ch, stop = wsSSE(t, ws+"/changes?path=src", k, lastID)
	if ev := nextEv(t, ch); ev.Event != "ready" || ev.ID != lastID {
		t.Fatalf("subtree ready = %+v", ev)
	}
	_, _ = call(t, "POST", base+"/conversations/"+conv+"/messages", k, `{"body":"more","wake":true}`)
	run2 := finishRun(t, v, conv, map[string][]byte{"src/a.go": []byte("a2\n"), "docs/notes.md": []byte("N\n"), "top.txt": []byte("t")})
	ev = nextEv(t, ch)
	by = ev.Data["by"].(map[string]any)
	if ev.Data["path"] != "src/a.go" || ev.Data["op"] != "modify" || by["kind"] != "run" || by["run_id"] != run2 || by["person"] != nil {
		t.Fatalf("run change = %+v", ev)
	}
	stop()

	// A cursor this workspace never had is lost: ready at the newest, then resync.
	ch, stop = wsSSE(t, ws+"/changes", k, "999999.1")
	if ev := nextEv(t, ch); ev.Event != "ready" {
		t.Fatalf("lost ready = %+v", ev)
	}
	if ev := nextEv(t, ch); ev.Event != "resync" || ev.Data["reason"] != "cursor_lost" {
		t.Fatalf("lost = %+v", ev)
	}
	stop()
	ch, stop = wsSSE(t, ws+"/changes", k, "nonsense")
	_ = nextEv(t, ch)
	if ev := nextEv(t, ch); ev.Event != "resync" {
		t.Fatalf("garbage cursor = %+v", ev)
	}
	stop()
	_ = run
}

func TestWorkspaceHistoryAndRefs(t *testing.T) {
	v := newEnv(t, fastWorkspace)
	withChatAgent(t, v)
	k := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/chat/conversations", k, `{"body":"hi","wake":true}`)
	conv := r["conversation_id"].(string)
	run := finishRun(t, v, conv, map[string][]byte{"a.md": []byte("one\n"), "b.md": []byte("b")})
	_, r = call(t, "GET", base+"/conversations/"+conv+"/workspace", k, "")
	ws := base + "/workspaces/" + r["workspace"].(map[string]any)["id"].(string)
	q := func(p string) string { return "?path=" + url.QueryEscape(p) }
	if code, _, r := wsCall(t, "PUT", ws+"/files"+q("a.md"), k, []byte("two\n"), "If-Match", "*"); code != 200 {
		t.Fatalf("write = %d %v", code, r)
	}
	// An unrelated change is not a version of a.md.
	if code, _, r := wsCall(t, "PUT", ws+"/files"+q("b.md"), k, []byte("B"), "If-Match", "*"); code != 200 {
		t.Fatalf("write = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "PUT", ws+"/files"+q("a.md"), k, []byte("three\n"), "If-Match", "*"); code != 200 {
		t.Fatalf("write = %d %v", code, r)
	}

	code, _, r := wsCall(t, "GET", ws+"/history"+q("a.md")+"&limit=2", k, nil)
	vs, _ := r["versions"].([]any)
	if code != 200 || len(vs) != 2 || r["next_before"] == nil {
		t.Fatalf("history = %d %v", code, r)
	}
	newest := vs[0].(map[string]any)
	if newest["version"] != workspace.Version([]byte("three\n")) || newest["source"] != "snapshot" ||
		newest["by"].(map[string]any)["kind"] != "person" || newest["by"].(map[string]any)["person"] != "kanishk" {
		t.Fatalf("newest = %v", newest)
	}
	_, _, r = wsCall(t, "GET", ws+"/history"+q("a.md")+"&before="+r["next_before"].(string), k, nil)
	vs = r["versions"].([]any)
	first := vs[0].(map[string]any)
	if len(vs) != 1 || r["next_before"] != nil || first["version"] != workspace.Version([]byte("one\n")) ||
		first["by"].(map[string]any)["kind"] != "run" || first["by"].(map[string]any)["run_id"] != run {
		t.Fatalf("older = %v", r)
	}

	// A version's ref reads it, for the editor and for download.
	ref := first["ref"].(string)
	code, h, r := wsCall(t, "GET", ws+"/files"+q("a.md")+"&ref="+url.QueryEscape(ref), k, nil)
	if code != 200 || r["text"] != "one\n" || h.Get("ETag") != `"`+workspace.Version([]byte("one\n"))+`"` {
		t.Fatalf("read at ref = %d %v", code, r)
	}
	if code, _, b := raw(t, "GET", ws+"/download"+q("a.md")+"&ref="+url.QueryEscape(ref), k); code != 200 || string(b) != "one\n" {
		t.Fatalf("download at ref = %d %q", code, b)
	}
	for _, bad := range []string{"nope", "s0", "s999999"} {
		if code, _, r := wsCall(t, "GET", ws+"/files"+q("a.md")+"&ref="+bad, k, nil); code != 404 || r["check"] != "not_found" {
			t.Fatalf("ref %q = %d %v", bad, code, r)
		}
	}
	// Another workspace's ref is not this one's.
	_, r = call(t, "POST", base+"/agents/chat/conversations", k, `{"title":"other","wake":false}`)
	_, r = call(t, "GET", base+"/conversations/"+r["conversation_id"].(string)+"/workspace", k, "")
	other := base + "/workspaces/" + r["workspace"].(map[string]any)["id"].(string)
	if code, _, r := wsCall(t, "GET", other+"/files"+q("a.md")+"&ref="+ref, k, nil); code != 404 {
		t.Fatalf("a ref of another workspace = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "GET", ws+"/history"+q("never.md"), k, nil); code != 200 || len(r["versions"].([]any)) != 0 {
		t.Fatalf("no history = %d %v", code, r)
	}
	if code, _, r := wsCall(t, "GET", ws+"/history", k, nil); code != 400 || r["check"] != "path" {
		t.Fatalf("history of the root = %d %v", code, r)
	}
}

func TestRunChanges(t *testing.T) {
	v := newEnv(t, fastWorkspace)
	withChatAgent(t, v)
	k := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/chat/conversations", k, `{"body":"hi","wake":true}`)
	conv := r["conversation_id"].(string)
	old := "package main\n\nfunc main() {\n\ta()\n\tb()\n}\n"
	first := finishRun(t, v, conv, map[string][]byte{"main.go": []byte(old), "gone.txt": []byte("x\ny\n"), "img.bin": []byte("a\x00")})
	_, _ = call(t, "POST", base+"/conversations/"+conv+"/messages", k, `{"body":"change it","wake":true}`)
	second := finishRun(t, v, conv, map[string][]byte{
		"main.go":    []byte(strings.Replace(old, "\tb()\n", "\tB()\n\tc()\n", 1)),
		"new.txt":    []byte("hello\n"),
		"img.bin":    []byte("b\x00"),
		"moved/x.md": []byte("x\ny\n"),
	})
	_, _ = call(t, "POST", base+"/conversations/"+conv+"/messages", k, `{"body":"nothing","wake":true}`)
	idle := finishRun(t, v, conv, nil)

	code, r := call(t, "GET", base+"/runs/"+second+"/changes", k, "")
	if code != 200 || r["run_id"] != second || r["truncated"] != false || r["before"] == nil || r["after"] == nil {
		t.Fatalf("changes = %d %v", code, r)
	}
	files := map[string]map[string]any{}
	for _, f := range r["files"].([]any) {
		f := f.(map[string]any)
		files[f["path"].(string)] = f
	}
	if len(files) != 4 || files["new.txt"]["status"] != "added" || files["img.bin"]["binary"] != true ||
		files["moved/x.md"]["status"] != "renamed" || files["moved/x.md"]["old_path"] != "gone.txt" {
		t.Fatalf("files = %v", r["files"])
	}
	m := files["main.go"]
	if m["status"] != "modified" || m["additions"] != 2.0 || m["deletions"] != 1.0 {
		t.Fatalf("main.go = %v", m)
	}
	h := m["hunks"].([]any)[0].(map[string]any)
	lines := h["lines"].([]any)
	if h["old_start"] != 2.0 || h["new_start"] != 2.0 || h["header"] != "package main" {
		t.Fatalf("hunk = %v", h)
	}
	del := lines[3].(map[string]any)
	add := lines[5].(map[string]any)
	if del["kind"] != "delete" || del["old"] != 5.0 || del["new"] != nil || del["text"] != "\tb()" ||
		add["kind"] != "add" || add["new"] != 6.0 || add["old"] != nil || add["text"] != "\tc()" {
		t.Fatalf("lines = %v", lines)
	}
	// before and after read the files as the run found and left them.
	ws := base + "/workspaces/" + r["workspace_id"].(string)
	before := r["before"].(map[string]any)["ref"].(string)
	if code, _, f := wsCall(t, "GET", ws+"/files?path=main.go&ref="+before, k, nil); code != 200 || f["text"] != old {
		t.Fatalf("before = %d %v", code, f)
	}
	// One file, whole.
	if code, r := call(t, "GET", base+"/runs/"+second+"/changes?path=new.txt", k, ""); code != 200 || len(r["files"].([]any)) != 1 {
		t.Fatalf("one file = %d %v", code, r)
	}

	// Runs carry their summary: the thread, the run, the run list.
	sum := map[string]any{"files": 4.0, "additions": 3.0, "deletions": 1.0}
	_, r = call(t, "GET", base+"/conversations/"+conv+"/thread", k, "")
	got := map[string]any{}
	for _, it := range r["items"].([]any) {
		if it := it.(map[string]any); it["kind"] == "run" {
			got[it["id"].(string)] = it["changes"]
		}
	}
	if !equalJSON(got[second], sum) || !equalJSON(got[idle], map[string]any{"files": 0.0, "additions": 0.0, "deletions": 0.0}) ||
		!equalJSON(got[first], map[string]any{"files": 3.0, "additions": 8.0, "deletions": 0.0}) {
		t.Fatalf("thread changes = %v", got)
	}
	if _, r := call(t, "GET", base+"/runs/"+second, k, ""); !equalJSON(r["run"].(map[string]any)["changes"], sum) {
		t.Fatalf("run changes = %v", r["run"])
	}
	if code, r := call(t, "GET", base+"/runs/"+idle+"/changes", k, ""); code != 200 || len(r["files"].([]any)) != 0 ||
		!equalJSON(r["before"], r["after"]) {
		t.Fatalf("an idle run = %d %v", code, r)
	}

	// A run of an agent without a workspace.
	id, err := v.s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: v.e.Current().Config.Agents["a"].SHA, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if code, r := call(t, "GET", base+"/runs/"+id+"/changes", k, ""); code != 404 || r["check"] != "no_workspace" {
		t.Fatalf("no workspace = %d %v", code, r)
	}
	if _, r := call(t, "GET", base+"/runs/"+id, k, ""); r["run"].(map[string]any)["changes"] != nil {
		t.Fatalf("no workspace changes = %v", r["run"])
	}
	if code, r := call(t, "GET", base+"/runs/00000000-0000-0000-0000-000000000000/changes", k, ""); code != 404 || r["check"] != "not_found" {
		t.Fatalf("no run = %d %v", code, r)
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
