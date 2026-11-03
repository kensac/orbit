package actions_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/spec"
)

func commentDeployment(apiURL string) spec.Deployment {
	d := deployment(apiURL)
	d.GitHub.CommentRepositories = []string{"kensac/orbit"}
	return d
}

func TestCommentIsOfferedOnlyWithCommentRepositories(t *testing.T) {
	named := &spec.Agent{Actions: spec.ActionSet{actions.CommentType: spec.RuleApprove}}
	offered := func(d spec.Deployment) bool {
		for _, ty := range actions.Catalog(named, d) {
			if ty.Name == actions.CommentType {
				return true
			}
		}
		return false
	}
	if !offered(commentDeployment("x")) || offered(deployment("x")) || offered(spec.Deployment{}) {
		t.Fatal("offered only with github.comment_repositories")
	}
	var ty actions.Type
	for _, c := range actions.Catalog(named, commentDeployment("x")) {
		if c.Name == actions.CommentType {
			ty = c
		}
	}
	for params, want := range map[string]string{
		`{"repo":"kensac/other","number":3,"body":"hi"}`:                                 "not in github.comment_repositories",
		`{"repo":"kensac/orbit","number":0,"body":"hi"}`:                                 "number must be positive",
		`{"repo":"kensac/orbit","number":3,"body":"  "}`:                                 "body must be",
		`{"repo":"kensac/orbit","number":3,"body":"hi","x":1}`:                           "unknown field",
		`{"repo":"kensac/orbit","number":3,"body":"` + strings.Repeat("a", 20001) + `"}`: "body must be",
	} {
		if err := ty.Validate(json.RawMessage(params)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.60s: %v, want %q", params, err, want)
		}
	}
}

func TestCommentPostsWithAnIssuesWriteTokenAndRevokesIt(t *testing.T) {
	f := githubtest.New(t)
	env := func(n string) (string, bool) { return githubtest.KeyPEM(), n == "ORBIT_GITHUB_APP_PRIVATE_KEY" }
	ex := &actions.Executor{Deployment: commentDeployment(f.URL()), Client: f.Srv.Client(), Env: env}
	params := json.RawMessage(`{"repo":"kensac/orbit","number":12,"body":"The deploy failed at the canary.\n\nGenerated with Claude Code\n"}`)
	res, err := ex.Execute(context.Background(), actions.Call{Agent: "ops", RunID: "r", Type: actions.CommentType, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"comment_id":1001,"number":12,"repo":"kensac/orbit","url":"https://github.com/kensac/orbit/issues/12#issuecomment-1001"}` {
		t.Fatalf("result = %s", res)
	}
	perms, _ := json.Marshal(f.TokenReq["permissions"])
	if string(perms) != `{"issues":"write"}` || len(f.Revoked) != 1 {
		t.Fatalf("token %s, revoked %d", perms, len(f.Revoked))
	}
	if len(f.Comments) != 1 || f.Comments[0]["body"] != "The deploy failed at the canary." || f.Comments[0]["number"] != 12 {
		t.Fatalf("comments = %v", f.Comments)
	}
	// A repository outside the list is refused at execution too, with no token minted.
	ex.Deployment.GitHub.CommentRepositories = []string{"kensac/elsewhere"}
	f.TokenReq = nil
	if _, err := ex.Execute(context.Background(), actions.Call{Agent: "ops", RunID: "r", Type: actions.CommentType, Params: params}); err == nil || f.TokenReq != nil {
		t.Fatalf("err = %v, token %v", err, f.TokenReq)
	}
}
