package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/kensac/orbit/internal/build"
	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/spec"
)

// GitHub for a chat agent: orbit clones into its workspace and pushes what
// it wrote, with the App's tokens, which never enter the agent's sandbox.
// github:merge (actions.go) merges what github:pr opened, behind the same
// gate as a ticket's PR.
const (
	CloneType = "github:clone"
	PRType    = "github:pr"
)

// Workspace is the conversation's workspace an action reads and writes:
// the running sandbox during the agent's turn, its stored directory after.
type Workspace interface {
	// Put unpacks a tar under dir.
	Put(ctx context.Context, dir string, tar []byte) error
	// Get packs dir as a tar, entries relative to it.
	Get(ctx context.Context, dir string) ([]byte, error)
}

type cloneParams struct {
	Repo string `json:"repo"`
	Dir  string `json:"dir"`
}

type prParams struct {
	Repo   string `json:"repo"`
	Dir    string `json:"dir"`
	Branch string `json:"branch"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	// Push only pushes the branch; false (the default) also opens or
	// updates its PR.
	PushOnly bool `json:"push_only"`
}

// workspaceDir is a relative directory of the workspace.
func workspaceDir(d string) (string, error) {
	c := path.Clean("/" + strings.TrimSpace(d))[1:]
	if c == "" || c == "." || strings.HasPrefix(c, ".orbit") || strings.HasPrefix(c, "inputs") || strings.HasPrefix(c, "uploads") {
		return "", fmt.Errorf("dir %q: want a directory of the workspace (not .orbit, inputs or uploads)", d)
	}
	return c, nil
}

func cloneType(d spec.Deployment) Type {
	return Type{
		Name: CloneType,
		Description: "Clone a repository's default branch into your workspace at dir (default: the repository's name). " +
			"Work on it there; github:pr pushes what you changed.",
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo": map[string]any{"type": "string", "enum": d.GitHub.Repositories},
				"dir":  map[string]any{"type": "string"},
			},
			"required":             []string{"repo"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error { _, err := decodeClone(d, p); return err },
	}
}

func decodeClone(d spec.Deployment, p json.RawMessage) (cloneParams, error) {
	var v cloneParams
	if err := strictDecode(p, &v); err != nil {
		return v, err
	}
	if !d.AllowsRepo(v.Repo) {
		return v, fmt.Errorf("github:clone: repository %q is not in github.repositories", v.Repo)
	}
	if v.Dir == "" {
		_, v.Dir, _ = strings.Cut(v.Repo, "/")
	}
	var err error
	v.Dir, err = workspaceDir(v.Dir)
	return v, err
}

func prType(d spec.Deployment) Type {
	return Type{
		Name: PRType,
		Description: "Push the repository in your workspace at dir to the branch orbit/chat/<branch>, as one commit on top of the " +
			"default branch (orbit commits your working tree; your own commits and .git are not used), and open or update its " +
			"pull request with title and body. push_only: true pushes without a PR. github:merge merges it once its checks pass.",
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo":      map[string]any{"type": "string", "enum": d.GitHub.Repositories},
				"dir":       map[string]any{"type": "string"},
				"branch":    map[string]any{"type": "string", "pattern": "^[a-z0-9][a-z0-9._-]{0,62}$"},
				"title":     map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
				"body":      map[string]any{"type": "string", "maxLength": 20000},
				"push_only": map[string]any{"type": "boolean"},
			},
			"required":             []string{"repo", "dir", "branch", "title"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error { _, err := decodePR(d, p); return err },
	}
}

func decodePR(d spec.Deployment, p json.RawMessage) (prParams, error) {
	var v prParams
	if err := strictDecode(p, &v); err != nil {
		return v, err
	}
	if !d.AllowsRepo(v.Repo) {
		return v, fmt.Errorf("github:pr: repository %q is not in github.repositories", v.Repo)
	}
	var err error
	if v.Dir, err = workspaceDir(v.Dir); err != nil {
		return v, err
	}
	if v.Branch, err = build.ChatBranch(v.Branch); err != nil {
		return v, err
	}
	if strings.TrimSpace(v.Title) == "" {
		return v, fmt.Errorf("github:pr: title is required")
	}
	return v, nil
}

func (e *Executor) app() (*github.App, error) {
	if e.Deployment.GitHub == nil {
		return nil, fmt.Errorf("orbit.yaml has no github section")
	}
	return github.NewApp(e.Deployment.GitHub, e.Env, e.Client)
}

func (e *Executor) clone(ctx context.Context, c Call) (json.RawMessage, error) {
	p, err := decodeClone(e.Deployment, c.Params)
	if err != nil {
		return nil, err
	}
	if c.Workspace == nil {
		return nil, fmt.Errorf("github:clone: no workspace (only a chat agent has one)")
	}
	app, err := e.app()
	if err != nil {
		return nil, err
	}
	tree, err := e.Git.Clone(ctx, app, p.Repo)
	if err != nil {
		return nil, err
	}
	if err := c.Workspace.Put(ctx, p.Dir, tree); err != nil {
		return nil, fmt.Errorf("github:clone: place it in the workspace: %w", err)
	}
	return json.Marshal(map[string]any{"repo": p.Repo, "dir": p.Dir, "bytes": len(tree)})
}

func (e *Executor) publish(ctx context.Context, c Call) (json.RawMessage, error) {
	p, err := decodePR(e.Deployment, c.Params)
	if err != nil {
		return nil, err
	}
	if c.Workspace == nil {
		return nil, fmt.Errorf("github:pr: no workspace (only a chat agent has one)")
	}
	if e.Deployment.Build == nil || e.Deployment.Build.Author == "" {
		return nil, fmt.Errorf("github:pr: needs build.author in orbit.yaml (the App's bot user commits)")
	}
	author, err := build.ParseIdentity(e.Deployment.Build.Author)
	if err != nil {
		return nil, err
	}
	app, err := e.app()
	if err != nil {
		return nil, err
	}
	tree, err := c.Workspace.Get(ctx, p.Dir)
	if err != nil {
		return nil, fmt.Errorf("github:pr: read %s from the workspace: %w", p.Dir, err)
	}
	cred, _ := e.Env("CLAUDE_CODE_OAUTH_TOKEN")
	res, err := e.Git.Publish(ctx, build.PublishRequest{
		App: app, Repo: p.Repo, Branch: p.Branch, Tree: bytes.NewReader(tree), Title: p.Title, Body: p.Body,
		Author: author, RunID: c.RunID, OpenPR: !p.PushOnly,
		Policy: build.DiffPolicy{MaxDiffLines: spec.DefaultMaxDiffLines * 5, Secrets: []string{cred}},
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}
