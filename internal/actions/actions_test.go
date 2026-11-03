package actions_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/spec"
)

func deployment(apiURL string) spec.Deployment {
	return spec.Deployment{GitHub: &spec.GitHub{AppID: githubtest.AppID, PrivateKeyEnv: "ORBIT_GITHUB_APP_PRIVATE_KEY",
		APIURL: apiURL, Repositories: []string{"kensac/orbit"}}}
}

func TestMergeIsOfferedOnlyWhenConfiguredAndNamed(t *testing.T) {
	named := &spec.Agent{Actions: spec.ActionSet{actions.MergeType: spec.RuleApprove}}
	has := func(types []actions.Type) bool {
		for _, ty := range types {
			if ty.Name == actions.MergeType {
				return true
			}
		}
		return false
	}
	if !has(actions.Catalog(named, deployment("x"))) {
		t.Fatal("named and configured: offered")
	}
	if has(actions.Catalog(&spec.Agent{}, deployment("x"))) {
		t.Fatal("unnamed: denied and not offered")
	}
	if has(actions.Catalog(named, spec.Deployment{})) {
		t.Fatal("no github block: not offered")
	}
}

func TestMergeParamsAreValidated(t *testing.T) {
	named := &spec.Agent{Actions: spec.ActionSet{actions.MergeType: spec.RuleApprove}}
	var merge actions.Type
	for _, ty := range actions.Catalog(named, deployment("x")) {
		if ty.Name == actions.MergeType {
			merge = ty
		}
	}
	for params, want := range map[string]string{
		`{"repo":"kensac/other","pull_number":7,"head_sha":"` + githubtest.Head + `"}`:              "not in github.repositories",
		`{"repo":"kensac/orbit","pull_number":0,"head_sha":"` + githubtest.Head + `"}`:              "pull_number",
		`{"repo":"kensac/orbit","pull_number":7,"head_sha":"main"}`:                                 "full commit sha",
		`{"repo":"kensac/orbit","pull_number":7,"head_sha":"` + githubtest.Head + `","force":true}`: "unknown field",
	} {
		if err := merge.Validate(json.RawMessage(params)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", params, err, want)
		}
	}
	if err := merge.Validate(json.RawMessage(`{"repo":"kensac/orbit","pull_number":7,"head_sha":"` + githubtest.Head + `"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestMergeExecutorMergesAndRevokesItsToken(t *testing.T) {
	f := githubtest.New(t)
	env := func(n string) (string, bool) {
		if n == "ORBIT_GITHUB_APP_PRIVATE_KEY" {
			return githubtest.KeyPEM(), true
		}
		return "", false
	}
	ex := &actions.Executor{Deployment: deployment(f.URL()), Client: f.Srv.Client(), Env: env,
		MergeOptions: github.MergeOptions{MergeableWait: time.Millisecond}}
	params := json.RawMessage(`{"repo":"kensac/orbit","pull_number":7,"head_sha":"` + githubtest.Head + `"}`)
	res, err := ex.Execute(context.Background(), actions.Call{Agent: "a", RunID: "r", Type: actions.MergeType, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"merge_sha":"2222","merged":true,"pull_number":7,"repo":"kensac/orbit"}` {
		t.Fatalf("result = %s", res)
	}
	perms, _ := json.Marshal(f.TokenReq["permissions"])
	if string(perms) != `{"checks":"read","contents":"write","pull_requests":"write","statuses":"read"}` {
		t.Fatalf("merge token permissions = %s", perms)
	}
	if len(f.Revoked) != 1 {
		t.Fatal("the merge token must be revoked")
	}

	// A failing gate is the action's error, and the token is still revoked.
	f.Reset()
	f.CheckRuns[0]["conclusion"] = "failure"
	if _, err := ex.Execute(context.Background(), actions.Call{Agent: "a", RunID: "r", Type: actions.MergeType, Params: params}); err == nil || !strings.Contains(err.Error(), "required check concluded failure") {
		t.Fatalf("red check = %v", err)
	}
	if len(f.Revoked) != 2 {
		t.Fatal("revoked after a refusal too")
	}
}

func TestPagePublishIsChatOnlyAndRuleGated(t *testing.T) {
	has := func(a *spec.Agent) bool {
		for _, typ := range actions.Catalog(a, spec.Deployment{}) {
			if typ.Name == actions.PagePublishType {
				return true
			}
		}
		return false
	}
	if has(&spec.Agent{Chat: true}) || has(&spec.Agent{Chat: false, Actions: spec.ActionSet{actions.PagePublishType: spec.RuleApprove}}) || !has(&spec.Agent{Chat: true, Actions: spec.ActionSet{actions.PagePublishType: spec.RuleApprove}}) {
		t.Fatal("page:publish must be offered only to named chat agents")
	}
	typ := actions.Catalog(&spec.Agent{Chat: true, Actions: spec.ActionSet{actions.PagePublishType: spec.RuleApprove}}, spec.Deployment{})[0]
	for _, raw := range []string{
		`{"title":" spaced ","html":"<p>x</p>"}`,
		`{"title":"ok","html":""}`,
		`{"title":"ok","html":"<p>x</p>","url":"https://example.test"}`,
		`{"title":"ok","html":"<p>x</p>"} {}`,
	} {
		if err := typ.Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid params %s", raw)
		}
	}
	if err := typ.Validate(json.RawMessage(`{"title":"ok","html":"<main>safe payload</main>"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestPagePublishExecutorReturnsCallbackResult(t *testing.T) {
	want := json.RawMessage(`{"page_id":"id","title":"x"}`)
	ex := &actions.Executor{PagePublish: func(_ context.Context, c actions.Call, p actions.PagePublish) (json.RawMessage, error) {
		if c.ID != "id" || p.Title != "x" || p.HTML != "<p>x</p>" {
			t.Fatalf("call = %+v params=%+v", c, p)
		}
		return want, nil
	}}
	got, err := ex.Execute(context.Background(), actions.Call{ID: "id", Type: actions.PagePublishType, Params: json.RawMessage(`{"title":"x","html":"<p>x</p>"}`)})
	if err != nil || string(got) != string(want) {
		t.Fatalf("execute = %s, %v", got, err)
	}
}
