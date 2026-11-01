package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/spec"
)

var ctx = context.Background()

const head = githubtest.Head

var testKey = githubtest.Key

type fakeGitHub = githubtest.Server

func newFake(t *testing.T) *fakeGitHub { return githubtest.New(t) }

func app(f *fakeGitHub) *github.App {
	return &github.App{ID: githubtest.AppID, Key: testKey, BaseURL: f.URL(), HTTP: f.Srv.Client(), Now: time.Now}
}

func TestParsePrivateKeyAcceptsGitHubsFormats(t *testing.T) {
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)}))
	der, _ := x509.MarshalPKCS8PrivateKey(testKey)
	pkcs8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for name, in := range map[string]string{
		"pkcs1": pkcs1, "pkcs8": pkcs8, "base64": base64.StdEncoding.EncodeToString([]byte(pkcs1)), "padded": "\n" + pkcs1 + "\n",
	} {
		k, err := github.ParsePrivateKey(in)
		if err != nil || !k.Equal(testKey) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := github.ParsePrivateKey("not a key"); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestNewAppReadsTheKeyFromTheEnvironment(t *testing.T) {
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)}))
	g := &spec.GitHub{AppID: 42, PrivateKeyEnv: "K"}
	if _, err := github.NewApp(g, func(string) (string, bool) { return "", false }, http.DefaultClient); err == nil || !strings.Contains(err.Error(), "K is not set") {
		t.Fatalf("missing key = %v", err)
	}
	a, err := github.NewApp(g, func(string) (string, bool) { return pemKey, true }, http.DefaultClient)
	if err != nil || a.BaseURL != github.DefaultAPIURL || a.ID != 42 {
		t.Fatalf("app = %+v %v", a, err)
	}
}

func TestTokenIsDownScopedToOneRepositoryAndTheAskedPermissions(t *testing.T) {
	f := newFake(t)
	tok, err := app(f).Token(ctx, "kensac/orbit", github.ReadContents)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "ghs_test" || tok.Repo != "kensac/orbit" {
		t.Fatalf("token = %+v", tok)
	}
	b, _ := json.Marshal(f.TokenReq)
	if string(b) != `{"permissions":{"contents":"read"},"repositories":["orbit"]}` {
		t.Fatalf("token request = %s", b)
	}
	if err := tok.Revoke(ctx); err != nil || len(f.Revoked) != 1 {
		t.Fatalf("revoke = %v %v", err, f.Revoked)
	}
	env := strings.Join(tok.GitAuthEnv(), "\n")
	if strings.Contains(env, "ghs_test") || !strings.Contains(env, base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_test"))) {
		t.Fatalf("git env = %s", env)
	}
}

func TestTokenRefusesAndRevokesAnOverScopedGrant(t *testing.T) {
	f := newFake(t)
	f.Granted = github.Permissions{"contents": "write"}
	if _, err := app(f).Token(ctx, "kensac/orbit", github.ReadContents); err == nil || !strings.Contains(err.Error(), "contents=write") {
		t.Fatalf("over-scoped = %v", err)
	}
	if len(f.Revoked) != 1 {
		t.Fatal("an over-scoped token must be revoked at once")
	}
	if _, err := app(f).Token(ctx, "kensac/orbit", github.Permissions{}); err == nil {
		t.Fatal("an empty permission set means everything; refused")
	}
	if _, err := app(f).Token(ctx, "../etc", github.ReadContents); err == nil {
		t.Fatal("bad repo accepted")
	}
}

