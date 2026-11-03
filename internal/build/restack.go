package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/github"
)

// Restack is a stacked ticket's branch moved onto a new base, by code: its
// own commits (From..Head) replayed onto Onto's head with git rebase, no
// model involved (docs/tickets.md, Stacked tickets). Onto is the branch the
// ticket now stacks on: the parent's after the parent changed, or the
// default branch once the parent merged.
type Restack struct {
	Repo     string
	TicketID string
	// Onto is a branch name; empty is the repository's default branch.
	Onto string
	// From is the commit the ticket's own commits start after (its
	// recorded base); Head its branch's head.
	From string
	Head string
	// Committer commits the replayed commits; their authors are kept.
	Committer Identity
}

// Restacked is a restack that pushed.
type Restacked struct {
	// Base is Onto's head the commits now start after; Head the new head.
	Base string
	Head string
	// Branch is the branch the PR should now target.
	Branch string
}

// ErrRestackConflict is a rebase that stopped on a conflict: the ticket
// needs a build, not a mechanical replay.
var ErrRestackConflict = errors.New("the ticket's commits do not apply onto the new base")

// restackDepth is how much of the ticket's branch is fetched to find its
// recorded base: a ticket carries one build and a few fixes and revises.
const restackDepth = 64

// Restack replays a ticket's own commits onto a new base and force-pushes
// its branch, with a read token to fetch and a delivery token to push,
// each revoked after. A conflict aborts the rebase and pushes nothing.
func (r *Runner) Restack(ctx context.Context, app *github.App, s Restack) (*Restacked, error) {
	if app == nil {
		return nil, errors.New("restacking needs the GitHub App")
	}
	branch, err := Branch(s.TicketID)
	if err != nil {
		return nil, err
	}
	if !shaRE.MatchString(s.From) || !shaRE.MatchString(s.Head) {
		return nil, fmt.Errorf("restack %s: from %q and head %q must be commits", branch, s.From, s.Head)
	}
	if err := os.MkdirAll(r.WorkRoot, 0o700); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(r.WorkRoot, "restack-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	if err := os.MkdirAll(filepath.Join(work, "home"), 0o700); err != nil {
		return nil, err
	}
	bin := r.Git
	if bin == "" {
		bin = "git"
	}
	top := git{bin: bin, dir: work, home: filepath.Join(work, "home")}
	if _, err := top.run(ctx, nil, "init", "--quiet", "repo"); err != nil {
		return nil, err
	}
	g := git{bin: bin, dir: filepath.Join(work, "repo"), home: top.home}
	if _, err := g.run(ctx, nil, "remote", "add", "origin", r.cloneURL(s.Repo)); err != nil {
		return nil, err
	}

	read, err := app.Token(ctx, s.Repo, github.ReadContents)
	if err != nil {
		return nil, err
	}
	defBranch, _, branchSHA, err := g.remoteHeads(ctx, read.GitAuthEnv(), branch)
	if err != nil {
		_ = read.Revoke(context.WithoutCancel(ctx))
		return nil, err
	}
	onto := s.Onto
	if onto == "" {
		onto = defBranch
	}
	if branchSHA != s.Head {
		_ = read.Revoke(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("restack %s: the branch is at %s, not the %s orbit recorded", branch, short(branchSHA), short(s.Head))
	}
	err = g.fetchDepth(ctx, read.GitAuthEnv(), "refs/heads/"+branch, "refs/orbit/branch", restackDepth)
	if err == nil {
		err = g.fetchDepth(ctx, read.GitAuthEnv(), "refs/heads/"+onto, "refs/orbit/onto", restackDepth)
	}
	_ = read.Revoke(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	base, err := g.run(ctx, nil, "rev-parse", "refs/orbit/onto")
	if err != nil {
		return nil, err
	}
	base = strings.TrimSpace(base)
	if _, err := g.run(ctx, nil, "merge-base", "--is-ancestor", s.From, s.Head); err != nil {
		return nil, fmt.Errorf("restack %s: its recorded base %s is not in its history", branch, short(s.From))
	}
	when := time.Now().UTC().Format(time.RFC3339)
	env := []string{"GIT_COMMITTER_NAME=" + s.Committer.Name, "GIT_COMMITTER_EMAIL=" + s.Committer.Email, "GIT_COMMITTER_DATE=" + when}
	if _, err := g.run(ctx, nil, "checkout", "--quiet", "--detach", s.Head); err != nil {
		return nil, err
	}
	if _, err := g.run(ctx, env, "rebase", "--quiet", "--no-autosquash", "--no-verify", "--onto", base, s.From); err != nil {
		conflicts, _ := g.run(ctx, nil, "diff", "--name-only", "--diff-filter=U")
		_, _ = g.run(ctx, nil, "rebase", "--abort")
		files := strings.Fields(conflicts)
		if len(files) == 0 {
			return nil, fmt.Errorf("restack %s onto %s: %w", branch, onto, err)
		}
		return nil, fmt.Errorf("%w: %s onto %s conflicts in %s", ErrRestackConflict, branch, onto, strings.Join(files, ", "))
	}
	head, err := g.run(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	head = strings.TrimSpace(head)
	rs, err := refspec(branch, head)
	if err != nil {
		return nil, err
	}
	tok, err := app.Token(ctx, s.Repo, github.Deliver)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tok.Revoke(context.WithoutCancel(ctx)) }()
	if _, err := g.run(ctx, tok.GitAuthEnv(), "push", "--quiet", "origin", rs); err != nil {
		return nil, err
	}
	return &Restacked{Base: base, Head: head, Branch: onto}, nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
