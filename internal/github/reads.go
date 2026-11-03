package github

import (
	"context"
	"encoding/json"
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

// The permission sets of an agent's github source (internal/sources), one per resource, each minted for one repository of github.read_repositories for one fetch and revoked after it.
var (
	// ReadIssues lists issues.
	ReadIssues = Permissions{"issues": "read"}
	// ReadPullList lists pull requests.
	ReadPullList = Permissions{"pull_requests": "read"}
	// ReadChecks reads a commit's check runs and statuses.
	ReadChecks = Permissions{"checks": "read", "statuses": "read"}
	// ReadReleases lists releases, which GitHub gates on contents.
	ReadReleases = Permissions{"contents": "read"}
)

// MaxListItems bounds one issue or release list: pages of 100 until limit, never more than this.
const MaxListItems = 1000

// maxBody is how much of an issue's or a release's body a list keeps; the rest is cut and marked.
const maxBody = 4000

// Issue is one issue as a list shows it. Pull requests, which GitHub's issue list includes, are left out.
type Issue struct {
	Number        int        `json:"number"`
	Title         string     `json:"title"`
	State         string     `json:"state"`
	Author        string     `json:"author"`
	Labels        []string   `json:"labels"`
	Assignees     []string   `json:"assignees"`
	Comments      int        `json:"comments"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ClosedAt      *time.Time `json:"closed_at"`
	HTMLURL       string     `json:"html_url"`
	Body          string     `json:"body"`
	BodyTruncated bool       `json:"body_truncated,omitempty"`
}

// Release is one release as a list shows it.
type Release struct {
	TagName       string     `json:"tag_name"`
	Name          string     `json:"name"`
	Draft         bool       `json:"draft"`
	Prerelease    bool       `json:"prerelease"`
	Author        string     `json:"author"`
	CreatedAt     time.Time  `json:"created_at"`
	PublishedAt   *time.Time `json:"published_at"`
	HTMLURL       string     `json:"html_url"`
	Body          string     `json:"body"`
	BodyTruncated bool       `json:"body_truncated,omitempty"`
}

func clipBody(s string) (string, bool) {
	if len(s) <= maxBody {
		return s, false
	}
	cut := maxBody
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut-- // not inside a UTF-8 sequence
	}
	return s[:cut], true
}

func listLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	return min(limit, MaxListItems)
}

// ListIssues lists repo's issues in state (open, closed or all), most recently updated first, every page up to limit; pull requests are left out.
func (c *Client) ListIssues(ctx context.Context, repo, state string, limit int) ([]Issue, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	if state == "" {
		state = "open"
	}
	if state != "open" && state != "closed" && state != "all" {
		return nil, fmt.Errorf("state %q is not open, closed or all", state)
	}
	limit = listLimit(limit)
	out := []Issue{}
	for page := 1; len(out) < limit && page <= MaxListItems/100+1; page++ {
		q := url.Values{"state": {state}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		var items []struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			State  string `json:"state"`
			User   struct {
				Login string `json:"login"`
			} `json:"user"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
			Assignees []struct {
				Login string `json:"login"`
			} `json:"assignees"`
			Comments    int             `json:"comments"`
			CreatedAt   time.Time       `json:"created_at"`
			UpdatedAt   time.Time       `json:"updated_at"`
			ClosedAt    *time.Time      `json:"closed_at"`
			HTMLURL     string          `json:"html_url"`
			Body        string          `json:"body"`
			PullRequest json.RawMessage `json:"pull_request"`
		}
		if err := c.do(ctx, http.MethodGet, p+"/issues?"+q.Encode(), nil, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			if len(it.PullRequest) > 0 && string(it.PullRequest) != "null" {
				continue
			}
			is := Issue{Number: it.Number, Title: it.Title, State: it.State, Author: it.User.Login, Labels: []string{}, Assignees: []string{},
				Comments: it.Comments, CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt, ClosedAt: it.ClosedAt, HTMLURL: it.HTMLURL}
			is.Body, is.BodyTruncated = clipBody(it.Body)
			for _, l := range it.Labels {
				is.Labels = append(is.Labels, l.Name)
			}
			for _, a := range it.Assignees {
				is.Assignees = append(is.Assignees, a.Login)
			}
			out = append(out, is)
		}
		if len(items) < 100 {
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListReleases lists repo's releases, newest first, every page up to limit.
func (c *Client) ListReleases(ctx context.Context, repo string, limit int) ([]Release, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	limit = listLimit(limit)
	out := []Release{}
	for page := 1; len(out) < limit; page++ {
		per := min(100, limit-len(out))
		q := url.Values{"per_page": {fmt.Sprint(per)}, "page": {fmt.Sprint(page)}}
		var items []struct {
			TagName    string `json:"tag_name"`
			Name       string `json:"name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
			Author     struct {
				Login string `json:"login"`
			} `json:"author"`
			CreatedAt   time.Time  `json:"created_at"`
			PublishedAt *time.Time `json:"published_at"`
			HTMLURL     string     `json:"html_url"`
			Body        string     `json:"body"`
		}
		if err := c.do(ctx, http.MethodGet, p+"/releases?"+q.Encode(), nil, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			r := Release{TagName: it.TagName, Name: it.Name, Draft: it.Draft, Prerelease: it.Prerelease, Author: it.Author.Login,
				CreatedAt: it.CreatedAt, PublishedAt: it.PublishedAt, HTMLURL: it.HTMLURL}
			r.Body, r.BodyTruncated = clipBody(it.Body)
			out = append(out, r)
		}
		if len(items) < per {
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
