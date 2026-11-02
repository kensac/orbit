package api_test

import (
	"strconv"
	"strings"
	"testing"
)

func TestMemoryEntriesAPI(t *testing.T) {
	v := newEnv(t, nil)
	pub, tok := v.srv.URL+"/api/v1", v.person("kanishk")

	code, out := call(t, "POST", pub+"/memory/entries", tok, `{"scope":"topic","topic":"finance","type":"feedback",
		"title":"Transfers are not spending","body":"Moving money between my accounts is not spending.",
		"how_to_apply":"Exclude transfers from totals.","author":"someone-else"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, out)
	}
	e := out["entry"].(map[string]any)
	id := e["id"].(string)
	if e["author"] != "kanishk" || e["topic"] != "finance" || e["key"] != "transfers-are-not-spending" || e["version"].(float64) != 1 {
		t.Fatalf("entry = %v", e)
	}
	if code, out := call(t, "POST", pub+"/memory/entries", tok, `{"scope":"agent","agent":"nobody","type":"user","title":"t","body":"b"}`); code != 400 {
		t.Fatalf("entry for an unknown agent = %d %v", code, out)
	}
	if code, _ := call(t, "POST", pub+"/memory/entries", tok, `{"scope":"profile","type":"user","title":"t","body":"b","extra":1}`); code != 400 {
		t.Fatalf("unknown field = %d", code)
	}

	if code, out := call(t, "PATCH", pub+"/memory/entries/"+id, tok, `{"version":1,"description":"Transfers excluded"}`); code != 200 ||
		out["entry"].(map[string]any)["version"].(float64) != 2 {
		t.Fatalf("patch = %d %v", code, out)
	}
	if code, _ := call(t, "PATCH", pub+"/memory/entries/"+id, tok, `{"version":1,"body":"stale"}`); code != 409 {
		t.Fatalf("stale patch = %d, want 409", code)
	}
	code, out = call(t, "GET", pub+"/memory/entries/"+id, tok, "")
	if code != 200 || len(out["versions"].([]any)) != 2 {
		t.Fatalf("get = %d %v", code, out)
	}

	if code, out := call(t, "GET", pub+"/memory/entries?topic=finance&q=TRANSFER", tok, ""); code != 200 || len(out["entries"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	if code, _ := call(t, "GET", pub+"/memory/entries?topic=finance&agent=a", tok, ""); code != 400 {
		t.Fatalf("topic and agent = %d", code)
	}

	// The agent does not read finance until it is attached.
	if code, out := call(t, "GET", pub+"/agents/a/context", tok, ""); code != 200 || out["entries"].(float64) != 0 {
		t.Fatalf("context before attach = %d %v", code, out)
	}
	if code, out := call(t, "PUT", pub+"/agents/a/topics/finance", tok, ""); code != 200 || out["created"] != true {
		t.Fatalf("attach = %d %v", code, out)
	}
	code, out = call(t, "GET", pub+"/agents/a/context", tok, "")
	if code != 200 || out["entries"].(float64) != 1 || !strings.Contains(out["index"].(string), "Transfers are not spending") {
		t.Fatalf("context = %d %v", code, out)
	}
	if code, out := call(t, "GET", pub+"/memory/topics", tok, ""); code != 200 || out["topics"].([]any)[0].(map[string]any)["entries"].(float64) != 1 {
		t.Fatalf("topics = %d %v", code, out)
	}
	if code, _ := call(t, "DELETE", pub+"/agents/a/topics/finance", tok, ""); code != 200 {
		t.Fatalf("detach = %d", code)
	}
	if code, _ := call(t, "DELETE", pub+"/agents/a/topics/finance", tok, ""); code != 404 {
		t.Fatalf("detach again = %d", code)
	}

	if code, out := call(t, "POST", pub+"/memory/entries/"+id+"/retire", tok, `{"note":"no longer true"}`); code != 200 ||
		out["entry"].(map[string]any)["status"] != "retired" {
		t.Fatalf("retire = %d %v", code, out)
	}
	if _, out := call(t, "GET", pub+"/memory/entries?topic=finance", tok, ""); len(out["entries"].([]any)) != 0 {
		t.Fatalf("a retired entry is listed as active: %v", out)
	}
}

func TestMemoryImportAPI(t *testing.T) {
	v := newEnv(t, nil)
	body := `{"dry_run":true,"entries":[{"scope":"profile","key":"who","type":"user","title":"Who","body":"A person."},
		{"scope":"topic","topic":"homelab","key":"kener","type":"feedback","title":"Kener stays","body":"Keep it."}]}`
	code, out := call(t, "POST", v.internal.URL+"/api/v1/memory/import", token, body)
	if code != 200 || out["dry_run"] != true || len(out["results"].([]any)) != 2 {
		t.Fatalf("dry run = %d %v", code, out)
	}
	if _, out := call(t, "GET", v.internal.URL+"/api/v1/memory/entries?status=all", token, ""); len(out["entries"].([]any)) != 0 {
		t.Fatal("a dry run wrote entries")
	}
	applied := strings.Replace(body, `"dry_run":true`, `"dry_run":false`, 1)
	if code, out := call(t, "POST", v.internal.URL+"/api/v1/memory/import", token, applied); code != 200 ||
		out["results"].([]any)[1].(map[string]any)["outcome"] != "created" {
		t.Fatalf("import = %d %v", code, out)
	}
	_, out = call(t, "POST", v.internal.URL+"/api/v1/memory/import", token, applied)
	if out["results"].([]any)[0].(map[string]any)["outcome"] != "unchanged" {
		t.Fatalf("re-import = %v", out)
	}
	_, out = call(t, "GET", v.internal.URL+"/api/v1/memory/entries?scope=topic", token, "")
	if e := out["entries"].([]any)[0].(map[string]any); e["source"] != "import" {
		t.Fatalf("imported entry = %v", e)
	}
}

func TestMemoryWriteAndRestoreAPI(t *testing.T) {
	v := newEnv(t, nil)
	pub, tok := v.srv.URL+"/api/v1", v.person("kanishk")
	code, out := call(t, "POST", pub+"/agents/a/memory", tok, `{"content":"first"}`)
	if code != 201 || out["version"].(map[string]any)["author"] != "kanishk" {
		t.Fatalf("write = %d %v", code, out)
	}
	first := out["version"].(map[string]any)["id"].(float64)
	if code, _ := call(t, "POST", pub+"/agents/a/memory", tok, `{"content":"second"}`); code != 201 {
		t.Fatalf("second write = %d", code)
	}
	code, out = call(t, "POST", pub+"/agents/a/memory", tok, `{"restore":`+strconv.Itoa(int(first))+`}`)
	if code != 201 || out["version"].(map[string]any)["content"] != "first" || out["version"].(map[string]any)["restored_from"].(float64) != first {
		t.Fatalf("restore = %d %v", code, out)
	}
	if code, _ := call(t, "POST", pub+"/agents/a/memory", tok, `{"content":"x","restore":1}`); code != 400 {
		t.Fatalf("both = %d", code)
	}
	if code, _ := call(t, "POST", pub+"/agents/a/memory", tok, `{"restore":999999}`); code != 404 {
		t.Fatalf("unknown version = %d", code)
	}
	if _, out := call(t, "GET", pub+"/agents/a", tok, ""); out["memory"] != "first" {
		t.Fatalf("current memory = %v", out["memory"])
	}
	if _, out := call(t, "GET", pub+"/agents/a/memory", tok, ""); len(out["versions"].([]any)) != 3 {
		t.Fatalf("versions = %v", out)
	}

	_, out = call(t, "POST", pub+"/memory/entries", tok, `{"scope":"profile","type":"user","title":"City","body":"Boston."}`)
	id := out["entry"].(map[string]any)["id"].(string)
	call(t, "PATCH", pub+"/memory/entries/"+id, tok, `{"body":"New York."}`)
	code, out = call(t, "POST", pub+"/memory/entries/"+id+"/restore", tok, `{"restore":1,"version":2}`)
	if code != 200 || out["entry"].(map[string]any)["body"] != "Boston." || out["entry"].(map[string]any)["version"].(float64) != 3 {
		t.Fatalf("restore entry = %d %v", code, out)
	}
	if code, _ := call(t, "POST", pub+"/memory/entries/"+id+"/restore", tok, `{"restore":3}`); code != 400 {
		t.Fatalf("restore of the current version = %d", code)
	}
}

func TestLearnNowAPI(t *testing.T) {
	v := newEnv(t, nil)
	pub, tok := v.srv.URL+"/api/v1", v.person("kanishk")
	_, out := call(t, "POST", pub+"/agents/a/conversations", tok, `{"body":"I never count transfers.","wake":false}`)
	conv := out["conversation_id"].(string)
	code, out := call(t, "POST", pub+"/conversations/"+conv+"/learn", tok, "")
	if code != 202 || out["run_id"] == "" {
		t.Fatalf("learn = %d %v", code, out)
	}
	if code, _ := call(t, "POST", pub+"/conversations/"+conv+"/learn", tok, ""); code != 409 {
		t.Fatalf("learn again = %d", code)
	}
	_, out = call(t, "GET", pub+"/agents/a/conversations?archived=true", tok, "")
	if cs := out["conversations"].([]any); len(cs) != 1 || cs[0].(map[string]any)["purpose"] != "learning" {
		t.Fatalf("archived = %v", out)
	}
}
