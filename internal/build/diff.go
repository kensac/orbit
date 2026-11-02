package build

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxFileBytes bounds one added or changed file: source code, not an
// artifact a build produced by accident.
const MaxFileBytes = 1 << 20

// maxPatch bounds the stored diff. MaxDiffLines already bounds a valid one
// far below this; a larger patch is binary or generated.
const maxPatch = 8 << 20

// ErrPolicy marks a build whose result the diff policy refused: nothing was
// committed.
var ErrPolicy = errors.New("diff policy")

func policyErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPolicy, fmt.Sprintf(format, args...))
}

// packTree tars dir for the sandbox, .git included so the model can read
// history and diff. The clone holds no credential: tokens live only in git's
// environment, never in .git/config.
func packTree(dir string) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		h := &tar.Header{Name: name, ModTime: info.ModTime(), Mode: int64(info.Mode().Perm())}
		switch {
		case d.IsDir():
			h.Typeflag, h.Name = tar.TypeDir, name+"/"
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			h.Typeflag, h.Linkname = tar.TypeSymlink, link
		case info.Mode().IsRegular():
			h.Typeflag, h.Size = tar.TypeReg, info.Size()
		default:
			return nil
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &buf, tw.Close()
}

// applyTree replaces the clone's working tree with the sandbox's: every
// file but .git is removed, then the archive is unpacked by this code (no
// tar tool, so no flag or extension to interpret). Everything named .git,
// at any depth, is dropped: the model's git metadata, hooks and refs never
// reach orbit's clone. A symlink must stay inside the tree.
func applyTree(dir string, archive io.Reader) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != ".git" {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	tr := tar.NewReader(archive)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the sandbox's tree: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return policyErr("the tree names %q, outside the checkout", h.Name)
		}
		if slices.Contains(strings.Split(name, "/"), ".git") {
			continue
		}
		if err := noSymlinkParent(dir, name); err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			target := path.Clean(path.Join(path.Dir(name), h.Linkname))
			if path.IsAbs(h.Linkname) || target == ".." || strings.HasPrefix(target, "../") {
				return policyErr("symlink %s points outside the repository (%s)", name, h.Linkname)
			}
			if slices.Contains(strings.Split(target, "/"), ".git") {
				return policyErr("symlink %s points into .git (%s)", name, h.Linkname)
			}
			if err := os.Symlink(h.Linkname, dst); err != nil {
				return err
			}
		default:
			return policyErr("%s is a %s, not a file, directory or symlink", name, typeName(h.Typeflag))
		}
	}
}

func typeName(t byte) string {
	switch t {
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "device"
	case tar.TypeFifo:
		return "FIFO"
	}
	return "tar entry of type " + strconv.Itoa(int(t))
}

// noSymlinkParent refuses an entry whose parent directories include a
// symlink: written through one, a file lands wherever the link points
// (.git/config, which can make git run a program, is the one that matters).
func noSymlinkParent(dir, name string) error {
	parts := strings.Split(name, "/")
	cur := dir
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return policyErr("%s is written through the symlink %s", name, p)
		}
	}
	return nil
}

// Diff is what a build changed, as orbit computed it.
type Diff struct {
	Files   []FileChange `json:"files"`
	Added   int          `json:"added"`
	Deleted int          `json:"deleted"`
	// Patch is the full diff, stored as the run's diff.patch artifact.
	Patch []byte `json:"-"`
}

// FileChange is one changed path.
type FileChange struct {
	Status string `json:"status"` // A, M, D, T
	Path   string `json:"path"`
	Mode   string `json:"mode,omitempty"`
}

// Paths lists the changed paths.
func (d Diff) Paths() []string {
	out := make([]string, 0, len(d.Files))
	for _, f := range d.Files {
		out = append(out, f.Path)
	}
	return out
}

// secretPatterns are credential shapes no commit should carry, whoever's
// they are. The backend credential itself is matched exactly as well.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{30,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
}

// DiffPolicy is what a build's result must satisfy before orbit commits it.
type DiffPolicy struct {
	Protected    []string
	MaxDiffLines int
	// Secrets are exact values that must not appear: the backend
	// credential the sandbox held.
	Secrets []string
}

