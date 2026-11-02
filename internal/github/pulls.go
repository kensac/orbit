package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// More permission sets, one per use (docs/tickets.md, GitHub access).
var (
	// WatchPermissions reads a PR, its checks and its commit statuses, and
	// compares commits: the ticket loop's reads.
	WatchPermissions = Permissions{"contents": "read", "pull_requests": "read", "checks": "read", "statuses": "read"}
	// FailureReport reads failing checks and their Actions job logs.
	FailureReport = Permissions{"checks": "read", "actions": "read"}
)

// NewPR is a pull request to open.
type NewPR struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
}

// OpenPR returns the open PR whose head is branch in repo itself, or nil.
func (c *Client) OpenPR(ctx context.Context, repo, branch string) (*PullRequest, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	owner, _, _ := strings.Cut(repo, "/")
	var prs []PullRequest
	q := url.Values{"head": {owner + ":" + branch}, "state": {"open"}, "per_page": {"10"}}
	if err := c.do(ctx, http.MethodGet, p+"/pulls?"+q.Encode(), nil, &prs); err != nil {
		return nil, err
	}
	for i := range prs {
		if prs[i].Head.Ref == branch && prs[i].State == "open" {
			return &prs[i], nil
		}
	}
	return nil, nil
}

// CreatePR opens a PR. When one is already open for the head (orbit opened
// it before a crash, or another orbit process did), it returns that one:
// a branch has at most one open PR, which is what retries rely on.
func (c *Client) CreatePR(ctx context.Context, repo string, n NewPR) (*PullRequest, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var pr PullRequest
	err = c.do(ctx, http.MethodPost, p+"/pulls", n, &pr)
	if IsStatus(err, http.StatusUnprocessableEntity) {
		if existing, ferr := c.OpenPR(ctx, repo, n.Head); ferr == nil && existing != nil {
			return existing, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return &pr, nil
}

// UpdatePR rewrites a PR's title and body.
func (c *Client) UpdatePR(ctx context.Context, repo string, number int, title, body string) error {
	p, err := repoPath(repo)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", p, number), map[string]string{"title": title, "body": body}, nil)
}

// ClosePR closes a PR without merging it. Its branch is kept.
func (c *Client) ClosePR(ctx context.Context, repo string, number int) error {
	p, err := repoPath(repo)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", p, number), map[string]string{"state": "closed"}, nil)
}

// Compare reports how head relates to base: "identical", "ahead" (head
// descends from base), "behind" or "diverged", and the files that differ
// (GitHub lists at most 300).
func (c *Client) Compare(ctx context.Context, repo, base, head string) (string, []string, error) {
	p, err := repoPath(repo)
	if err != nil {
		return "", nil, err
	}
	var r struct {
		Status string `json:"status"`
		Files  []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	path := fmt.Sprintf("%s/compare/%s...%s", p, url.PathEscape(base), url.PathEscape(head))
	if err := c.do(ctx, http.MethodGet, path, nil, &r); err != nil {
		return "", nil, err
	}
	files := make([]string, 0, len(r.Files))
	for _, f := range r.Files {
		files = append(files, f.Filename)
	}
	return r.Status, files, nil
}

// JobLogTail returns the last max bytes of an Actions job's log. GitHub
// answers with a redirect to the log's storage (a presigned URL), which is
// followed here by hand, with no credential: the token never leaves for
// another host, whatever the HTTP client's redirect rules. The whole log is
// streamed and only its tail kept, because the failure is at the end.
func (c *Client) JobLogTail(ctx context.Context, repo string, jobID int64, max int) (string, error) {
	p, err := repoPath(repo)
	if err != nil {
		return "", err
	}
	noFollow := *c.http
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	nc := &Client{base: c.base, auth: c.auth, http: &noFollow}
	path := fmt.Sprintf("%s/actions/jobs/%d/logs", p, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nc.base+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", nc.auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "orbit")
	resp, err := nc.http.Do(req)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || loc == "" {
		return "", &APIError{Method: http.MethodGet, Path: path, Status: resp.StatusCode, Message: "no redirect to the log"}
	}
	lreq, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return "", err
	}
	lresp, err := noFollow.Do(lreq)
	if err != nil {
		return "", err
	}
	defer lresp.Body.Close()
	if lresp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("job log: %s", lresp.Status)
	}
	t := &tail{max: max}
	if _, err := io.Copy(t, io.LimitReader(lresp.Body, maxJobLog)); err != nil {
		return "", err
	}
	return string(t.b), nil
}

// maxJobLog bounds how much of one job log is read: GitHub keeps a job's
// log up to 64 MB, and a tail of a log past this is not worth the transfer.
const maxJobLog = 64 << 20

// tail keeps the last max bytes written to it.
type tail struct {
	max int
	b   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append(t.b[:0], t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

// CheckState is where a commit's checks stand.
type CheckState int

// The states Classify reports.
const (
	// ChecksPending: nothing has failed, and not every check the gate
	// needs has succeeded yet.
	ChecksPending CheckState = iota
	// ChecksGreen: Gate passes.
	ChecksGreen
	// ChecksFailed: a check the gate needs concluded in a way that can no
	// longer pass.
	ChecksFailed
)

// Failing is one check that failed, as a fix attempt and a person see it.
type Failing struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url,omitempty"`
	// JobID is the Actions job behind a check run from github-actions, 0
	// otherwise (a commit status, another app).
	JobID int64 `json:"job_id,omitempty"`
}

// Classify says whether a commit's checks are green (Gate passes), failed
// (a check the gate needs concluded anything that is not a pass, so waiting
// cannot help), or still pending, and lists what failed. It reads the same
// rule Gate does: with required checks only they count, and a required
// check that is skipped or neutral has failed; with none, every check run
// and status counts.
func Classify(required []RequiredCheck, runs []CheckRun, statuses []Status) (CheckState, []Failing) {
	if Gate(required, runs, statuses) == nil {
		return ChecksGreen, nil
	}
	var failing []Failing
	run := func(r CheckRun, requiredCheck bool) {
		if r.Status != "completed" {
			return
		}
		pass := r.Conclusion == "success" || (!requiredCheck && unrequiredPass[r.Conclusion])
		if !pass {
			f := Failing{Name: r.Name, Conclusion: r.Conclusion, URL: r.HTMLURL}
			if r.App.Slug == "github-actions" {
				f.JobID = r.ID
			}
			failing = append(failing, f)
		}
	}
	status := func(s Status) {
		if s.State == "failure" || s.State == "error" {
			failing = append(failing, Failing{Name: s.Context, Conclusion: s.State, URL: s.TargetURL})
		}
	}
	if len(required) > 0 {
		for _, req := range required {
			matched := false
			for _, r := range runs {
				if r.Name == req.Context && (req.AppID <= 0 || r.App.ID == req.AppID) {
					matched = true
					run(r, true)
				}
			}
			if !matched && req.AppID <= 0 {
				for _, s := range statuses {
					if s.Context == req.Context {
						status(s)
					}
				}
			}
		}
	} else {
		for _, r := range runs {
			run(r, false)
		}
		for _, s := range statuses {
			status(s)
		}
	}
	if len(failing) > 0 {
		return ChecksFailed, failing
	}
	return ChecksPending, nil
}
