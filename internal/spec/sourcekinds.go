package spec

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// GitHubResources are what a github source reads: a repository's issues, pull requests, a commit's checks, or its releases.
var GitHubResources = []string{"issues", "pulls", "checks", "releases"}

// MaxGitHubLimit is the most items a github source lists, and MaxGitHubPulls the most pull requests (github.MaxPulls).
const (
	MaxGitHubLimit = 1000
	MaxGitHubPulls = 300
)

// MaxLokiSince is the furthest back a loki source reads, and MaxLokiLimit the most lines it asks for: a day of one service's logs is already far past what a model should read raw.
const (
	MaxLokiSince = 24 * time.Hour
	MaxLokiLimit = 5000
)

// DefaultLokiSince and DefaultLokiLimit apply when a loki source sets neither.
const (
	DefaultLokiSince = time.Hour
	DefaultLokiLimit = 1000
)

func (c *Config) validateGitHubSource(a *Agent, s Source) []error {
	var errs []error
	at := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agent %q: source %q: "+format, append([]any{a.Name, s.Name}, args...)...))
	}
	if s.URL != "" || len(s.Headers) > 0 || len(s.Query) > 0 || s.Paginate != nil || s.Expr != "" || s.Path != "" {
		at("a github source takes repo, resource, ref, state and limit, not url, headers, query, paginate, expr or path")
	}
	switch {
	case !RepoRE.MatchString(s.Repo) || strings.Contains(s.Repo, ".."):
		at("repo %q is not owner/name", s.Repo)
	case !c.Deployment.AllowsRead(s.Repo):
		at("%s is not in github.read_repositories", s.Repo)
	}
	if !slices.Contains(GitHubResources, s.Resource) {
		at("resource %q is not one of %s", s.Resource, strings.Join(GitHubResources, ", "))
	}
	switch s.Resource {
	case "checks":
		if s.Ref == "" || !RepoRefRE.MatchString(s.Ref) || strings.Contains(s.Ref, "..") {
			at("checks needs ref, a branch, tag or commit sha")
		}
		if s.State != "" || s.Limit != 0 {
			at("checks takes no state or limit")
		}
	case "issues", "pulls":
		if s.Ref != "" {
			at("%s takes no ref", s.Resource)
		}
		if s.State != "" && !slices.Contains([]string{"open", "closed", "all"}, s.State) {
			at("state must be open, closed or all")
		}
	case "releases":
		if s.Ref != "" || s.State != "" {
			at("releases takes no ref or state")
		}
	}
	if s.Limit < 0 || s.Limit > MaxGitHubLimit || (s.Resource == "pulls" && s.Limit > MaxGitHubPulls) {
		at("limit is at most %d (%d for pulls)", MaxGitHubLimit, MaxGitHubPulls)
	}
	if !slices.Contains(a.Needs, NeedsGitHub) {
		at("needs %q in needs, so only a host holding the GitHub App's key claims its runs", NeedsGitHub)
	}
	return errs
}