// check stages the clone's working tree and enforces the policy on the
// staged diff against HEAD: something changed; no protected path, .git
// path or submodule; no symlink out of the tree (applyTree); no file over
// MaxFileBytes; at most MaxDiffLines; no secret in an added line.
func (p DiffPolicy) check(ctx context.Context, g git) (*Diff, error) {
	if _, err := g.run(ctx, nil, "add", "--all", "--", "."); err != nil {
		return nil, err
	}
	raw, err := g.run(ctx, nil, "diff", "--cached", "--raw", "-z", "--no-renames", "HEAD")
	if err != nil {
		return nil, err
	}
	d := &Diff{}
	fields := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		// :<old mode> <new mode> <old sha> <new sha> <status>
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 {
			return nil, fmt.Errorf("unexpected git diff --raw line %q", fields[i])
		}
		d.Files = append(d.Files, FileChange{Status: meta[4], Path: fields[i+1], Mode: meta[1]})
		if meta[1] == "160000" {
			return nil, policyErr("%s would be a submodule", fields[i+1])
		}
		if meta[4] != "D" && meta[1] != "120000" && meta[1] != "000000" {
			size, err := g.run(ctx, nil, "cat-file", "-s", meta[3])
			if err != nil {
				return nil, err
			}
			if n, _ := strconv.Atoi(strings.TrimSpace(size)); n > MaxFileBytes {
				return nil, policyErr("%s is %d bytes, over the %d-byte limit for a changed file", fields[i+1], n, MaxFileBytes)
			}
		}
	}
	if len(d.Files) == 0 {
		return nil, policyErr("the build changed nothing")
	}
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Path < d.Files[j].Path })
	for _, f := range d.Files {
		for _, gl := range append(append([]string{}, AlwaysProtected...), p.Protected...) {
			if MatchGlob(gl, f.Path) {
				return nil, policyErr("%s is a protected path (%s)", f.Path, gl)
			}
		}
	}
	num, err := g.run(ctx, nil, "diff", "--cached", "--numstat", "--no-renames", "HEAD")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(num), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		a, _ := strconv.Atoi(f[0])
		del, _ := strconv.Atoi(f[1])
		d.Added += a
		d.Deleted += del
	}
	if p.MaxDiffLines > 0 && d.Added+d.Deleted > p.MaxDiffLines {
		return nil, policyErr("the diff is %d lines (%d added, %d deleted), over the limit of %d", d.Added+d.Deleted, d.Added, d.Deleted, p.MaxDiffLines)
	}
	patch, err := g.run(ctx, nil, "diff", "--cached", "--no-renames", "--binary", "HEAD")
	if err != nil {
		return nil, err
	}
	if len(patch) > maxPatch {
		return nil, policyErr("the diff is %d bytes, over %d", len(patch), maxPatch)
	}
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		for _, s := range p.Secrets {
			if s != "" && strings.Contains(line, s) {
				// Never echo the value: the error reaches notifications.
				return nil, policyErr("an added line contains the backend credential the sandbox held")
			}
		}
		for _, re := range secretPatterns {
			if re.MatchString(line) {
				return nil, policyErr("an added line looks like a credential (%s)", re.String())
			}
		}
	}
	d.Patch = []byte(patch)
	return d, nil
}

// commit records the staged tree as one commit by the App's bot user. The
// message is the PR's title and body and a trailer naming the run, which is
// how a retried attempt recognizes a branch it already pushed and reopens
// its PR from the commit alone.
func commit(ctx context.Context, g git, author Identity, title, body, runID string) (string, error) {
	when := time.Now().UTC().Format(time.RFC3339)
	env := []string{
		"GIT_AUTHOR_NAME=" + author.Name, "GIT_AUTHOR_EMAIL=" + author.Email, "GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_NAME=" + author.Name, "GIT_COMMITTER_EMAIL=" + author.Email, "GIT_COMMITTER_DATE=" + when,
	}
	msg := title + "\n\n"
	if strings.TrimSpace(body) != "" {
		msg += strings.TrimSpace(body) + "\n\n"
	}
	msg += runTrailer + runID + "\n"
	if _, err := g.run(ctx, env, "commit", "--quiet", "--no-verify", "--no-gpg-sign", "-m", msg); err != nil {
		return "", err
	}
	sha, err := g.run(ctx, nil, "rev-parse", "HEAD")
	return strings.TrimSpace(sha), err
}

// runTrailer names the build run in its commit.
const runTrailer = "Orbit-Run: "
