package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/spec"
)

func TestLokiQueriesARangeEndingNowAndDropsStats(t *testing.T) {
	var got url.Values
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		got, key = r.URL.Query(), r.Header.Get("X-Scope-OrgID")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"container":"orbit"},"values":[["1","boom"]]}],"stats":{"summary":{"bytesProcessedPerSecond":1}}}}`)
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	f := &Fetcher{Client: srv.Client(), Env: func(n string) (string, bool) { return "tenant", n == "TENANT" }, Now: func() time.Time { return now },
		Credentials: spec.Credentials{"TENANT": {URLPrefix: srv.URL + "/loki/"}}}
	res, err := f.Fetch(context.Background(), spec.Source{Name: "logs", Kind: "loki", URL: srv.URL + "/", Expr: `{container="orbit"} |= "error"`,
		Headers: map[string]string{"X-Scope-OrgID": "${TENANT}"}, Since: spec.Duration(30 * time.Minute), MaxBytes: 1 << 20}, Vars{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Content) != `{"result":[{"stream":{"container":"orbit"},"values":[["1","boom"]]}],"resultType":"streams"}` {
		t.Fatalf("content = %s", res.Content)
	}
	if got.Get("query") != `{container="orbit"} |= "error"` || got.Get("limit") != "1000" || got.Get("direction") != "backward" ||
		got.Get("end") != fmt.Sprint(now.UnixNano()) || got.Get("start") != fmt.Sprint(now.Add(-30*time.Minute).UnixNano()) || key != "tenant" {
		t.Fatalf("query = %v, key %q", got, key)
	}
	// A credential bound elsewhere is never sent to Loki.
	f.Credentials = spec.Credentials{"TENANT": {URLPrefix: "https://elsewhere.example/"}}
	if _, err := f.Fetch(context.Background(), spec.Source{Name: "logs", Kind: "loki", URL: srv.URL, Expr: `{a="b"}`,
		Headers: map[string]string{"X-Scope-OrgID": "${TENANT}"}, MaxBytes: 1 << 20}, Vars{Now: now}); err == nil || !strings.Contains(err.Error(), "may be sent only to") {
		t.Fatalf("err = %v", err)
	}
	// A query Loki refuses fails the source with Loki's reason.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "parse error at line 1, col 2: syntax error")
	}))
	defer bad.Close()
	f = &Fetcher{Client: bad.Client(), Env: noEnv}
	if _, err := f.Fetch(context.Background(), spec.Source{Name: "logs", Kind: "loki", URL: bad.URL, Expr: `{`, MaxBytes: 1 << 20}, Vars{Now: now}); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("err = %v", err)
	}
}

func TestGitHubSourceReadsWithAPerFetchTokenOfOneResource(t *testing.T) {
	f := githubtest.New(t)
	env := func(n string) (string, bool) { return githubtest.KeyPEM(), n == "ORBIT_GITHUB_APP_PRIVATE_KEY" }
	g := &spec.GitHub{AppID: githubtest.AppID, PrivateKeyEnv: "ORBIT_GITHUB_APP_PRIVATE_KEY", APIURL: f.URL(), Repositories: []string{"kensac/orbit"}}
	app, err := github.NewApp(g, env, f.Srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	var minted []string
	fe := &Fetcher{Client: f.Srv.Client(), Env: env, GitHub: func(ctx context.Context, repo string, perms github.Permissions) (*github.Token, error) {
		b, _ := json.Marshal(perms)
		minted = append(minted, repo+" "+string(b))
		return app.Token(ctx, repo, perms)
	}}
	long := strings.Repeat("x", 5000)
	f.Issues = []map[string]any{
		{"number": 3, "title": "Bug", "state": "open", "user": map[string]any{"login": "kanishk"}, "labels": []any{map[string]any{"name": "bug"}},
			"comments": 2, "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-02T00:00:00Z", "html_url": "https://github.com/kensac/orbit/issues/3", "body": long},
		{"number": 4, "title": "A PR", "state": "open", "pull_request": map[string]any{"url": "x"}, "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-02T00:00:00Z"},
	}
	res, err := fe.Fetch(context.Background(), spec.Source{Name: "issues", Kind: "github", Repo: "kensac/orbit", Resource: "issues", MaxBytes: 1 << 20}, Vars{})
	if err != nil {
		t.Fatal(err)
	}
	var issues []map[string]any
	_ = json.Unmarshal(res.Content, &issues)
	if len(issues) != 1 || issues[0]["number"] != 3.0 || issues[0]["body_truncated"] != true || len(issues[0]["body"].(string)) != 4000 ||
		issues[0]["labels"].([]any)[0] != "bug" || issues[0]["author"] != "kanishk" {
		t.Fatalf("issues = %s", res.Content)
	}
	if len(f.Revoked) != 1 {
		t.Fatal("the token is revoked after the fetch")
	}

	res, err = fe.Fetch(context.Background(), spec.Source{Name: "checks", Kind: "github", Repo: "kensac/orbit", Resource: "checks", Ref: githubtest.Head, MaxBytes: 1 << 20}, Vars{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Content), `{"conclusion":"failure","name":"gitleaks","status":"completed","url":""}`) {
		t.Fatalf("checks = %s", res.Content)
	}
	f.Releases = []map[string]any{{"tag_name": "v1", "name": "One", "author": map[string]any{"login": "bot"}, "created_at": "2026-11-01T00:00:00Z", "body": "notes"}}
	res, err = fe.Fetch(context.Background(), spec.Source{Name: "rel", Kind: "github", Repo: "kensac/orbit", Resource: "releases", Limit: 5, MaxBytes: 1 << 20}, Vars{})
	if err != nil || !strings.Contains(string(res.Content), `"tag_name":"v1"`) {
		t.Fatalf("releases = %s %v", res.Content, err)
	}
	if _, err := fe.Fetch(context.Background(), spec.Source{Name: "prs", Kind: "github", Repo: "kensac/orbit", Resource: "pulls", MaxBytes: 1 << 20}, Vars{}); err != nil {
		t.Fatal(err)
	}
	want := []string{`kensac/orbit {"issues":"read"}`, `kensac/orbit {"checks":"read","statuses":"read"}`, `kensac/orbit {"contents":"read"}`, `kensac/orbit {"pull_requests":"read"}`}
	if strings.Join(minted, "|") != strings.Join(want, "|") || len(f.Revoked) != 4 {
		t.Fatalf("minted %v, revoked %d", minted, len(f.Revoked))
	}
	// Too much for max_bytes fails, still revoking.
	if _, err := fe.Fetch(context.Background(), spec.Source{Name: "issues", Kind: "github", Repo: "kensac/orbit", Resource: "issues", MaxBytes: 100}, Vars{}); err == nil || !strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("err = %v", err)
	}
	if len(f.Revoked) != 5 {
		t.Fatal("revoked after a refusal too")
	}
}
