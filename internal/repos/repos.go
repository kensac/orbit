// Package repos keeps a bare mirror of each repository agents read, under
// orbit's data directory, and gives a run the tree at one commit without
// .git. Every git command here runs in orbit's process with no system or
// global config, no hooks and no prompt; a token reaches git only through
// the environment of the fetch that needs it (github.Token.GitAuthEnv),
// never a file, a remote URL or the mirror's config. No model runs git:
// the history tools (tools.go) are orbit's commands over the mirror.
package repos

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Mirrors is orbit's bare mirrors, one per repository.
type Mirrors struct {
	// Root holds <owner>__<name>.git and its lock file.
	Root string
	// Git is the git binary ("git").
	Git string
	// URL is where a repository is fetched from;
	// https://github.com/<repo>.git when nil (tests point it elsewhere).
	URL func(repo string) string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Limits on one tree given to a run. orbit's own tree is a few MB; these
// stop a repository that is mostly binaries or generated files from filling
// the data volume on every run.
const (
	MaxTreeBytes   = 256 << 20
	MaxTreeEntries = 100_000
	// fetchTimeout bounds one fetch; a first full fetch of a large
	// repository is the slow case.
	fetchTimeout = 10 * time.Minute
)

var (
	repoRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	shaRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func slug(repo string) (string, error) {
	if !repoRE.MatchString(repo) || strings.Contains(repo, "..") {
		return "", fmt.Errorf("%q is not owner/name", repo)
	}
	return strings.ReplaceAll(repo, "/", "__"), nil
}

// Dir is repo's mirror.
func (m *Mirrors) Dir(repo string) (string, error) {
	s, err := slug(repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(m.Root, s+".git"), nil
}

// Has reports whether repo has a mirror with at least one commit, and
// returns its directory: a build's clone is seeded from it.
func (m *Mirrors) Has(repo string) (string, bool) {
	dir, err := m.Dir(repo)
	if err != nil {
		return "", false
	}
	if _, err := m.run(context.Background(), dir, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		return "", false
	}
	return dir, true
}

func (m *Mirrors) bin() string {
	if m.Git == "" {
		return "git"
	}
	return m.Git
}

func (m *Mirrors) url(repo string) string {
	if m.URL != nil {
		return m.URL(repo)
	}
	return "https://github.com/" + repo + ".git"
}

// lock serialises fetches of one repository: in this process by mutex,
// and across processes sharing the data directory (two instances during a
// rollout) by flock on a file beside the mirror, so two fetches never race
// on the mirror's ref locks.
func (m *Mirrors) lock(repo string) (func(), error) {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	mu := m.locks[repo]
	if mu == nil {
		mu = &sync.Mutex{}
		m.locks[repo] = mu
	}
	m.mu.Unlock()
	mu.Lock()
	dir, err := m.Dir(repo)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	if err := os.MkdirAll(m.Root, 0o700); err != nil {
		mu.Unlock()
		return nil, err
	}
	f, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		mu.Unlock()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		mu.Unlock()
	}, nil
}

// fetchRefspecs are what a mirror keeps: branches, tags and every pull
// request's head, so a run can diff a PR against its base without a token.
var fetchRefspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*", "+refs/pull/*/head:refs/pull/*/head"}

// Fetch brings repo's mirror up to date with the remote, creating it on
// first use, with auth (a read token's GitAuthEnv) in git's environment
// only. HEAD follows the remote's default branch.
func (m *Mirrors) Fetch(ctx context.Context, repo string, auth []string) error {
	unlock, err := m.lock(repo)
	if err != nil {
		return err
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	dir, _ := m.Dir(repo)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		_ = os.RemoveAll(dir)
		if _, err := m.run(ctx, m.Root, nil, "init", "--quiet", "--bare", dir); err != nil {
			return err
		}
	}
	// The remote is given on the command line, never stored: a mirror's
	// config holds nothing a later command could be steered by.
	url := m.url(repo)
	out, err := m.run(ctx, dir, auth, "ls-remote", "--symref", url, "HEAD")
	if err != nil {
		return err
	}
	var def string
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "ref:" && f[2] == "HEAD" {
			def = f[1]
		}
	}
	if !strings.HasPrefix(def, "refs/heads/") {
		return fmt.Errorf("fetch %s: the remote did not report its default branch", repo)
	}
	args := append([]string{"fetch", "--quiet", "--prune", "--no-tags", "--no-write-fetch-head", url}, fetchRefspecs...)
	if _, err := m.run(ctx, dir, auth, args...); err != nil {
		return fmt.Errorf("fetch %s: %w", repo, err)
	}
	if _, err := m.run(ctx, dir, nil, "symbolic-ref", "HEAD", def); err != nil {
		return err
	}
	return nil
}

// Resolve returns the commit ref names in repo's mirror: a branch, a tag,
// a full sha, or "" for the default branch.
func (m *Mirrors) Resolve(ctx context.Context, repo, ref string) (string, error) {
	dir, err := m.Dir(repo)
	if err != nil {
		return "", err
	}
	if ref == "" {
		ref = "HEAD"
	}
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("%s: %q is not a ref", repo, ref)
	}
	out, err := m.run(ctx, dir, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s has no commit %q", repo, ref)
	}
	sha := strings.TrimSpace(out)
	if !shaRE.MatchString(sha) {
		return "", fmt.Errorf("%s: %q resolved to %q", repo, ref, sha)
	}
	return sha, nil
}

