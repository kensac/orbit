package engine_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// github:comment end to end: the run proposes a comment, the rule lets it run, orbit posts it as the App with an issues:write token it revokes; a repository outside comment_repositories is denied when proposed.
func TestGitHubCommentAction(t *testing.T) {
	s := testdb.New(t)
	gh := githubtest.New(t)
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("orbit.yaml", fmt.Sprintf(`
timezone: America/New_York
defaults: {runner: fake, channels: [log]}
runners: {fake: {kind: claude-code}}
channels: {log: {kind: log}}
github:
  app_id: %d
  private_key_env: GH_KEY
  api_url: %s
  repositories: [kensac/elsewhere]
  comment_repositories: [kensac/orbit]
`, githubtest.AppID, gh.URL()))
	write("agents/ops.yaml", "name: ops\ncharter: Watch the homelab.\nactions: {github:comment: auto}\n")
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{output: `{"summary": "commented", "report": "r", "notify": {"send": false, "priority": "default", "title": ""},
  "memory": {"update": false, "content": ""}, "questions": [], "actions": [
    {"type": "github:comment", "params": {"repo": "kensac/orbit", "number": 9, "body": "Restarts since 06:00: 4."}, "reason": "report"},
    {"type": "github:comment", "params": {"repo": "kensac/elsewhere", "number": 1, "body": "x"}, "reason": "elsewhere"}
  ]}`}
	e := &engine.Engine{
		Store: s, DataDir: t.TempDir(), ConfigDir: dir, Env: func(k string) (string, bool) { return githubtest.KeyPEM(), k == "GH_KEY" },
		Client: gh.Srv.Client(), Secret: []byte("secret"),
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) { return fr, nil },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg.Agents["ops"])
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"ops": {SHA: cfg.Agents["ops"].SHA, JSON: b, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "ops", SpecSHA: cfg.Agents["ops"].SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fr.got.System+fr.got.Prompt, "github:comment") {
		t.Error("the model is offered github:comment")
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	acts, _ := s.ListActions(ctx, "ops", "", 0)
	by := map[string]store.Action{}
	for _, a := range acts {
		by[a.Reason] = a
	}
	if a := by["report"]; a.Status != "executed" || !strings.Contains(string(a.Result), "issuecomment-1001") {
		t.Fatalf("report: %s %s %v", a.Status, a.Result, a.Error)
	}
	if a := by["elsewhere"]; a.Status != "denied" || a.Error == nil || !strings.Contains(*a.Error, "not in github.comment_repositories") {
		t.Fatalf("elsewhere: %s %v", a.Status, a.Error)
	}
	gh.Mu.Lock()
	defer gh.Mu.Unlock()
	perms, _ := json.Marshal(gh.TokenReq["permissions"])
	if len(gh.Comments) != 1 || gh.Comments[0]["body"] != "Restarts since 06:00: 4." || string(perms) != `{"issues":"write"}` || len(gh.Revoked) != 1 {
		t.Fatalf("comments %v, token %s, revoked %d", gh.Comments, perms, len(gh.Revoked))
	}
}
