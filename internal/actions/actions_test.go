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

func TestDefineOffersOnlyConfiguredConnectorNames(t *testing.T) {
	a := &spec.Agent{Name: "orbit", Actions: spec.ActionSet{actions.DefineType: spec.RuleApprove}}
	d := spec.Deployment{Connectors: map[string]spec.Connector{
		"sheets":  {URL: "https://sheets.example/mcp", Description: "Private endpoint description."},
		"housing": {URL: "https://housing.example/mcp", Auth: spec.ConnectorAuth{Value: "secret"}},
	}}
	var define actions.Type
	for _, typ := range actions.Catalog(a, d) {
		if typ.Name == actions.DefineType {
			define = typ
			break
		}
	}
	if define.Name == "" {
		t.Fatal("agent:define was not offered")
	}
	for _, want := range []string{"housing", "sheets", "Adding a connector", "approval", "does not add a server or sign in"} {
		if !strings.Contains(define.Description, want) {
			t.Fatalf("description lacks %q: %s", want, define.Description)
		}
	}
	for _, secret := range []string{"https://", "Private endpoint", "secret"} {
		if strings.Contains(define.Description, secret) {
			t.Fatalf("description contains connector configuration %q: %s", secret, define.Description)
		}
	}
	schema := define.ParamsSchema["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
	connector := schema["connectors"].(map[string]any)
	items := connector["items"].(map[string]any)
	if got := items["enum"].([]string); strings.Join(got, ",") != "housing,sheets" {
		t.Fatalf("connector enum = %v", got)
	}

	noConnectors := &spec.Agent{Name: "orbit", Actions: spec.ActionSet{actions.DefineType: spec.RuleApprove}}
	found := false
	for _, typ := range actions.Catalog(noConnectors, spec.Deployment{}) {
		if typ.Name == actions.DefineType {
			found = true
			props := typ.ParamsSchema["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
			if !strings.Contains(typ.Description, "No connectors are configured") {
				t.Fatalf("description does not state connector availability: %s", typ.Description)
			}
			if _, ok := props["connectors"]; ok {
				t.Fatalf("schema offers a connector when none are configured: %+v", props["connectors"])
			}
		}
	}
	if !found {
		t.Fatal("agent:define was not offered without connectors")
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
