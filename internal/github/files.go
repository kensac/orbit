package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// ReadPullDiff reads a pull request and its files' patches: a ticket's
// diff (GET /tickets/{id}/diff), minted per request for the ticket's
// repository and revoked after.
var ReadPullDiff = Permissions{"contents": "read", "pull_requests": "read"}

// PullFile is one file of a pull request as GitHub lists it. Patch is the
// file's unified hunks, absent for a binary file and for one too large for
// GitHub to show.
type PullFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Changes          int    `json:"changes"`
	Patch            string `json:"patch"`
}

// MaxPullFiles is the most files GitHub lists for a pull request.
const MaxPullFiles = 3000

// PullFiles lists a pull request's files, pages of 100 up to limit (at most
// MaxPullFiles), so a list is never silently cut at a page.
func (c *Client) PullFiles(ctx context.Context, repo string, number, limit int) ([]PullFile, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("%d is not a pull request number", number)
	}
	limit = min(max(limit, 1), MaxPullFiles)
	out := []PullFile{}
	for page := 1; len(out) < limit; page++ {
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		var batch []PullFile
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d/files?%s", p, number, q.Encode()), nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
