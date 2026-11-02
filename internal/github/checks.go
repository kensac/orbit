package github

import (
	"context"
	"fmt"
	"net/http"
)

// RerunPermissions reruns failed Actions jobs: the only use of actions
// write. It also covers dispatching and cancelling workflows, which no
// code here does; the token is minted for the rerun call and revoked after.
var RerunPermissions = Permissions{"actions": "write"}

// RerunJob asks GitHub to run one Actions job again, on the same commit and
// workflow file. The new attempt replaces the failed one as the latest
// check run of that name, so the gate reads it from then on.
func (c *Client) RerunJob(ctx context.Context, repo string, jobID int64) error {
	p, err := repoPath(repo)
	if err != nil {
		return err
	}
	if jobID <= 0 {
		return fmt.Errorf("job id %d is not an Actions job", jobID)
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("%s/actions/jobs/%d/rerun", p, jobID), map[string]any{}, nil)
}
