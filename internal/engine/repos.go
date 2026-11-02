package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/repos"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
)

// Repositories an agent reads (spec repos:). Before the model is called,
// orbit fetches each into its mirror with a contents:read token (revoked
// after) and writes the tree at the ref's commit, without .git, under
// inputs/repos/<name>. What the run read is _repos.json (repo, ref, sha),
// an input like any other, so the unchanged gate compares commits and a
// recovered attempt re-extracts the same ones. History and GitHub state are
// orbit's tools over the mirror and the App (repoTool), never a credential
// or a .git the model holds.

// reposFile is the record of what a run read, an input beside the trees.
const reposFile = "_repos.json"

// repoRead is one repository a run was given.
type repoRead struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
}

// repoRun is what a run's repositories add to its turn: the prompt's
// section and the files written (a live sandbox is sent them).
type repoRun struct {
	prompt string
	files  []string
}

func (e *Engine) githubApp(d spec.Deployment) (*github.App, error) {
	return github.NewApp(d.GitHub, e.Env, e.Client)
}

// readToken mints a token for one repository the agent reads, refusing any
// not in github.read_repositories whatever the caller asks.
func (e *Engine) readToken(ctx context.Context, d spec.Deployment, repo string, perms github.Permissions) (*github.Token, error) {
	if !d.AllowsRead(repo) {
		return nil, fmt.Errorf("%s is not in github.read_repositories", repo)
	}
	app, err := e.githubApp(d)
	if err != nil {
		return nil, err
	}
	return app.Token(ctx, repo, perms)
}

// fetchMirror brings repo's mirror up to date with a contents:read token
// that exists for this fetch alone.
func (e *Engine) fetchMirror(ctx context.Context, d spec.Deployment, repo string) error {
	tok, err := e.readToken(ctx, d, repo, github.ReadContents)
	if err != nil {
		return err
	}
	defer func() { _ = tok.Revoke(context.WithoutCancel(ctx)) }()
	return e.Repos.Fetch(ctx, repo, tok.GitAuthEnv())
}

