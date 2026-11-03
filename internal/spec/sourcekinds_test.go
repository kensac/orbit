package spec

import (
	"strings"
	"testing"
)

func TestGitHubAndLokiSourcesAreValidatedAtLoad(t *testing.T) {
	const gh = base + "github: {app_id: 1, private_key_env: K, repositories: [kensac/orbit], read_repositories: [kensac/orbit]}\n"
	for src, want := range map[string]string{
		`{name: s, kind: github, repo: kensac/orbit, resource: issues, state: closed, limit: 50}`:       "",
		`{name: s, kind: github, repo: kensac/orbit, resource: checks, ref: main}`:                      "",
		`{name: s, kind: github, repo: kensac/orbit, resource: releases}`:                               "",
		`{name: s, kind: github, repo: kensac/other, resource: issues}`:                                 "kensac/other is not in github.read_repositories",
		`{name: s, kind: github, repo: kensac/orbit, resource: commits}`:                                `resource "commits" is not one of`,
		`{name: s, kind: github, repo: kensac/orbit, resource: checks}`:                                 "checks needs ref",
		`{name: s, kind: github, repo: kensac/orbit, resource: issues, ref: main}`:                      "issues takes no ref",
		`{name: s, kind: github, repo: kensac/orbit, resource: issues, state: merged}`:                  "state must be open, closed or all",
		`{name: s, kind: github, repo: kensac/orbit, resource: pulls, limit: 301}`:                      "limit is at most 1000 (300 for pulls)",
		`{name: s, kind: github, repo: kensac/orbit, resource: issues, url: "https://api.github.com/"}`: "not url, headers",
		`{name: s, kind: loki, url: "http://loki:3100", expr: '{a="b"}', since: 2h, limit: 100}`:        "",
		`{name: s, kind: loki, url: "http://loki:3100"}`:                                                "loki needs url and expr",
		`{name: s, kind: loki, url: "http://loki:3100", expr: '{a="b"}', since: 25h}`:                   "since is at most 24h",
		`{name: s, kind: loki, url: "http://loki:3100", expr: '{a="b"}', limit: 5001}`:                  "limit is at most 5000",
		`{name: s, kind: http, url: "http://x/", limit: 3}`:                                             "limit is for a github or loki source",
		`{name: s, kind: http, url: "http://x/", since: 1h}`:                                            "since is for a loki source",
		`{name: s, kind: prometheus, url: "http://x/", expr: up, repo: kensac/orbit}`:                   "repo, resource, ref and state are for a github source",
	} {
		_, err := Load(write(t, map[string]string{"orbit.yaml": gh, "agents/a.yaml": "name: a\ncharter: x\nneeds: [github]\nsources: [" + src + "]\n"}))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", src, err, want)
		}
	}
	_, err := Load(write(t, map[string]string{"orbit.yaml": gh, "agents/a.yaml": "name: a\ncharter: x\nsources: [{name: s, kind: github, repo: kensac/orbit, resource: issues}]\n"}))
	if err == nil || !strings.Contains(err.Error(), `needs "github" in needs`) {
		t.Fatalf("err = %v", err)
	}
	// A database agent may read GitHub too, within read_repositories, and Loki through a credential only.
	if err := loadDB(t, `{"name":"d","charter":"x","needs":["github"],"sources":[{"name":"s","kind":"loki","url":"http://loki:3100","expr":"{a=\"b\"}","headers":{"X":"${ORBIT_API_TOKEN}"}}]}`, nil); err == nil || !strings.Contains(err.Error(), "not in orbit.yaml's credentials") {
		t.Fatalf("err = %v", err)
	}
}
