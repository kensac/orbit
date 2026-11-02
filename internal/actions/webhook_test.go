package actions_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/spec"
)

func webhookOf(t *testing.T, d spec.Deployment, name string) actions.Type {
	t.Helper()
	a := &spec.Agent{Actions: spec.ActionSet{"webhook:" + name: spec.RuleAuto}}
	for _, ty := range actions.Catalog(a, d) {
		if ty.Name == "webhook:"+name {
			return ty
		}
	}
	t.Fatalf("webhook:%s not offered", name)
	return actions.Type{}
}

func TestWebhookSchemaShowsThePathParams(t *testing.T) {
	d := spec.Deployment{Endpoints: map[string]spec.Endpoint{"page": {URL: "http://x/api", Method: "PATCH", Path: "/pages/{n}/items/{slug}",
		Params: map[string]spec.EndpointParam{"n": {Type: "integer"}, "slug": {Type: "string", Enum: []string{"a", "b"}}}}}}
	ty := webhookOf(t, d, "page")
	b, _ := json.Marshal(ty.ParamsSchema)
	for _, want := range []string{`"required":["body","path"]`, `"n":{"minimum":0,"type":"integer"}`, `"enum":["a","b"]`, `"required":["n","slug"]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("schema %s lacks %s", b, want)
		}
	}
	if !strings.Contains(ty.Description, "PATCH /pages/{n}/items/{slug}") {
		t.Errorf("description = %q", ty.Description)
	}
	for params, want := range map[string]string{
		`{"path":{"n":3,"slug":"a"},"body":{}}`:           "",
		`{"path":{"n":-1,"slug":"a"},"body":{}}`:          "non-negative",
		`{"path":{"n":1.5,"slug":"a"},"body":{}}`:         "integer",
		`{"path":{"n":"3","slug":"a"},"body":{}}`:         "integer",
		`{"path":{"n":3,"slug":"c"},"body":{}}`:           "one of a, b",
		`{"path":{"n":3,"slug":"a","x":"y"},"body":{}}`:   "not a parameter",
		`{"path":{"n":3},"body":{}}`:                      "path.slug is required",
		`{"path":{"n":3,"slug":"a"},"body":[1]}`:          "JSON object",
		`{"path":{"n":3,"slug":"a"},"body":{},"url":"x"}`: "unknown field",
	} {
		err := ty.Validate(json.RawMessage(params))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", params, err, want)
		}
	}
	// An endpoint without a path takes no path params.
	plain := webhookOf(t, spec.Deployment{Endpoints: map[string]spec.Endpoint{"ha": {URL: "http://x/hook"}}}, "ha")
	if err := plain.Validate(json.RawMessage(`{"path":{"id":"1"},"body":{}}`)); err == nil {
		t.Fatal("a path param for an endpoint without a path")
	}
}

func TestWebhookEscapesValuesAndHoldsCredentialsToTheirPrefix(t *testing.T) {
	var hits atomic.Int32
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath, gotKey = r.URL.EscapedPath(), r.Header.Get("Authorization")
		if r.URL.Path == "/redirect/a" {
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := spec.Deployment{
		Credentials: spec.Credentials{"KEY": {URLPrefix: srv.URL + "/items/"}},
		Endpoints: map[string]spec.Endpoint{
			"items":    {URL: srv.URL, Path: "/items/{id}", Params: map[string]spec.EndpointParam{"id": {Type: "string", Pattern: `[a-z ]+`}}, Headers: map[string]string{"Authorization": "Bearer ${KEY}"}},
			"other":    {URL: srv.URL, Path: "/other/{id}", Params: map[string]spec.EndpointParam{"id": {Type: "string", Pattern: `[a-z]+`}}, Headers: map[string]string{"Authorization": "Bearer ${KEY}"}},
			"redirect": {URL: srv.URL, Path: "/redirect/{id}", Params: map[string]spec.EndpointParam{"id": {Type: "string", Pattern: `[a-z]+`}}},
		},
	}
	env := func(n string) (string, bool) { return "k-1", n == "KEY" }
	ex := &actions.Executor{Deployment: d, Client: srv.Client(), Env: env}
	run := func(typ, params string) (json.RawMessage, error) {
		return ex.Execute(context.Background(), actions.Call{Agent: "a", RunID: "r", Type: typ, Params: json.RawMessage(params)})
	}
	if _, err := run("webhook:items", `{"path":{"id":"a b"},"body":{}}`); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/items/a%20b" || gotKey != "Bearer k-1" {
		t.Fatalf("sent %s with %q", gotPath, gotKey)
	}
	// The credential is bound to /items/: an endpoint elsewhere never sends it.
	before := hits.Load()
	if _, err := run("webhook:other", `{"path":{"id":"a"},"body":{}}`); err == nil || !strings.Contains(err.Error(), "may be sent only to") || strings.Contains(err.Error(), "k-1") {
		t.Fatalf("other = %v", err)
	}
	if hits.Load() != before {
		t.Fatal("a request went out with a credential outside its prefix")
	}
	// A redirect is not followed: the write is not re-sent elsewhere.
	res, err := run("webhook:redirect", `{"path":{"id":"a"},"body":{}}`)
	if err == nil || !strings.Contains(string(res), `"status":307`) || gotPath != "/redirect/a" {
		t.Fatalf("redirect = %s %v, last path %s", res, err, gotPath)
	}
}
