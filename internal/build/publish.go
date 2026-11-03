package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kensac/orbit/internal/github"
)

// Clone and Publish are a chat agent's GitHub path (github:clone and
// github:pr, internal/actions): orbit clones and pushes with the App's
// tokens, so no token ever enters the agent's sandbox. Publish never runs
// git in a .git the model wrote: it takes only the working tree, overlays it
// on a fresh clone of the default branch, and holds it to the build's diff
// policy before committing it as the App's bot user.

// ChatBranchPrefix is the namespace an agent's branches are pushed to,
// inside orbit/ (github.BranchPrefix), so github:merge's gate covers them.
const ChatBranchPrefix = "orbit/chat/"

var chatBranchRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ChatBranch is the branch an agent's name for its work maps to:
// orbit/chat/<name>.
func ChatBranch(name string) (string, error) {
	name = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), ChatBranchPrefix)
	if !chatBranchRE.MatchString(name) || strings.Contains(name, "..") || strings.HasSuffix(name, ".lock") {
		return "", fmt.Errorf("branch %q: want lower-case letters, digits, '.', '_' or '-' (pushed as %s<name>)", name, ChatBranchPrefix)
	}
	return ChatBranchPrefix + name, nil
}

// Git is where Clone and Publish work: the git binary and a scratch root.
type Git struct {
	Bin      string
	WorkRoot string
	// CloneURL is where a repository is fetched from and pushed to;
	// https://github.com/<repo>.git by default (tests point it elsewhere).
	CloneURL func(repo string) string
}

func (gg Git) url(repo string) string {
	if gg.CloneURL != nil {
		return gg.CloneURL(repo)
	}
	return "https://github.com/" + repo + ".git"
}

func (gg Git) scratch(prefix string) (git, func(), error) {
	if err := os.MkdirAll(gg.WorkRoot, 0o700); err != nil {
		return git{}, nil, err
	}
	dir, err := os.MkdirTemp(gg.WorkRoot, prefix)
	if err != nil {
		return git{}, nil, err
	}
	bin := gg.Bin
	if bin == "" {
		bin = "git"
	}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return git{}, nil, err
	}
	return git{bin: bin, dir: dir, home: home}, func() { _ = os.RemoveAll(dir) }, nil
}

// Clone fetches repo's default branch with a read token, revoked after, and
// returns the checkout as a tar (with .git, whose config names the plain
// https remote and holds no credential).
func (gg Git) Clone(ctx context.Context, app *github.App, repo string) ([]byte, error) {
	top, done, err := gg.scratch("clone-")
	if err != nil {
		return nil, err
	}
	defer done()
	tok, err := app.Token(ctx, repo, github.ReadContents)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tok.Revoke(context.WithoutCancel(ctx)) }()
	if _, err := top.run(ctx, tok.GitAuthEnv(), "clone", "--quiet", "--no-tags", gg.url(repo), "repo"); err != nil {
		return nil, err
	}
	tree, err := packTree(filepath.Join(top.dir, "repo"))
	if err != nil {
		return nil, err
	}
	return tree.Bytes(), nil
}

// PublishRequest is one push of an agent's work, and its PR.
type PublishRequest struct {
	App  *github.App
	Repo string
	// Branch is the full branch (ChatBranch).
	Branch string
	// Tree is the repository's working tree as a tar; any .git in it is
	// ignored.
	Tree io.Reader
	// Title and Body are the commit's and the PR's (scrubbed of
	// attribution).
	Title, Body string
	Author      Identity
	// RunID names the run in the commit's trailer.
	RunID  string
	Policy DiffPolicy
	// OpenPR opens (or updates) a PR from Branch into the default branch.
	OpenPR bool
}

// PublishResult is what was pushed.
type PublishResult struct {
	Branch   string `json:"branch"`
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	Files    int    `json:"files"`
	Added    int    `json:"added"`
	Deleted  int    `json:"deleted"`
	PRNumber int    `json:"pull_number,omitempty"`
	PRURL    string `json:"pr_url,omitempty"`
}

// Publish commits the tree on top of the default branch's head and pushes it
// to Branch (forced: the branch is the agent's own), then opens or updates
// its PR when asked.
func (gg Git) Publish(ctx context.Context, r PublishRequest) (*PublishResult, error) {
	if !strings.HasPrefix(r.Branch, ChatBranchPrefix) {
		return nil, fmt.Errorf("refusing to push %q: an agent's branches are under %s", r.Branch, ChatBranchPrefix)
	}
	if strings.TrimSpace(r.Title) == "" {
		return nil, errors.New("a title is required")
	}
	top, done, err := gg.scratch("publish-")
	if err != nil {
		return nil, err
	}
	defer done()
	if _, err := top.run(ctx, nil, "init", "--quiet", "repo"); err != nil {
		return nil, err
	}
	g := git{bin: top.bin, dir: filepath.Join(top.dir, "repo"), home: top.home}
	if _, err := g.run(ctx, nil, "remote", "add", "origin", gg.url(r.Repo)); err != nil {
		return nil, err
	}
	read, err := r.App.Token(ctx, r.Repo, github.ReadContents)
	if err != nil {
		return nil, err
	}
	defBranch, defSHA, _, err := g.remoteHeads(ctx, read.GitAuthEnv(), r.Branch)
	if err == nil {
		err = g.fetch(ctx, read.GitAuthEnv(), "refs/heads/"+defBranch, "refs/orbit/base")
	}
	_ = read.Revoke(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	if _, err := g.run(ctx, nil, "checkout", "--quiet", "--detach", defSHA); err != nil {
		return nil, err
	}
	if err := applyTree(g.dir, r.Tree); err != nil {
		return nil, err
	}
	diff, err := r.Policy.check(ctx, g)
	if err != nil {
		return nil, err
	}
	title := prTitle(r.Title, r.Title)
	sha, err := commit(ctx, g, r.Author, title, commitBody(r.Body), r.RunID)
	if err != nil {
		return nil, err
	}
	res := &PublishResult{Branch: r.Branch, BaseSHA: defSHA, HeadSHA: sha, Files: len(diff.Files), Added: diff.Added, Deleted: diff.Deleted}
	tok, err := r.App.Token(ctx, r.Repo, github.Deliver)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tok.Revoke(context.WithoutCancel(ctx)) }()
	if _, err := g.run(ctx, tok.GitAuthEnv(), "push", "--quiet", "origin", "+"+sha+":refs/heads/"+r.Branch); err != nil {
		return nil, err
	}
	if !r.OpenPR {
		return res, nil
	}
	c := tok.Client()
	body := github.ScrubAttribution(strings.TrimSpace(r.Body))
	pr, err := c.OpenPR(ctx, r.Repo, r.Branch)
	if err != nil {
		return res, fmt.Errorf("find the PR: %w", err)
	}
	if pr == nil {
		pr, err = c.CreatePR(ctx, r.Repo, github.NewPR{Title: title, Head: r.Branch, Base: defBranch, Body: body})
		if err != nil {
			return res, fmt.Errorf("open the PR: %w", err)
		}
	} else if err := c.UpdatePR(ctx, r.Repo, pr.Number, title, body); err != nil {
		return res, fmt.Errorf("update PR #%d: %w", pr.Number, err)
	}
	res.PRNumber, res.PRURL = pr.Number, pr.HTMLURL
	return res, nil
}
