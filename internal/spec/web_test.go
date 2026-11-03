package spec

import (
	"reflect"
	"strings"
	"testing"
)

func TestWebAllowValidation(t *testing.T) {
	load := func(agent string) error {
		_, err := Load(write(t, map[string]string{"orbit.yaml": base + "sandbox: {sandboxd: \"sandboxd:8080\"}\n", "agents/a.yaml": "name: a\ncharter: x\n" + agent}))
		return err
	}
	for agent, want := range map[string]string{
		"tools: [WebFetch]\nweb: {allow: [10.0.0.1]}\n":                                "is an address",
		"tools: [WebFetch]\nweb: {allow: [\"[::1]\"]}\n":                               "is an address",
		"tools: [WebFetch]\nweb: {allow: [Example.com]}\n":                             "is not a lowercase domain",
		"tools: [WebFetch]\nweb: {allow: [\"*\"]}\n":                                   "is not a lowercase domain",
		"tools: [WebFetch]\nweb: {allow: [https://example.com/]}\n":                    "is not a lowercase domain",
		"tools: [WebFetch]\nweb: {allow: [nas.local]}\n":                               "is a private name",
		"tools: [WebFetch]\nweb: {allow: [\"*.tail1234.ts.net\"]}\n":                   "is a private name",
		"tools: [WebSearch]\nweb: {allow: [example.com]}\n":                            "allow needs WebFetch in tools",
		"tools: [WebFetch]\nweb: {allow: []}\n":                                        "allow names no domain",
		"chat: true\nsandbox: build\ntools: [WebFetch]\nweb: {allow: [example.com]}\n": "a sandboxed agent's network",
	} {
		if err := load(agent); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", agent, err, want)
		}
	}
	if err := load("tools: [WebFetch]\nweb: {allow: [example.com, \"*.wikipedia.org\", docs.python.org]}\n"); err != nil {
		t.Fatal(err)
	}
}

func TestWebWidening(t *testing.T) {
	bare := &Agent{Tools: []string{"WebFetch"}}
	two := &Agent{Tools: []string{"WebFetch"}, Web: &WebSpec{Allow: []string{"a.com", "b.com"}}}
	three := &Agent{Tools: []string{"WebFetch"}, Web: &WebSpec{Allow: []string{"a.com", "b.com", "c.com"}}}
	for _, c := range []struct {
		cur, next *Agent
		want      []string
	}{
		{bare, two, nil},
		{two, bare, []string{"web: any domain"}},
		{two, three, []string{"web domain c.com"}},
		{three, two, nil},
		{&Agent{}, two, []string{"tool WebFetch"}},
	} {
		if got := Widening(c.cur, c.next); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Widening(%v, %v) = %q, want %q", c.cur.Web, c.next.Web, got, c.want)
		}
	}
}
