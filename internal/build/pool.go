package build

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Pool keeps orbit's own clones between runs, one per key (a ticket, a
// conversation), and a few spare clones per repository already at the
// default branch's head, so a build or a reading run starts from a checkout
// that needs only an incremental fetch. Each clone is self-contained (its
// own .git, never a worktree pointing elsewhere), because a build hands the
// whole tree, .git included, to the sandbox. No credential is ever on
// disk: fetches and pushes take the token in git's environment only.
//
// A clone kept for a key is made clean before every use (checkout --force,
// clean -ffdx), so nothing an earlier attempt left in the tree carries over;
// its .git is written only by orbit's git, since the sandbox's readback
// drops every .git.
type Pool struct {
	// Root holds checkouts/<key>/ and warm/<repo>/<id>/.
	Root string
	// Git is the git binary ("git").
	Git string
	// Warm is how many spare clones Prewarm keeps per repository.
	Warm int
	// CloneURL is where a repository is fetched from;
	// https://github.com/<repo>.git when nil.
	CloneURL func(repo string) string
}

var keyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,99}$`)

// TicketKey is a ticket's checkout key: every build of the ticket (the
// build, its fixes and revisions) reuses one clone.
func TicketKey(ticketID string) string {
	return "ticket-" + strings.ToLower(strings.ReplaceAll(ticketID, "-", ""))
}

// LaneKey is a race lane's checkout key: its lanes build one after
// another on their own branches, and a lane's fixes after the pick reuse
// its clone. An empty lane is the ticket's key.
func LaneKey(ticketID, lane string) string {
	if lane == "" {
		return TicketKey(ticketID)
	}
	return TicketKey(ticketID) + "-" + lane
}

// ConversationKey is a conversation's checkout key, for an agent that
// reads a repository across the runs of one conversation.
func ConversationKey(conversationID string) string {
	return "conversation-" + strings.ToLower(strings.ReplaceAll(conversationID, "-", ""))
}

func (p *Pool) bin() string {
	if p.Git == "" {
		return "git"
	}
	return p.Git
}

func (p *Pool) url(repo string) string {
	if p.CloneURL != nil {
		return p.CloneURL(repo)
	}
	return "https://github.com/" + repo + ".git"
}

func repoSlug(repo string) (string, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`).MatchString(repo) || strings.Contains(repo, "..") {
		return "", fmt.Errorf("%q is not owner/name", repo)
	}
	return strings.ReplaceAll(repo, "/", "__"), nil
}

// Dir is where key's clone lives.
func (p *Pool) Dir(key string) (string, error) {
	if !keyRE.MatchString(key) {
		return "", fmt.Errorf("checkout key %q is not [a-z][a-z0-9-]*", key)
	}
	return filepath.Join(p.Root, "checkouts", key), nil
}

// Acquire returns key's clone of repo, its origin set to the repository:
// the one kept for key, else a spare one moved into place, else a new empty
// one. home is the HOME git runs with. The caller fetches and checks out
// what it needs (Prepare).
func (p *Pool) Acquire(ctx context.Context, key, repo, home string) (git, error) {
	dir, err := p.Dir(key)
	if err != nil {
		return git{}, err
	}
	slug, err := repoSlug(repo)
	if err != nil {
		return git{}, err
	}
	g := git{bin: p.bin(), dir: dir, home: home}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if _, err := g.run(ctx, nil, "remote", "set-url", "origin", p.url(repo)); err == nil {
			return g, nil
		}
		// A clone that git cannot use is replaced, not repaired.
		if err := os.RemoveAll(dir); err != nil {
			return git{}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return git{}, err
	}
	if spares, _ := filepath.Glob(filepath.Join(p.Root, "warm", slug, "*")); len(spares) > 0 {
		for _, s := range spares {
			// A rename is atomic: two instances never take the same one.
			if os.Rename(s, dir) == nil {
				if _, err := g.run(ctx, nil, "remote", "set-url", "origin", p.url(repo)); err == nil {
					return g, nil
				}
				_ = os.RemoveAll(dir)
			}
		}
	}
	if err := initClone(ctx, p.bin(), dir, home, p.url(repo)); err != nil {
		_ = os.RemoveAll(dir)
		return git{}, err
	}
	return g, nil
}

func initClone(ctx context.Context, bin, dir, home, url string) error {
	parent := git{bin: bin, dir: filepath.Dir(dir), home: home}
	if _, err := parent.run(ctx, nil, "init", "--quiet", filepath.Base(dir)); err != nil {
		return err
	}
	g := git{bin: bin, dir: dir, home: home}
	_, err := g.run(ctx, nil, "remote", "add", "origin", url)
	return err
}

// Prepare checks a clone out at sha, detached, with every change and
// untracked or ignored file removed.
func (g git) prepare(ctx context.Context, sha string) error {
	if _, err := g.run(ctx, nil, "checkout", "--quiet", "--force", "--detach", sha); err != nil {
		return err
	}
	_, err := g.run(ctx, nil, "clean", "-ffdxq")
	return err
}

// Release removes key's clone: its ticket is finished, or its conversation
// archived. A key with no clone is not an error.
func (p *Pool) Release(key string) error {
	dir, err := p.Dir(key)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// Spares counts repo's spare clones.
func (p *Pool) Spares(repo string) (int, error) {
	slug, err := repoSlug(repo)
	if err != nil {
		return 0, err
	}
	s, err := filepath.Glob(filepath.Join(p.Root, "warm", slug, "*"))
	return len(s), err
}

// Prewarm tops repo's spare clones up to Warm, each a shallow clone of the
// default branch's head, fetched with auth (a read token's GitAuthEnv). It
// returns how many it made.
func (p *Pool) Prewarm(ctx context.Context, repo string, auth []string) (int, error) {
	slug, err := repoSlug(repo)
	if err != nil {
		return 0, err
	}
	have, err := p.Spares(repo)
	if err != nil {
		return 0, err
	}
	made := 0
	for ; have+made < p.Warm; made++ {
		if err := p.warmOne(ctx, slug, repo, auth); err != nil {
			return made, err
		}
	}
	return made, nil
}

func (p *Pool) warmOne(ctx context.Context, slug, repo string, auth []string) error {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	tmp := filepath.Join(p.Root, "warm-tmp", slug+"-"+hex.EncodeToString(b))
	home := tmp + "-home"
	defer os.RemoveAll(home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	dir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := initClone(ctx, p.bin(), dir, home, p.url(repo)); err != nil {
		return fail(err)
	}
	g := git{bin: p.bin(), dir: dir, home: home}
	_, defSHA, _, err := g.remoteHeads(ctx, auth, "")
	if err != nil {
		return fail(err)
	}
	if err := g.fetch(ctx, auth, "HEAD", "refs/orbit/base"); err != nil {
		return fail(err)
	}
	if err := g.prepare(ctx, defSHA); err != nil {
		return fail(err)
	}
	// Only a finished clone becomes a spare: it is moved in whole.
	dst := filepath.Join(p.Root, "warm", slug)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fail(err)
	}
	if err := os.Rename(dir, filepath.Join(dst, filepath.Base(tmp))); err != nil {
		return fail(err)
	}
	_ = os.RemoveAll(tmp)
	return nil
}
