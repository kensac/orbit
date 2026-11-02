package api_test

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestSkillsAPI(t *testing.T) {
	v := newEnv(t, nil)
	pub, tok := v.srv.URL+"/api/v1", v.person("kanishk")
	md := `---\nname: close\ndescription: Close a month.\n---\nSteps.\n`
	file := base64.StdEncoding.EncodeToString([]byte("A"))
	code, out := call(t, "POST", pub+"/skills", tok, `{"name":"close","content":"`+md+`","files":[{"path":"references/a.md","content":"`+file+`"}]}`)
	if code != 201 || out["skill"].(map[string]any)["author"] != "kanishk" {
		t.Fatalf("create = %d %v", code, out)
	}
	if code, _ := call(t, "POST", pub+"/skills", tok, `{"name":"close","content":"`+md+`"}`); code != 409 {
		t.Fatalf("create twice = %d", code)
	}
	if code, _ := call(t, "POST", pub+"/skills", tok, `{"name":"bad","content":"no frontmatter"}`); code != 400 {
		t.Fatalf("invalid = %d", code)
	}
	code, out = call(t, "PUT", pub+"/skills/close", tok, `{"version":1,"content":"`+strings.Replace(md, "Steps.", "Better steps.", 1)+`"}`)
	if code != 200 || out["changed"] != true || out["skill"].(map[string]any)["version"].(float64) != 2 {
		t.Fatalf("put = %d %v", code, out)
	}
	code, out = call(t, "GET", pub+"/skills/close/versions/1", tok, "")
	if code != 200 || out["skill"].(map[string]any)["files"].([]any)[0].(map[string]any)["content"] != file {
		t.Fatalf("version 1 = %d %v", code, out)
	}
	if code, _ := call(t, "PUT", pub+"/agents/a/skills/nope", tok, ""); code != 404 {
		t.Fatalf("attach unknown = %d", code)
	}
	if code, _ := call(t, "PUT", pub+"/agents/a/skills/close", tok, ""); code != 200 {
		t.Fatalf("attach = %d", code)
	}
	_, out = call(t, "GET", pub+"/skills", tok, "")
	if l := out["skills"].([]any); len(l) != 1 || l[0].(map[string]any)["agents"].([]any)[0] != "a" {
		t.Fatalf("list = %v", out)
	}
	_, out = call(t, "GET", pub+"/agents/a/context", tok, "")
	if sk := out["skills"].([]any); len(sk) != 1 || sk[0].(map[string]any)["version"].(float64) != 2 {
		t.Fatalf("context = %v", out)
	}
	if code, out := call(t, "POST", pub+"/skills/close/restore", tok, `{"restore":1}`); code != 200 || out["skill"].(map[string]any)["version"].(float64) != 3 {
		t.Fatalf("restore = %d %v", code, out)
	}
	if code, out := call(t, "POST", pub+"/skills/close/retire", tok, ""); code != 200 || out["skill"].(map[string]any)["status"] != "retired" {
		t.Fatalf("retire = %d %v", code, out)
	}
	code, out = call(t, "POST", pub+"/skills/import", tok, `{"dry_run":true,"skills":[{"name":"close","content":"`+md+`"},{"name":"other","content":"---\nname: other\ndescription: o\n---\nx"}]}`)
	if code != 200 || out["results"].([]any)[1].(map[string]any)["outcome"] != "created" {
		t.Fatalf("import = %d %v", code, out)
	}
}
