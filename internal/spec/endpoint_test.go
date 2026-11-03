package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEndpointPathsAreValidatedAtLoad(t *testing.T) {
	for ep, want := range map[string]string{
		`{url: "http://x/api", path: "/t/{id}", params: {id: {type: string, pattern: "[0-9]+"}}}`:          "",
		`{url: "http://x/api", path: "/t/{id}", params: {id: {type: integer}}}`:                            "",
		`{url: "http://x/api", path: "/t/{id}"}`:                                                           "{id} has no entry in params",
		`{url: "http://x/api", path: "/t", params: {id: {type: integer}}}`:                                 "params.id is not a placeholder",
		`{url: "http://x/api", path: "/t/{id}", params: {id: {type: string}}}`:                             "a string needs a pattern or an enum",
		`{url: "http://x/api", path: "/t/{id}", params: {id: {type: string, pattern: "("}}}`:               "pattern:",
		`{url: "http://x/api", path: "/t/{id}", params: {id: {type: integer, pattern: "1"}}}`:              "an integer takes no pattern",
		`{url: "http://x/api", path: "/t/{id}", params: {id: {type: boolean}}}`:                            "type must be string or integer",
		`{url: "http://x/api", path: "t/{id}", params: {id: {type: integer}}}`:                             "must start with /",
		`{url: "http://x/api", path: "/t/{id}?x=1", params: {id: {type: integer}}}`:                        "no query",
		`{url: "http://x/api", path: "/../{id}", params: {id: {type: integer}}}`:                           "dot segment",
		`{url: "http://x/api", path: "/t/{ID}"}`:                                                           "a placeholder is {name}",
		`{url: "http://x/api?k=1", path: "/t"}`:                                                            "no query or fragment when path is set",
		`{url: "/relative"}`:                                                                               "absolute http(s) URL",
		`{url: "http://x/api", body_schema: {type: 3}}`:                                                    "body_schema:",
		`{url: "http://x/api", path: "/t/{id}", params: {id: {type: string, enum: [a], max_length: 999}}}`: "max_length is 1 to 200",
	} {
		_, err := Load(write(t, map[string]string{"orbit.yaml": base + "endpoints:\n  e: " + ep + "\n"}))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", ep, err, want)
		}
	}
}

func TestEndpointCredentialsAreBoundToTheirPrefix(t *testing.T) {
	const creds = "credentials:\n  KEY: {url_prefix: \"https://sure.example.com/api/\"}\n"
	for ep, want := range map[string]string{
		// The whole URL is known: a mismatch fails the load.
		`{url: "https://sure.example.com/hook", headers: {K: "${KEY}"}}`:     "credential KEY may be sent only to https://sure.example.com/api/",
		`{url: "https://sure.example.com/api/v1/x", headers: {K: "${KEY}"}}`: "",
		// With placeholders the URL is checked on every send (actions.Executor).
		`{url: "https://sure.example.com", path: "/{p}", params: {p: {type: string, enum: [api]}}, headers: {K: "${KEY}"}}`: "",
	} {
		_, err := Load(write(t, map[string]string{"orbit.yaml": base + creds + "endpoints:\n  e: " + ep + "\n"}))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", ep, err, want)
		}
	}
}

func TestRenderURLEscapesEachValueAsOneSegment(t *testing.T) {
	ep := Endpoint{URL: "http://x/api/", Path: "/t/{id}/n/{n}", Params: map[string]EndpointParam{
		"id": {Type: "string", Pattern: `[^ ]+`}, "n": {Type: "integer"}}}
	u, err := ep.RenderURL(map[string]json.RawMessage{"id": json.RawMessage(`"a/b?c"`), "n": json.RawMessage(`7`)})
	if err != nil || u != "http://x/api/t/a%2Fb%3Fc/n/7" {
		t.Fatalf("url = %q %v", u, err)
	}
	if _, err := ep.RenderURL(map[string]json.RawMessage{"id": json.RawMessage(`".."`), "n": json.RawMessage(`7`)}); err == nil || !strings.Contains(err.Error(), "dot segment") {
		t.Fatalf("dot segment = %v", err)
	}
}
