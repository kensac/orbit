package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// CommentPermissions posts a comment on an issue or a pull request (both are issues to GitHub's comments API), and nothing else: no contents, no merge, no labels beyond what a comment can do.
var CommentPermissions = Permissions{"issues": "write"}

// MaxCommentBody is the longest comment orbit posts; GitHub's own limit is 65536 characters.
const MaxCommentBody = 20000

// Comment is a posted comment.
type Comment struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// CreateComment posts body on issue or pull request number of repo, its attribution lines scrubbed (ScrubAttribution).
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) (*Comment, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("%d is not an issue or pull request number", number)
	}
	body = ScrubAttribution(body)
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("the comment is empty")
	}
	var out Comment
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", p, number), map[string]string{"body": body}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
