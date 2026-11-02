package sandboxd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/build"
	"github.com/kensac/orbit/internal/build/buildtest"
	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/spec"
)

// TestIntegrationBuild runs the build runner end to end against a real
// sandboxd and docker: the checkout goes in through the tar filter, the
// stdio permission protocol runs over a real attach (with fakeclaude as
// the image's claude, as uid 10001 on a read-only root), the tree comes
// back, and orbit commits, pushes and opens the PR on the host side (a
// bare repository and the fake GitHub). Like TestIntegrationIsolation it
// skips without docker locally and fails without it under CI.
func TestIntegrationBuild(t *testing.T) {
	d, info := dockerForTest(t)
	if testing.Short() {
		t.Skip("-short")
	}
	arch := goarch(info.Architecture)
	bins := buildBinaries(t, arch)
	_, here, _, _ := runtime.Caller(0)
	fake := filepath.Join(t.TempDir(), "claude")
	cmd := exec.Command("go", "build", "-trimpath", "-o", fake, ".")
	cmd.Dir = filepath.Join(filepath.Dir(here), "..", "build", "buildtest", "testdata", "fakeclaude")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakeclaude: %v\n%s", err, b)
	}
	b, err := os.ReadFile(fake)
	if err != nil {
		t.Fatal(err)
	}
	bins["claude"] = b
	image := buildTestImage(t, d, bins)
	suffix := newID()[:8]
	internalNet, outsideNet, proxyName := "orbit-sbx-test-"+suffix, "orbit-sbx-out-"+suffix, "orbit-egress-"+suffix
	createNetwork(t, d, internalNet, true)
	createNetwork(t, d, outsideNet, false)
	startEgress(t, d, image, proxyName, internalNet, outsideNet)
	dir := socketDir(t)
	cfg := Config{
		Token:   strings.Repeat("s", 40),
		Listen:  "unix:" + filepath.Join(dir, "sandboxd.sock"),
		Image:   image,
		Network: internalNet,
		Proxy:   proxyName + ":3128",
		Docker:  d.sock,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, slog.New(slog.NewTextHandler(&lockedLog{}, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	serveUnix(t, filepath.Join(dir, "sandboxd.sock"), s.Handler())
	sb, err := sandbox.NewClient(cfg.Listen, cfg.Token)
	if err != nil {
		t.Fatal(err)
	}

	bare := buildtest.Repo(t)
	gh := githubtest.New(t)
	gh.Mu.Lock()
	gh.PR = nil
	gh.BranchHead = func(ref string) string { return buildtest.RevParse(bare, "refs/heads/"+ref) }
	gh.Mu.Unlock()
	app := &github.App{ID: githubtest.AppID, Key: githubtest.Key, BaseURL: gh.URL(), HTTP: gh.Srv.Client(), Now: time.Now}
	steps, _ := json.Marshal(map[string]any{"steps": []any{
		map[string]any{"tool": "Write", "input": map[string]any{"file_path": "/work/hello.txt", "content": "hello from the sandbox\n"}},
		map[string]any{"tool": "Write", "input": map[string]any{"file_path": "/work/deploy/Dockerfile", "content": "FROM evil\n"}},
		map[string]any{"tool": "Bash", "input": map[string]any{"command": "go test ./..."}},
	}})
	var decisions []string
	runner := &build.Runner{WorkRoot: t.TempDir(), CloneURL: func(string) string { return bare }}
	res, err := runner.Run(ctx, build.Job{
		RunID: "integration-" + suffix, Attempt: 1, TicketID: "3f2a9c1e-0000-4000-8000-000000000001",
		Title: "Add hello", Repo: "kensac/orbit", Plan: "Add hello.txt.\n\nFAKE-SCRIPT: " + string(steps), PlanVersion: 1,
		Ticket: json.RawMessage(`{"title":"Add hello"}`), Purpose: "build",
		Policy:  spec.BuildRepo{Commands: []string{"go test"}, Protected: []string{"deploy/**"}, MaxDiffLines: 100},
		Timeout: 2 * time.Minute, Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "placeholder-credential-value"},
		Author: build.Identity{Name: "orbit-kensac[bot]", Email: "351241810+orbit-kensac[bot]@users.noreply.github.com"},
	}, build.Deps{App: app, Sandbox: sb, Record: func(r build.PermissionRequest, dec build.Decision) error {
		decisions = append(decisions, r.Tool+" "+map[bool]string{true: "allow", false: "deny"}[dec.Allow])
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(decisions, ",") != "Write allow,Write deny,Bash allow" {
		t.Fatalf("decisions = %v", decisions)
	}
	if paths := res.Diff.Paths(); len(paths) != 1 || paths[0] != "hello.txt" || res.PRNumber != 7 {
		t.Fatalf("result = %+v %v", res, paths)
	}
	if got := buildtest.Git(t, bare, "show", "refs/heads/orbit/3f2a9c1e:hello.txt"); got != "hello from the sandbox" {
		t.Fatalf("pushed hello.txt = %q", got)
	}
	// The sandbox is gone.
	var list []struct{ ID string }
	filters, _ := json.Marshal(map[string][]string{"label": {LabelRunID + "=integration-" + suffix}})
	if err := d.call(ctx, "GET", "/containers/json", url.Values{"all": {"1"}, "filters": {string(filters)}}, nil, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("containers left: %v", list)
	}
}