func TestTokenWithAWrongKeyIsRefusedByGitHub(t *testing.T) {
	f := newFake(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	a := app(f)
	a.Key = other
	if _, err := a.Token(ctx, "kensac/orbit", github.ReadContents); !github.IsStatus(err, 401) {
		t.Fatalf("wrong key = %v", err)
	}
}

func run(name, status, conclusion string, app int64) github.CheckRun {
	r := github.CheckRun{Name: name, Status: status, Conclusion: conclusion}
	r.App.ID = app
	return r
}

func TestGate(t *testing.T) {
	ok := run("check", "completed", "success", 1)
	for _, tc := range []struct {
		name     string
		required []github.RequiredCheck
		runs     []github.CheckRun
		statuses []github.Status
		want     string // "" means green
	}{
		{"required green, other failing", []github.RequiredCheck{{Context: "check"}}, []github.CheckRun{ok, run("lint", "completed", "failure", 1)}, nil, ""},
		{"required pending", []github.RequiredCheck{{Context: "check"}}, []github.CheckRun{run("check", "in_progress", "", 1)}, nil, "required check is in_progress"},
		{"required failed", []github.RequiredCheck{{Context: "check"}}, []github.CheckRun{run("check", "completed", "failure", 1)}, nil, "required check concluded failure"},
		{"required skipped is not success", []github.RequiredCheck{{Context: "check"}}, []github.CheckRun{run("check", "completed", "skipped", 1)}, nil, "concluded skipped"},
		{"required missing", []github.RequiredCheck{{Context: "check"}}, nil, nil, "required check has not reported"},
		{"required from another app", []github.RequiredCheck{{Context: "check", AppID: 2}}, []github.CheckRun{ok}, nil, "has not reported"},
		{"required from the named app", []github.RequiredCheck{{Context: "check", AppID: 1}}, []github.CheckRun{ok}, nil, ""},
		{"required as a status", []github.RequiredCheck{{Context: "ci/legacy"}}, nil, []github.Status{{Context: "ci/legacy", State: "success"}}, ""},
		{"required status pending", []github.RequiredCheck{{Context: "ci/legacy"}}, nil, []github.Status{{Context: "ci/legacy", State: "pending"}}, "ci/legacy is pending"},
		{"unprotected, all green", nil, []github.CheckRun{ok}, []github.Status{{Context: "s", State: "success"}}, ""},
		{"unprotected, one failing", nil, []github.CheckRun{ok, run("lint", "completed", "failure", 1)}, nil, "lint concluded failure"},
		{"unprotected, a status failing", nil, []github.CheckRun{ok}, []github.Status{{Context: "s", State: "error"}}, "s is error"},
		{"unprotected, nothing ran", nil, nil, nil, "no checks ran"},
	} {
		err := github.Gate(tc.required, tc.runs, tc.statuses)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestScrubAttribution(t *testing.T) {
	in := "Pass --model to the claude-code runner.\r\n\r\nTested with make check.\n\n" +
		"\U0001F916 Generated with [Some Tool](https://example.com)\n\n" +
		"Generated by Claude\n" +
		"Co-Authored-By: Claude Opus <noreply@anthropic.com>\n" +
		"Co-authored-by: A Person <a@example.com>\n"
	want := "Pass --model to the claude-code runner.\n\nTested with make check.\n\nCo-authored-by: A Person <a@example.com>"
	if got := github.ScrubAttribution(in); got != want {
		t.Fatalf("scrubbed:\n%q\nwant:\n%q", got, want)
	}
}

func merge(f *fakeGitHub) (string, error) {
	tok, err := app(f).Token(ctx, "kensac/orbit", github.MergePermissions)
	if err != nil {
		return "", err
	}
	return github.Merge(ctx, tok.Client(), github.MergeRequest{Repo: "kensac/orbit", Number: 7, HeadSHA: head},
		github.MergeOptions{MergeableWait: time.Millisecond})
}

func TestMergeSquashesWithAScrubbedBodyPinnedToTheHead(t *testing.T) {
	f := newFake(t)
	f.Mergeable = []any{nil, nil, true}
	sha, err := merge(f)
	if err != nil {
		t.Fatal(err)
	}
	if sha != "2222" || f.PRReads != 3 {
		t.Fatalf("sha %s after %d reads", sha, f.PRReads)
	}
	if len(f.Patched) != 1 || f.Patched[0]["body"] != "What it does." {
		t.Fatalf("patched = %v", f.Patched)
	}
	if f.Merged["merge_method"] != "squash" || f.Merged["sha"] != head ||
		f.Merged["commit_message"] != "What it does." || f.Merged["commit_title"] != "Add a thing (#7)" {
		t.Fatalf("merge request = %v", f.Merged)
	}
}

func TestMergeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fakeGitHub)
		want  string
	}{
		{"head moved", func(f *fakeGitHub) {
			f.PR["head"].(map[string]any)["sha"] = "3333333333333333"
		}, "not the approved"},
		{"not an orbit branch", func(f *fakeGitHub) { f.PR["head"].(map[string]any)["ref"] = "feature" }, "not an orbit/ branch"},
		{"from a fork", func(f *fakeGitHub) {
			f.PR["head"].(map[string]any)["repo"] = map[string]any{"full_name": "someone/orbit"}
		}, "another repository"},
		{"not the default branch", func(f *fakeGitHub) { f.PR["base"].(map[string]any)["ref"] = "release" }, "not the default branch"},
		{"closed", func(f *fakeGitHub) { f.PR["state"] = "closed" }, "is closed"},
		{"draft", func(f *fakeGitHub) { f.PR["draft"] = true }, "draft"},
		{"conflicts", func(f *fakeGitHub) { f.Mergeable = []any{false}; f.PR["mergeable_state"] = "dirty" }, "not mergeable (dirty)"},
		{"still computing", func(f *fakeGitHub) { f.Mergeable = []any{nil} }, "has not finished computing"},
		{"blocked", func(f *fakeGitHub) { f.PR["mergeable_state"] = "blocked" }, "mergeable_state blocked"},
		{"required check pending", func(f *fakeGitHub) {
			f.CheckRuns[0]["status"], f.CheckRuns[0]["conclusion"] = "queued", nil
		}, "required check is queued"},
		{"ruleset check missing", func(f *fakeGitHub) {
			f.Rules = []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{
				"required_status_checks": []any{map[string]any{"context": "codeql"}}}}}
		}, "required codeql has not reported"},
		{"unprotected and a check failing", func(f *fakeGitHub) { f.Protection = nil }, "gitleaks concluded failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			tc.setup(f)
			_, err := merge(f)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if f.Merged != nil {
				t.Fatal("a refused merge must not reach the merge endpoint")
			}
		})
	}
}

func TestMergeNeedsTheCheckedSHA(t *testing.T) {
	f := newFake(t)
	tok, _ := app(f).Token(ctx, "kensac/orbit", github.MergePermissions)
	if _, err := github.Merge(ctx, tok.Client(), github.MergeRequest{Repo: "kensac/orbit", Number: 7}, github.MergeOptions{}); err == nil {
		t.Fatal("merge without head_sha")
	}
}
