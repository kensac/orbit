package repos

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The history tools' bounds. A model reads every byte it is given, so an
// answer is at most MaxOutput (the head, with a note saying how much was
// cut), and a command at most toolTimeout.
const (
	MaxOutput     = 64 << 10
	DefaultLog    = 20
	MaxLog        = 200
	toolTimeout   = 30 * time.Second
	fieldSep      = "\x1f"
	maxPathLength = 1024
)

// LogEntry is one commit of git_log.
type LogEntry struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// checkRev refuses a revision git could read as an option. Anything else
// is git's to resolve (a branch, a tag, a sha, main~3, a PR's
// refs/pull/<n>/head), and it can only name objects in the mirror.
func checkRev(rev string) error {
	if rev == "" || strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, " \t\n\x00") || len(rev) > 256 {
		return fmt.Errorf("%q is not a revision", rev)
	}
	return nil
}

func checkPath(p string) error {
	if strings.HasPrefix(p, "-") || strings.ContainsRune(p, 0) || len(p) > maxPathLength {
		return fmt.Errorf("%q is not a path in the repository", p)
	}
	return nil
}

// Log lists commits reachable from rev (the default branch when empty),
// newest first, optionally only those touching path.
func (m *Mirrors) Log(ctx context.Context, repo, rev, path string, max int, since string) ([]LogEntry, error) {
	dir, err := m.Dir(repo)
	if err != nil {
		return nil, err
	}
	if rev == "" {
		rev = "HEAD"
	}
	if err := checkRev(rev); err != nil {
		return nil, err
	}
	if err := checkPath(path); err != nil {
		return nil, err
	}
	if max <= 0 {
		max = DefaultLog
	}
	max = min(max, MaxLog)
	args := []string{"log", "--no-color", "--format=%H" + fieldSep + "%an" + fieldSep + "%aI" + fieldSep + "%s", "-n", strconv.Itoa(max)}
	if since != "" {
		if _, err := time.Parse("2006-01-02", since); err != nil {
			return nil, fmt.Errorf("since %q is not YYYY-MM-DD", since)
		}
		args = append(args, "--since="+since)
	}
	args = append(args, "--end-of-options", rev, "--")
	if path != "" {
		args = append(args, path)
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	out, err := m.run(ctx, dir, nil, args...)
	if err != nil {
		return nil, err
	}
	entries := []LogEntry{}
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		f := strings.SplitN(line, fieldSep, 4)
		if len(f) == 4 {
			entries = append(entries, LogEntry{SHA: f[0], Author: f[1], Date: f[2], Subject: f[3]})
		}
	}
	return entries, nil
}

// Show is a commit (its message, the files it changed and its patch), or,
// with path, that file's content at rev.
func (m *Mirrors) Show(ctx context.Context, repo, rev, path string) (string, error) {
	dir, err := m.Dir(repo)
	if err != nil {
		return "", err
	}
	if err := checkRev(rev); err != nil {
		return "", err
	}
	if err := checkPath(path); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	args := []string{"show", "--no-color", "--no-ext-diff", "--no-textconv"}
	if path != "" {
		args = append(args, "--end-of-options", rev+":"+path)
	} else {
		args = append(args, "--stat", "--patch", "--format=fuller", "--end-of-options", rev)
	}
	return m.bounded(ctx, dir, args...)
}

// Diff is the patch from base to head (both revisions), optionally only
// path, or only the changed files and their line counts with stat.
func (m *Mirrors) Diff(ctx context.Context, repo, base, head, path string, stat bool) (string, error) {
	dir, err := m.Dir(repo)
	if err != nil {
		return "", err
	}
	for _, r := range []string{base, head} {
		if err := checkRev(r); err != nil {
			return "", err
		}
	}
	if err := checkPath(path); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	args := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv"}
	if stat {
		args = append(args, "--stat")
	}
	args = append(args, "--end-of-options", base, head, "--")
	if path != "" {
		args = append(args, path)
	}
	return m.bounded(ctx, dir, args...)
}

// Blame is path's lines at rev (the default branch when empty), each with
// the commit, author and date that last changed it; start and end (1-based,
// inclusive) narrow it.
func (m *Mirrors) Blame(ctx context.Context, repo, rev, path string, start, end int) (string, error) {
	dir, err := m.Dir(repo)
	if err != nil {
		return "", err
	}
	if rev == "" {
		rev = "HEAD"
	}
	if err := checkRev(rev); err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if err := checkPath(path); err != nil {
		return "", err
	}
	args := []string{"blame", "--date=short", "-l"}
	if start > 0 || end > 0 {
		if start <= 0 {
			start = 1
		}
		if end > 0 && end < start {
			return "", fmt.Errorf("end %d is before start %d", end, start)
		}
		l := strconv.Itoa(start) + ","
		if end > 0 {
			l += strconv.Itoa(end)
		}
		args = append(args, "-L", l)
	}
	args = append(args, rev, "--", path)
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	return m.bounded(ctx, dir, args...)
}

// bounded runs a read and keeps at most MaxOutput of its output.
func (m *Mirrors) bounded(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := m.run(ctx, dir, nil, args...)
	if err != nil {
		return "", err
	}
	return Clip(out, MaxOutput), nil
}

// Clip keeps the first max bytes of s, cut at a line end when one is near,
// with a note saying how much was left out.
func Clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := s[:max]
	if i := strings.LastIndexByte(head, '\n'); i > max/2 {
		head = head[:i+1]
	}
	return head + fmt.Sprintf("\n[orbit cut this answer at %d of its %d bytes: narrow it with a path or a range]\n", len(head), len(s))
}