// repoInputs gives the run its repositories. A recovered attempt (rec)
// reads the commits its interrupted attempt read, from the restored
// _repos.json; any other fetches and resolves each ref now.
func (e *Engine) repoInputs(ctx context.Context, d spec.Deployment, a *spec.Agent, runID string, attempt int, workdir, inputsDir string,
	recovering bool, inputs []runner.Input, tools bool, emit func(string, any)) ([]runner.Input, *repoRun, error) {
	if len(a.Repos) == 0 {
		return inputs, nil, nil
	}
	if e.Repos == nil {
		return nil, nil, errors.New("repos: this instance keeps no repository mirrors")
	}
	var read []repoRead
	if recovering {
		for _, in := range inputs {
			if path.Base(in.Path) == reposFile {
				if err := json.Unmarshal(in.Content, &read); err != nil {
					return nil, nil, fmt.Errorf("repos: the interrupted attempt's %s: %w", reposFile, err)
				}
			}
		}
	}
	fresh := read == nil
	if fresh {
		for _, r := range a.Repos {
			if err := e.fetchMirror(ctx, d, r.Repo); err != nil {
				emit("orbit.repo_failed", map[string]any{"repo": r.Repo, "error": err.Error()})
				return nil, nil, fmt.Errorf("repo %s: %w", r.Repo, err)
			}
			sha, err := e.Repos.Resolve(ctx, r.Repo, r.Ref)
			if err != nil {
				return nil, nil, fmt.Errorf("repo %s: %w", r.Repo, err)
			}
			read = append(read, repoRead{Repo: r.Repo, Ref: r.Ref, SHA: sha})
		}
	}
	rr := &repoRun{}
	var b strings.Builder
	b.WriteString("\n# Repositories\n\nEach is the tree at one commit, fetched for this run: plain files without .git (symlinks and submodules left out). Read them with your file tools.\n\n")
	for _, r := range read {
		name := spec.RepoRef{Repo: r.Repo}.Name()
		rel := filepath.Join(inputsDir, "repos", name)
		dest := filepath.Join(workdir, rel)
		if err := os.RemoveAll(dest); err != nil {
			return nil, nil, err
		}
		files, skipped, err := e.Repos.Extract(ctx, r.Repo, r.SHA, dest)
		if err != nil && !fresh && !e.Repos.HasCommit(ctx, r.Repo, r.SHA) {
			// A recovered attempt on a host whose mirror lacks the commit.
			if ferr := e.fetchMirror(ctx, d, r.Repo); ferr == nil {
				files, skipped, err = e.Repos.Extract(ctx, r.Repo, r.SHA, dest)
			}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("repo %s: %w", r.Repo, err)
		}
		for _, f := range files {
			rr.files = append(rr.files, filepath.Join(rel, filepath.FromSlash(f)))
		}
		emit("orbit.repo", map[string]any{"repo": r.Repo, "ref": r.Ref, "sha": r.SHA, "path": filepath.ToSlash(rel), "files": len(files), "skipped": skipped})
		ref := r.Ref
		if ref == "" {
			ref = "the default branch"
		}
		fmt.Fprintf(&b, "- %s at %s, commit %s: `%s/`\n", r.Repo, ref, r.SHA, filepath.ToSlash(rel))
	}
	if tools {
		b.WriteString("\nHistory and GitHub state come from orbit's tools, exact and current, never from guessing or the web: " +
			"`git_log`, `git_show`, `git_diff` and `git_blame` over orbit's mirror (every branch, tag and pull request head, as refs/pull/<n>/head), " +
			"and `pr_list` and `pr_view` (with checks) from GitHub.\n")
	}
	rr.prompt = b.String()
	if !fresh {
		return inputs, rr, nil
	}
	body, _ := json.MarshalIndent(read, "", "  ")
	rel := filepath.Join(inputsDir, reposFile)
	if err := os.WriteFile(filepath.Join(workdir, rel), body, 0o600); err != nil {
		return nil, nil, err
	}
	if err := e.Store.PutArtifact(ctx, runID, attempt, "in", rel, "application/json", body); err != nil {
		return nil, nil, err
	}
	return append(inputs, runner.Input{Name: strings.TrimSuffix(reposFile, ".json"), Path: rel, MediaType: "application/json", Content: body}), rr, nil
}

// repoSocket returns the agent's repository tools socket, or "" for an
// agent that reads no repository.
func (e *Engine) repoSocket(a *spec.Agent) string {
	if len(a.Repos) == 0 || e.Repos == nil {
		return ""
	}
	agent := a.Name
	return e.agentSocket("repos", agent, func(conn net.Conn) {
		var resp harness.RepoResponse
		req, err := harness.ReadRepoRequest(conn)
		if err == nil {
			resp.Text, err = e.repoTool(context.Background(), agent, req.Tool, req.Args)
		}
		if err != nil {
			resp.Error = err.Error()
		}
		b, _ := json.Marshal(resp)
		_, _ = conn.Write(append(b, '\n'))
	})
}

