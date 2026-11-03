package api_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// withFakeGitHub points v's GitHub App at a fake.
func withFakeGitHub(t *testing.T, v *env) *githubtest.Server {
	t.Helper()
	gh := githubtest.New(t)
	dir := v.e.ConfigDir
	if err := os.WriteFile(filepath.Join(dir, "orbit.yaml"), fmt.Appendf(nil, `
public_url: http://orbit.test
defaults: {runner: r, channels: [c]}
runners: {r: {kind: claude-code}}
channels: {c: {kind: log}}
github: {app_id: %d, private_key_env: GH_KEY, api_url: %s, repositories: [kensac/orbit]}
`, githubtest.AppID, gh.URL()), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	v.e.Env = func(k string) (string, bool) {
		if k == "GH_KEY" {
			return githubtest.KeyPEM(), true
		}
		return "", false
	}
	v.e.Client = gh.Srv.Client()
	if err := v.e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return gh
}

func TestTicketDiff(t *testing.T) {
	v := newEnv(t, nil)
	gh := withFakeGitHub(t, v)
	k := v.person("kanishk")
	u := v.srv.URL + "/api/v1/tickets"
	_, r := call(t, "POST", u, k, `{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/orbit","wake":false}`)
	id := r["ticket_id"].(string)
	if code, _ := call(t, "GET", u+"/"+id+"/diff", k, ""); code != 409 {
		t.Fatalf("a diff with no PR = %d", code)
	}
	_, _ = v.s.ProposePlan(ctx, id, "", "plan")
	_, _ = v.s.ApprovePlan(ctx, id, 1, "kanishk", "")
	a := v.e.Current().Config.Agents["a"]
	run, _ := v.s.StartBuild(ctx, store.StartBuild{TicketID: id, Agent: "a", SpecSHA: a.SHA, Purpose: store.BuildPurposeBuild})
	c, _ := v.s.ClaimRun(ctx, "w")
	pr := 7
	if err := v.s.FinishBuild(ctx, run, store.RunResult{Attempt: c.Attempt, Summary: "built"},
		store.TicketUpdate{Branch: "orbit/abc123", PRNumber: &pr, HeadSHA: githubtest.Head}); err != nil {
		t.Fatal(err)
	}
	gh.Mu.Lock()
	gh.PR["base"].(map[string]any)["sha"] = "2222222222222222222222222222222222222222"
	gh.PRFiles = []map[string]any{
		{"filename": "internal/a.go", "status": "modified", "additions": 2, "deletions": 1,
			"patch": "@@ -10,4 +10,5 @@ func main() {\n \tx()\n-\ty()\n+\tY()\n+\tz()\n \treturn\n }"},
		{"filename": "logo.png", "status": "added", "additions": 0, "deletions": 0},
		{"filename": "big.sql", "status": "added", "additions": 90000, "deletions": 0},
		{"filename": "docs/new.md", "previous_filename": "docs/old.md", "status": "renamed", "additions": 0, "deletions": 0},
	}
	gh.Mu.Unlock()

	code, r := call(t, "GET", u+"/"+id+"/diff", k, "")
	if code != 200 || r["pr"] != 7.0 || r["repo"] != "kensac/orbit" || r["truncated"] != false ||
		r["head"].(map[string]any)["sha"] != githubtest.Head || r["base"].(map[string]any)["sha"] != "2222222222222222222222222222222222222222" ||
		r["base"].(map[string]any)["ref"] != "main" {
		t.Fatalf("diff = %d %v", code, r)
	}
	files := r["files"].([]any)
	f := files[0].(map[string]any)
	h := f["hunks"].([]any)[0].(map[string]any)
	lines := h["lines"].([]any)
	del, add := lines[1].(map[string]any), lines[3].(map[string]any)
	if f["status"] != "modified" || f["additions"] != 2.0 || h["header"] != "func main() {" || h["new_lines"] != 5.0 ||
		del["old"] != 11.0 || del["new"] != nil || add["new"] != 12.0 || add["text"] != "\tz()" {
		t.Fatalf("a.go = %v", f)
	}
	if png := files[1].(map[string]any); png["binary"] != true || png["status"] != "added" {
		t.Fatalf("png = %v", png)
	}
	if big := files[2].(map[string]any); big["truncated"] != true || big["binary"] != false || big["additions"] != 90000.0 {
		t.Fatalf("big = %v", big)
	}
	if mv := files[3].(map[string]any); mv["status"] != "renamed" || mv["old_path"] != "docs/old.md" || mv["binary"] != false {
		t.Fatalf("rename = %v", mv)
	}
	if code, r := call(t, "GET", u+"/"+id+"/diff?path=docs/new.md", k, ""); code != 200 || len(r["files"].([]any)) != 1 {
		t.Fatalf("one file = %d %v", code, r)
	}
	if code, _ := call(t, "GET", u+"/"+id+"/diff?path=nope.go", k, ""); code != 404 {
		t.Fatalf("a file the PR does not change = %d", code)
	}
	if code, _ := call(t, "GET", u+"/"+id+"/diff?path=../x", k, ""); code != 400 {
		t.Fatalf("an unclean path = %d", code)
	}
	// The token was for this repository, read only, and revoked.
	gh.Mu.Lock()
	perms := gh.TokenReq["permissions"].(map[string]any)
	repos := gh.TokenReq["repositories"]
	revoked := len(gh.Revoked)
	gh.Mu.Unlock()
	if len(perms) != 2 || perms["contents"] != "read" || perms["pull_requests"] != "read" || fmt.Sprint(repos) != "[orbit]" || revoked < 3 {
		t.Fatalf("token = %v %v, revoked %d", perms, repos, revoked)
	}
	if code, _ := call(t, "GET", u+"/00000000-0000-0000-0000-000000000000/diff", k, ""); code != 404 {
		t.Fatalf("no ticket = %d", code)
	}
}
