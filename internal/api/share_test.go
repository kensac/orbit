package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/api"
)

// TestTemplatesAreSharedAndImportedGrantingNothing: a template is shared as a bundle and as a signed link anyone holding it can fetch (a tampered one is refused); an import of a bundle, given or fetched from a link, lowers every auto rule to approve and drops connectors, refuses hidden characters, and is a dry run when asked; the guarded fetch refuses a link to a private address.
func TestTemplatesAreSharedAndImportedGrantingNothing(t *testing.T) {
	var linkClient *http.Client
	v := newEnv(t, func(a *api.Server, _ *http.Server) { a.LinkClient = linkClient })
	tok := v.person("kanishk")
	u := v.srv.URL + "/api/v1/templates"
	if code, b := call(t, "POST", u, tok, newsTemplate); code != 201 {
		t.Fatalf("create = %d %v", code, b)
	}
	code, b := call(t, "POST", u+"/share", tok, `{"templates": [{"name": "news"}], "link": true, "expires_in_hours": 2}`)
	if code != 200 || b["bundle"].(map[string]any)["format"] != "orbit.templates/1" || len(b["bundle"].(map[string]any)["templates"].([]any)) != 1 {
		t.Fatalf("share = %d %v", code, b)
	}
	link := b["link"].(string)
	if !strings.HasPrefix(link, "http://orbit.test/api/v1/templates/shared/") || b["expires_at"] == nil {
		t.Fatalf("link = %v", b)
	}
	path := strings.TrimPrefix(link, "http://orbit.test")
	if code, b := call(t, "GET", v.srv.URL+path, "", ""); code != 200 || len(b["templates"].([]any)) != 1 {
		t.Fatalf("the link without a token = %d %v", code, b)
	}
	if code, _ := call(t, "GET", v.srv.URL+path[:len(path)-1]+"0", "", ""); code != 403 {
		t.Fatalf("a tampered link = %d", code)
	}
	if code, _ := call(t, "POST", u+"/share", tok, `{"templates": [{"name": "news"}], "link": true, "expires_in_hours": 1000}`); code != 400 {
		t.Fatalf("a link past a week = %d", code)
	}

	// Someone else's bundle: auto rules and a connector, which arrive as approve and not at all.
	foreign := `{"format": "orbit.templates/1", "templates": [{"name": "pay", "description": "Pays bills.", "parameters": [],
	  "spec": {"name": "payer", "charter": "Pay.", "actions": {"notify": "auto", "continue": "deny"}, "connectors": [{"name": "bank"}]}}]}`
	code, b = call(t, "POST", u+"/import", tok, `{"bundle": `+foreign+`, "dry_run": true}`)
	res := b["results"].([]any)[0].(map[string]any)
	if code != 200 || res["outcome"] != "created" || len(res["lowered"].([]any)) != 2 {
		t.Fatalf("dry run = %d %v", code, b)
	}
	if code, _ := call(t, "GET", u+"/pay", tok, ""); code != 404 {
		t.Fatalf("a dry run imported: %d", code)
	}
	if code, b = call(t, "POST", u+"/import", tok, `{"bundle": `+foreign+`, "note": "from a friend"}`); code != 200 {
		t.Fatalf("import = %d %v", code, b)
	}
	_, got := call(t, "GET", u+"/pay", tok, "")
	k := got["template"].(map[string]any)
	sp, _ := json.Marshal(k["spec"])
	if k["source"] != "import" || k["note"] != "from a friend" || strings.Contains(string(sp), "connectors") ||
		!strings.Contains(string(sp), `"notify":"approve"`) || !strings.Contains(string(sp), `"continue":"deny"`) {
		t.Fatalf("imported = %v", k)
	}
	if code, b := call(t, "POST", u+"/import", tok, `{"bundle": `+strings.Replace(foreign, "Pays bills.", `Pays \u202ebills.`, 1)+`}`); code != 400 || !strings.Contains(b["error"].(string), "hidden character U+202E") {
		t.Fatalf("a bidi control = %d %v", code, b)
	}

	// From another orbit's link: the guarded fetch refuses a local address; a client that reaches it imports.
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/templates/shared/") || r.Header.Get("Authorization") != "" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(strings.Replace(foreign, `"pay"`, `"pay-two"`, 1)))
	}))
	defer other.Close()
	otherLink := other.URL + "/api/v1/templates/shared/abc.def"
	if code, b := call(t, "POST", u+"/import", tok, `{"link": "`+otherLink+`"}`); code != 400 || !strings.Contains(b["error"].(string), "not a public address") {
		t.Fatalf("a local link = %d %v", code, b)
	}
	if code, b := call(t, "POST", u+"/import", tok, `{"link": "http://example.com/api/v1/templates/shared/x"}`); code != 400 || !strings.Contains(b["error"].(string), "https") {
		t.Fatalf("an http link = %d %v", code, b)
	}
	linkClient = other.Client()
	v2 := newEnv(t, func(a *api.Server, _ *http.Server) { a.LinkClient = linkClient })
	code, b = call(t, "POST", v2.srv.URL+"/api/v1/templates/import", v2.person("kanishk"), `{"link": "`+otherLink+`"}`)
	if code != 200 || b["results"].([]any)[0].(map[string]any)["name"] != "pay-two" {
		t.Fatalf("import from a link = %d %v", code, b)
	}
}