// repoChatTools adds the repository tools to a chat run's own.
func (e *Engine) repoChatTools(a *spec.Agent, chat *runner.Chat) {
	if len(a.Repos) == 0 || e.Repos == nil {
		return
	}
	for _, t := range harness.RepoTools {
		chat.Tools = append(chat.Tools, runner.ChatTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	inner, agent := chat.Call, a.Name
	chat.Call = func(ctx context.Context, name string, args json.RawMessage) (string, bool) {
		if !harness.IsRepoTool(name) {
			return inner(ctx, name, args)
		}
		text, err := e.repoTool(ctx, agent, name, args)
		if err != nil {
			return name + ": " + err.Error(), true
		}
		return text, false
	}
}

// repoArgs are every repository tool's arguments; each tool reads its own.
type repoArgs struct {
	Repo   string `json:"repo"`
	Rev    string `json:"rev"`
	Path   string `json:"path"`
	Since  string `json:"since"`
	Max    int    `json:"max"`
	Base   string `json:"base"`
	Head   string `json:"head"`
	Stat   bool   `json:"stat"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	State  string `json:"state"`
	Limit  int    `json:"limit"`
	Number int    `json:"number"`
}

// repoTool runs one repository tool for agent, within the repositories its
// spec in force names: git over the mirror, GitHub with a ReadPulls token
// minted for the call and revoked after it.
func (e *Engine) repoTool(ctx context.Context, agent, tool string, raw json.RawMessage) (string, error) {
	snap := e.Current()
	a, ok := snap.Config.Agents[agent]
	if !ok {
		return "", fmt.Errorf("agent %s is not configured", agent)
	}
	var args repoArgs
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return "", fmt.Errorf("arguments: %w", err)
		}
	}
	repo := args.Repo
	if repo == "" && len(a.Repos) == 1 {
		repo = a.Repos[0].Repo
	}
	var ref *spec.RepoRef
	for i := range a.Repos {
		if a.Repos[i].Repo == repo {
			ref = &a.Repos[i]
		}
	}
	if ref == nil {
		names := make([]string, len(a.Repos))
		for i, r := range a.Repos {
			names[i] = r.Repo
		}
		return "", fmt.Errorf("repo %q is not one of yours (%s)", repo, strings.Join(names, ", "))
	}
	d := snap.Config.Deployment
	if _, has := e.Repos.Has(repo); !has && !strings.HasPrefix(tool, "pr_") {
		if err := e.fetchMirror(ctx, d, repo); err != nil {
			return "", err
		}
	}
	rev := args.Rev
	if rev == "" {
		rev = ref.Ref
	}
	asJSON := func(v any, err error) (string, error) {
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(v)
		return repos.Clip(string(b), repos.MaxOutput), nil
	}
	switch tool {
	case "git_log":
		return asJSON(e.Repos.Log(ctx, repo, rev, args.Path, args.Max, args.Since))
	case "git_show":
		return e.Repos.Show(ctx, repo, args.Rev, args.Path)
	case "git_diff":
		return e.Repos.Diff(ctx, repo, args.Base, args.Head, args.Path, args.Stat)
	case "git_blame":
		return e.Repos.Blame(ctx, repo, rev, args.Path, args.Start, args.End)
	case "pr_list", "pr_view":
		tok, err := e.readToken(ctx, d, repo, github.ReadPulls)
		if err != nil {
			return "", err
		}
		defer func() { _ = tok.Revoke(context.WithoutCancel(ctx)) }()
		c := tok.Client()
		if tool == "pr_list" {
			return asJSON(c.ListPulls(ctx, repo, args.State, args.Limit))
		}
		pr, err := c.ViewPull(ctx, repo, args.Number)
		if err != nil {
			return "", err
		}
		runs, err := c.CheckRuns(ctx, repo, pr.Head.SHA)
		if err != nil {
			return "", fmt.Errorf("checks of %s: %w", pr.Head.SHA, err)
		}
		statuses, err := c.Statuses(ctx, repo, pr.Head.SHA)
		if err != nil {
			return "", fmt.Errorf("statuses of %s: %w", pr.Head.SHA, err)
		}
		checks := make([]map[string]any, len(runs))
		for i, r := range runs {
			checks[i] = map[string]any{"name": r.Name, "status": r.Status, "conclusion": r.Conclusion, "url": r.HTMLURL}
		}
		sts := make([]map[string]any, len(statuses))
		for i, s := range statuses {
			sts[i] = map[string]any{"context": s.Context, "state": s.State, "url": s.TargetURL}
		}
		return asJSON(map[string]any{"pull_request": pr, "checks": checks, "statuses": sts}, nil)
	}
	return "", fmt.Errorf("unknown repository tool %q", tool)
}
