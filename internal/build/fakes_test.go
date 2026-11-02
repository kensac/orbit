package build

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/build/buildtest"
	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/github/githubtest"
)

// world is one build's surroundings: a bare repository standing in for
// GitHub's git side, the fake GitHub API, the fake sandboxd and a runner.
type world struct {
	t      *testing.T
	bare   string
	gh     *githubtest.Server
	sb     *buildtest.Sandboxd
	app    *github.App
	runner *Runner

	mu        sync.Mutex
	decisions []recorded
	events    []string
	artifacts map[string][]byte
	failRec   bool
}

type recorded struct {
	Req PermissionRequest
	D   Decision
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return buildtest.Git(t, dir, args...)
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, artifacts: map[string][]byte{}}
	w.bare = buildtest.Repo(t)
	w.gh = githubtest.New(t)
	w.gh.Mu.Lock()
	w.gh.PR = nil
	w.gh.BranchHead = func(ref string) string { return buildtest.RevParse(w.bare, "refs/heads/"+ref) }
	w.gh.Mu.Unlock()
	w.app = &github.App{ID: githubtest.AppID, Key: githubtest.Key, BaseURL: w.gh.URL(), HTTP: w.gh.Srv.Client(), Now: time.Now}
	w.sb = buildtest.NewSandboxd(t)
	w.runner = &Runner{WorkRoot: t.TempDir(), CloneURL: func(string) string { return w.bare }}
	return w
}

func (w *world) deps() Deps {
	return Deps{
		App:     w.app,
		Sandbox: w.sb.Client(w.t),
		Emit: func(kind string, payload []byte) {
			w.mu.Lock()
			w.events = append(w.events, kind+" "+string(payload))
			w.mu.Unlock()
		},
		Artifact: func(dir, name, mt string, content []byte) error {
			w.mu.Lock()
			w.artifacts[dir+"/"+name] = content
			w.mu.Unlock()
			return nil
		},
		Record: func(r PermissionRequest, d Decision) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.failRec {
				return fmt.Errorf("database is down")
			}
			w.decisions = append(w.decisions, recorded{r, d})
			return nil
		},
	}
}

const testTicket = "3f2a9c1e-0000-4000-8000-000000000001"

const testCredential = "sk-ant-oat01-test-credential-value-0123456789"

func (w *world) job(runID string, steps ...map[string]any) Job {
	s, _ := json.Marshal(map[string]any{"steps": steps})
	return Job{
		RunID: runID, Attempt: 1, TicketID: testTicket, Title: "Add a hello file", Repo: "kensac/orbit",
		Plan: "Add hello.txt.\n\nFAKE-SCRIPT: " + string(s), PlanVersion: 2,
		Ticket:  json.RawMessage(`{"title":"Add a hello file","body":"please"}`),
		Purpose: "build",
		Policy:  orbitRepo,
		Model:   "m", MaxTurns: 30, Timeout: time.Minute,
		Env:    map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": testCredential},
		Author: Identity{Name: "orbit-kensac[bot]", Email: "351241810+orbit-kensac[bot]@users.noreply.github.com"},
	}
}

func (w *world) remoteBranch() string { return buildtest.RevParse(w.bare, "refs/heads/orbit/3f2a9c1e") }

func (w *world) hasEvent(prefix string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range w.events {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
