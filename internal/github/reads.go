package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ReadPulls reads pull requests, their checks and commit statuses: the
// repository tools' GitHub reads (internal/repos), minted per call for one
// repository of github.read_repositories and revoked after. It writes
// nothing and reads no file contents.
var ReadPulls = Permissions{"pull_requests": "read", "checks": "read", "statuses": "read"}

// PullSummary is one pull request as a list shows it. MergedAt is set only
// for a merged one, so a closed PR is never mistaken for a merge.
type PullSummary struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	Draft     bool       `json:"draft"`
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	HTMLURL   string     `json:"html_url"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// PullDetail is one pull request in full, as GET pulls/{n} answers.
type PullDetail struct {
	PullSummary
	Body           string `json:"body"`
	Merged         bool   `json:"merged"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Additions      int    `json:"additions"`
	Deletions      int    `json:"deletions"`
	ChangedFiles   int    `json:"changed_files"`
	Commits        int    `json:"commits"`
}

// MaxPulls bounds one list: pages of 100 until limit, never more than this.
const MaxPulls = 300

// ListPulls lists repo's pull requests in state (open, closed or all),
// most recently updated first, every page up to limit, so a list is never
// silently cut at GitHub's page size.
func (c *Client) ListPulls(ctx context.Context, repo, state string, limit int) ([]PullSummary, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	switch state {
	case "":
		state = "open"
	case "open", "closed", "all":
	default:
		return nil, fmt.Errorf("state %q is not open, closed or all", state)
	}
	if limit <= 0 {
		limit = 30
	}
	limit = min(limit, MaxPulls)
	out := []PullSummary{}
	for page := 1; len(out) < limit; page++ {
		per := min(100, limit-len(out))
		q := url.Values{"state": {state}, "sort": {"updated"}, "direction": {"desc"},
			"per_page": {fmt.Sprint(per)}, "page": {fmt.Sprint(page)}}
		var prs []PullSummary
		if err := c.do(ctx, http.MethodGet, p+"/pulls?"+q.Encode(), nil, &prs); err != nil {
			return nil, err
		}
		out = append(out, prs...)
		if len(prs) < per {
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ViewPull reads one pull request in full.
func (c *Client) ViewPull(ctx context.Context, repo string, number int) (*PullDetail, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("%d is not a pull request number", number)
	}
	var pr PullDetail
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", p, number), nil, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}
