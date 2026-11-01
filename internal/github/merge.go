package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// BranchPrefix is the only head branch orbit merges: its own PRs.
const BranchPrefix = "orbit/"

// PullRequest is the part of a PR the merge gate reads.
type PullRequest struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	Merged         bool   `json:"merged"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repo"`
	} `json:"base"`
}

// RequiredCheck is one status check a branch requires. AppID > 0 means only
// a check run from that app satisfies it.
type RequiredCheck struct {
	Context string
	AppID   int64
}

// CheckRun is one check run on a commit.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

// Status is one commit status (the older statuses API, still used by some
// integrations).
type Status struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

func repoPath(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return "", fmt.Errorf("%q is not owner/name", repo)
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// PR reads a pull request.
func (c *Client) PR(ctx context.Context, repo string, number int) (*PullRequest, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var pr PullRequest
	return &pr, c.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", p, number), nil, &pr)
}

// RequiredChecks returns the checks branch requires, from classic branch
// protection and from rulesets together. A branch with neither returns none.
func (c *Client) RequiredChecks(ctx context.Context, repo, branch string) ([]RequiredCheck, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	seen := map[RequiredCheck]bool{}
	var out []RequiredCheck
	add := func(r RequiredCheck) {
		if r.Context != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	var b struct {
		Protection struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
				Checks   []struct {
					Context string `json:"context"`
					AppID   *int64 `json:"app_id"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if err := c.do(ctx, http.MethodGet, p+"/branches/"+url.PathEscape(branch), nil, &b); err != nil {
		return nil, err
	}
	rsc := b.Protection.RequiredStatusChecks
	if len(rsc.Checks) > 0 {
		for _, ch := range rsc.Checks {
			r := RequiredCheck{Context: ch.Context}
			if ch.AppID != nil && *ch.AppID > 0 {
				r.AppID = *ch.AppID
			}
			add(r)
		}
	} else {
		for _, ctxName := range rsc.Contexts {
			add(RequiredCheck{Context: ctxName})
		}
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context       string `json:"context"`
				IntegrationID int64  `json:"integration_id"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	err = c.do(ctx, http.MethodGet, p+"/rules/branches/"+url.PathEscape(branch), nil, &rules)
	if err != nil && !IsStatus(err, http.StatusNotFound) {
		return nil, err
	}
	for _, r := range rules {
		if r.Type != "required_status_checks" {
			continue
		}
		for _, ch := range r.Parameters.RequiredStatusChecks {
			add(RequiredCheck{Context: ch.Context, AppID: ch.IntegrationID})
		}
	}
	return out, nil
}

// maxCheckPages bounds pagination: 1000 check runs on one commit is far
// beyond any repository orbit merges, and a loop must end.
const maxCheckPages = 10

// CheckRuns returns the latest check run of each check on sha.
func (c *Client) CheckRuns(ctx context.Context, repo, sha string) ([]CheckRun, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var all []CheckRun
	for page := 1; page <= maxCheckPages; page++ {
		var r struct {
			TotalCount int        `json:"total_count"`
			CheckRuns  []CheckRun `json:"check_runs"`
		}
		path := fmt.Sprintf("%s/commits/%s/check-runs?filter=latest&per_page=100&page=%d", p, url.PathEscape(sha), page)
		if err := c.do(ctx, http.MethodGet, path, nil, &r); err != nil {
			return nil, err
		}
		all = append(all, r.CheckRuns...)
		if len(r.CheckRuns) == 0 || len(all) >= r.TotalCount {
			return all, nil
		}
	}
	return nil, fmt.Errorf("more than %d pages of check runs on %s", maxCheckPages, sha)
}

// Statuses returns the latest commit status of each context on sha.
func (c *Client) Statuses(ctx context.Context, repo, sha string) ([]Status, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var r struct {
		Statuses []Status `json:"statuses"`
	}
	err = c.do(ctx, http.MethodGet, fmt.Sprintf("%s/commits/%s/status?per_page=100", p, url.PathEscape(sha)), nil, &r)
	return r.Statuses, err
}

// Gate decides whether a commit's checks allow a merge. With required
// checks, each must have reported and concluded success. With none, every
// check run and every status must have, and there must be at least one: a
// merge with no check at all is a deploy with no gate. It returns nil or an
// error naming every check that is not green.
func Gate(required []RequiredCheck, runs []CheckRun, statuses []Status) error {
	var problems []string
	green := func(r CheckRun) bool { return r.Status == "completed" && r.Conclusion == "success" }
	describe := func(r CheckRun) string {
		if r.Status != "completed" {
			return fmt.Sprintf("%s is %s", r.Name, r.Status)
		}
		return fmt.Sprintf("%s concluded %s", r.Name, r.Conclusion)
	}
	if len(required) > 0 {
		for _, req := range required {
			var matched []CheckRun
			for _, r := range runs {
				if r.Name == req.Context && (req.AppID <= 0 || r.App.ID == req.AppID) {
					matched = append(matched, r)
				}
			}
			var st *Status
			if req.AppID <= 0 {
				for i := range statuses {
					if statuses[i].Context == req.Context {
						st = &statuses[i]
					}
				}
			}
			switch {
			case len(matched) > 0:
				for _, r := range matched {
					if !green(r) {
						problems = append(problems, "required "+describe(r))
					}
				}
			case st != nil:
				if st.State != "success" {
					problems = append(problems, fmt.Sprintf("required %s is %s", st.Context, st.State))
				}
			default:
				problems = append(problems, fmt.Sprintf("required %s has not reported", req.Context))
			}
		}
	} else {
		if len(runs)+len(statuses) == 0 {
			return errors.New("no checks ran on the head commit, and a merge is a deploy")
		}
		for _, r := range runs {
			if !green(r) {
				problems = append(problems, describe(r))
			}
		}
		for _, s := range statuses {
			if s.State != "success" {
				problems = append(problems, fmt.Sprintf("%s is %s", s.Context, s.State))
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("checks are not green: %s", strings.Join(problems, "; "))
}

// attribution matches a line that credits an AI tool for the work: a
// co-author trailer for one, a "generated with/by" footer, or the robot
// marker such footers carry. A line that merely mentions a tool by name
// (orbit has a runner kind named after one) is kept.
var (
	aiName        = `(claude|anthropic|openai|chatgpt|gpt-?\d|codex|copilot|gemini|cursor|devin|aider)`
	coAuthorAI    = regexp.MustCompile(`(?i)^\s*co-authored-by:.*` + aiName)
	generatedWith = regexp.MustCompile(`(?i)\b(generated|written|created|authored|made)\s+(with|by|using)\b.*` + aiName)
	robotMarker   = regexp.MustCompile("\U0001F916")
	blankRuns     = regexp.MustCompile(`\n{3,}`)
)

// ScrubAttribution removes attribution lines from a PR body. A squash merge
// makes the body the commit message, and nothing in the fleet's history
// names an AI assistant (docs/agents/working-agreement.md).
func ScrubAttribution(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if coAuthorAI.MatchString(l) || generatedWith.MatchString(l) || robotMarker.MatchString(l) {
			continue
		}
		kept = append(kept, l)
	}
	return strings.TrimSpace(blankRuns.ReplaceAllString(strings.Join(kept, "\n"), "\n\n"))
}

// MergeRequest names the PR to merge and the commit that was checked.
type MergeRequest struct {
	Repo    string
	Number  int
	HeadSHA string
}

// MergeOptions tunes the wait for GitHub to compute mergeability.
type MergeOptions struct {
	// MergeableTries and MergeableWait bound the re-reads while GitHub
	// reports mergeable as null (it computes it in the background after a
	// push). Zero values mean 5 tries 2 s apart.
	MergeableTries int
	MergeableWait  time.Duration
}

// Merge squash-merges an orbit PR when, and only when, every gate holds at
// this moment: it is orbit's own open PR on the default branch, its head is
// still the checked commit, GitHub says it is mergeable, and its checks are
// green. The body is scrubbed and written back before the merge, and the
// merge pins the head sha, so a push in between makes GitHub refuse. It
// returns the merge commit's sha.
func Merge(ctx context.Context, c *Client, m MergeRequest, opt MergeOptions) (string, error) {
	if opt.MergeableTries <= 0 {
		opt.MergeableTries = 5
	}
	if opt.MergeableWait <= 0 {
		opt.MergeableWait = 2 * time.Second
	}
	p, err := repoPath(m.Repo)
	if err != nil {
		return "", err
	}
	if m.HeadSHA == "" {
		return "", errors.New("head_sha is required: an approval is for one commit")
	}
	var pr *PullRequest
	for try := 1; ; try++ {
		if pr, err = c.PR(ctx, m.Repo, m.Number); err != nil {
			return "", err
		}
		if pr.Mergeable != nil || pr.State != "open" || try >= opt.MergeableTries {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(opt.MergeableWait):
		}
	}
	switch {
	case pr.Merged:
		return "", fmt.Errorf("PR #%d is already merged", m.Number)
	case pr.State != "open":
		return "", fmt.Errorf("PR #%d is %s", m.Number, pr.State)
	case pr.Draft:
		return "", fmt.Errorf("PR #%d is a draft", m.Number)
	case pr.Base.Ref != pr.Base.Repo.DefaultBranch:
		return "", fmt.Errorf("PR #%d targets %s, not the default branch %s", m.Number, pr.Base.Ref, pr.Base.Repo.DefaultBranch)
	case pr.Head.Repo == nil || pr.Head.Repo.FullName != pr.Base.Repo.FullName:
		return "", fmt.Errorf("PR #%d comes from another repository; orbit merges only its own branches", m.Number)
	case !strings.HasPrefix(pr.Head.Ref, BranchPrefix):
		return "", fmt.Errorf("PR #%d's branch %s is not an %s branch; orbit merges only its own PRs", m.Number, pr.Head.Ref, BranchPrefix)
	case pr.Head.SHA != m.HeadSHA:
		return "", fmt.Errorf("PR #%d's head is %s, not the approved %s: something was pushed after", m.Number, short(pr.Head.SHA), short(m.HeadSHA))
	case pr.Mergeable == nil:
		return "", fmt.Errorf("GitHub has not finished computing whether PR #%d is mergeable", m.Number)
	case !*pr.Mergeable:
		return "", fmt.Errorf("PR #%d is not mergeable (%s)", m.Number, pr.MergeableState)
	}
	switch pr.MergeableState {
	case "dirty", "blocked", "behind", "draft", "unknown":
		return "", fmt.Errorf("PR #%d is not ready to merge: mergeable_state %s", m.Number, pr.MergeableState)
	}
	required, err := c.RequiredChecks(ctx, m.Repo, pr.Base.Ref)
	if err != nil {
		return "", fmt.Errorf("read required checks: %w", err)
	}
	runs, err := c.CheckRuns(ctx, m.Repo, m.HeadSHA)
	if err != nil {
		return "", fmt.Errorf("read check runs: %w", err)
	}
	statuses, err := c.Statuses(ctx, m.Repo, m.HeadSHA)
	if err != nil {
		return "", fmt.Errorf("read commit statuses: %w", err)
	}
	if err := Gate(required, runs, statuses); err != nil {
		return "", err
	}
	body := ScrubAttribution(pr.Body)
	if body != strings.TrimSpace(pr.Body) {
		if err := c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", p, m.Number), map[string]string{"body": body}, nil); err != nil {
			return "", fmt.Errorf("scrub the PR body: %w", err)
		}
	}
	var res struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	err = c.do(ctx, http.MethodPut, fmt.Sprintf("%s/pulls/%d/merge", p, m.Number), map[string]string{
		"merge_method":   "squash",
		"sha":            m.HeadSHA,
		"commit_title":   fmt.Sprintf("%s (#%d)", pr.Title, m.Number),
		"commit_message": body,
	}, &res)
	if err != nil {
		return "", fmt.Errorf("merge: %w", err)
	}
	if !res.Merged {
		return "", fmt.Errorf("merge: GitHub did not merge PR #%d", m.Number)
	}
	return res.SHA, nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
