package spec

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// GitHubResources are what a github source reads: a repository's issues, pull requests, a commit's checks, or its releases.
var GitHubResources = []string{"issues", "pulls", "checks", "releases"}

var lakeBucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
var lakeRegionRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

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

// MaxLakeObjects bounds object listings before any object body is read.
const (
	MaxLakeObjects     = 1000
	DefaultLakeObjects = 100
)

func (c *Config) validateLakeSource(a *Agent, s Source) []error {
	var errs []error
	at := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agent %q: source %q: "+format, append([]any{a.Name, s.Name}, args...)...))
	}
	if s.URL == "" || strings.Contains(s.URL, "${") || strings.Contains(s.URL, "{{") {
		at("lake needs a fixed S3-compatible url without templates")
	} else {
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			at("lake url must be an http(s) endpoint without user, query or fragment")
		} else {
			for _, part := range strings.Split(u.Path, "/") {
				if part == "." || part == ".." {
					at("lake url may not contain a dot path segment")
					break
				}
			}
		}
	}
	if !lakeBucketRE.MatchString(s.Bucket) || strings.Contains(s.Bucket, "..") {
		at("bucket %q must be a lower-case S3 bucket name", s.Bucket)
	}
	if s.Prefix == "" || strings.HasPrefix(s.Prefix, "/") || strings.Contains(s.Prefix, "\\") || strings.Contains(s.Prefix, "${") {
		at("prefix is required and must be a relative object-key prefix")
	}
	for _, part := range strings.Split(s.Prefix, "/") {
		if part == ".." || part == "." {
			at("prefix may not contain a dot path segment")
			break
		}
	}
	if !lakeRegionRE.MatchString(s.Region) {
		at("region must contain lower-case letters, numbers and hyphens")
	}
	for _, item := range []struct{ field, value string }{{"access_key_id", s.AccessKeyID}, {"secret_access_key", s.SecretAccessKey}} {
		if err := lakeEnvRef(item.field, item.value); err != nil {
			at("%v", err)
		}
		for _, name := range EnvRefs(item.value) {
			if _, ok := c.Deployment.Credentials[name]; !ok {
				at("${%s} is not in orbit.yaml's credentials", name)
			}
		}
	}
	if s.SessionToken != "" {
		if err := lakeEnvRef("session_token", s.SessionToken); err != nil {
			at("%v", err)
		}
		for _, name := range EnvRefs(s.SessionToken) {
			if _, ok := c.Deployment.Credentials[name]; !ok {
				at("${%s} is not in orbit.yaml's credentials", name)
			}
		}
	}
	if s.MaxObjects < 0 || s.MaxObjects > MaxLakeObjects {
		at("max_objects is at most %d", MaxLakeObjects)
	}
	if s.Headers != nil || s.Query != nil || s.Paginate != nil || s.Expr != "" || s.Path != "" || s.Repo != "" || s.Resource != "" || s.Ref != "" || s.State != "" || s.Limit != 0 || s.Since != 0 || len(s.ByTrigger) > 0 {
		at("lake takes url, bucket, prefix, region, credentials, max_objects and max_bytes only")
	}
	return errs
}

func lakeEnvRef(field, value string) error {
	refs := EnvRefs(value)
	if len(refs) != 1 || value != "${"+refs[0]+"}" {
		return fmt.Errorf("%s must be one ${VAR} reference, with the secret configured in orbit.yaml", field)
	}
	return nil
}

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