// HasCommit reports whether repo's mirror holds sha.
func (m *Mirrors) HasCommit(ctx context.Context, repo, sha string) bool {
	if !shaRE.MatchString(sha) {
		return false
	}
	got, err := m.Resolve(ctx, repo, sha)
	return err == nil && got == sha
}

// Extract writes repo's tree at sha into dest (created; it must not exist
// or be empty): regular files and directories only, so nothing in a
// repository can point outside dest, and no .git. It returns the files'
// paths relative to dest. Symlinks and submodules are left out (the
// skipped count says how many).
func (m *Mirrors) Extract(ctx context.Context, repo, sha, dest string) (files []string, skipped int, err error) {
	if !shaRE.MatchString(sha) {
		return nil, 0, fmt.Errorf("%q is not a commit sha", sha)
	}
	dir, err := m.Dir(repo)
	if err != nil {
		return nil, 0, err
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return nil, 0, err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = root.Close() }()
	cmd := m.command(ctx, dir, nil, "archive", "--format=tar", sha)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	files, skipped, xerr := untar(root, stdout)
	if xerr != nil {
		_ = cmd.Process.Kill()
	}
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	if xerr != nil {
		return nil, 0, fmt.Errorf("extract %s at %s: %w", repo, sha[:12], xerr)
	}
	if werr != nil {
		return nil, 0, fmt.Errorf("git archive %s: %s", repo, lastLine(strings.TrimSpace(errb.String()), werr))
	}
	return files, skipped, nil
}

func untar(root *os.Root, r io.Reader) ([]string, int, error) {
	tr := tar.NewReader(r)
	var files []string
	var total int64
	entries, skipped := 0, 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, skipped, nil
		}
		if err != nil {
			return nil, 0, err
		}
		if entries++; entries > MaxTreeEntries {
			return nil, 0, fmt.Errorf("more than %d entries", MaxTreeEntries)
		}
		name := path.Clean(h.Name)
		if h.Typeflag == tar.TypeXGlobalHeader || name == "." {
			continue
		}
		if !fs.ValidPath(name) || name == ".git" || strings.HasPrefix(name, ".git/") {
			skipped++
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return nil, 0, err
			}
		case tar.TypeReg:
			if total += h.Size; total > MaxTreeBytes {
				return nil, 0, fmt.Errorf("more than %d MiB", MaxTreeBytes>>20)
			}
			if d := path.Dir(name); d != "." {
				if err := root.MkdirAll(d, 0o700); err != nil {
					return nil, 0, err
				}
			}
			mode := os.FileMode(0o600)
			if h.Mode&0o100 != 0 {
				mode = 0o700
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return nil, 0, err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return nil, 0, err
			}
			files = append(files, name)
		default:
			// A symlink could name a path outside the tree, and a
			// submodule is an empty directory in an archive anyway.
			skipped++
		}
	}
}

func (m *Mirrors) command(ctx context.Context, dir string, auth []string, args ...string) *exec.Cmd {
	full := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always", "-c", "protocol.file.allow=always"}, args...)
	cmd := exec.CommandContext(ctx, m.bin(), full...)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + m.Root,
		"LANG=C.UTF-8",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_NO_REPLACE_OBJECTS=1",
	}, auth...)
	return cmd
}

// run runs git in dir and returns its stdout.
func (m *Mirrors) run(ctx context.Context, dir string, auth []string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := m.command(ctx, dir, auth, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %s", args[0], lastLine(strings.TrimSpace(errb.String()), err))
	}
	return out.String(), nil
}

func lastLine(msg string, err error) string {
	if msg == "" {
		return err.Error()
	}
	if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
		return msg[i+1:]
	}
	return msg
}
