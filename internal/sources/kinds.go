package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/spec"
)

// loki runs one LogQL query_range ending now: since back (default spec.DefaultLokiSince), at most limit lines (default spec.DefaultLokiLimit), newest first. The input is Loki's data without its stats: {resultType, result}.
func (f *Fetcher) loki(ctx context.Context, s spec.Source, vars Vars) (*Fetched, error) {
	used := map[string]bool{}
	base, err := f.render(s.URL, vars, used)
	if err != nil {
		return nil, err
	}
	expr, err := f.render(s.Expr, vars, used)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	for k, v := range s.Headers {
		rv, err := f.render(v, vars, used)
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		headers.Set(k, rv)
	}
	since := time.Duration(s.Since)
	if since == 0 {
		since = spec.DefaultLokiSince
	}
	limit := s.Limit
	if limit == 0 {
		limit = spec.DefaultLokiLimit
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	end := now().UTC()
	start := end.Add(-since)
	u, err := url.Parse(strings.TrimRight(base, "/") + "/loki/api/v1/query_range")
	if err != nil {
		return nil, err
	}
	u.RawQuery = url.Values{"query": {expr}, "start": {strconv.FormatInt(start.UnixNano(), 10)}, "end": {strconv.FormatInt(end.UnixNano(), 10)},
		"limit": {strconv.Itoa(limit)}, "direction": {"backward"}}.Encode()
	b, _, err := f.get(ctx, u.String(), headers, s.MaxBytes, used)
	if err != nil {
		return nil, err
	}
	var r struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("loki: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("loki: %s", r.Error)
	}
	if len(r.Data.Result) == 0 {
		r.Data.Result = json.RawMessage(`[]`)
	}
	out, _ := json.Marshal(map[string]any{"resultType": r.Data.ResultType, "result": r.Data.Result})
	return &Fetched{Name: s.Name, MediaType: "application/json", Content: out,
		Meta: map[string]any{"expr": expr, "start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339), "limit": limit}}, nil
}

// githubPerms is the token each github resource is fetched with.
var githubPerms = map[string]github.Permissions{
	"issues":   github.ReadIssues,
	"pulls":    github.ReadPullList,
	"checks":   github.ReadChecks,
	"releases": github.ReadReleases,
}

// github reads one resource of one repository as the App, with a token minted for this fetch alone and revoked after it.
func (f *Fetcher) github(ctx context.Context, s spec.Source) (*Fetched, error) {
	perms, ok := githubPerms[s.Resource]
	if !ok {
		return nil, fmt.Errorf("github: unknown resource %q", s.Resource)
	}
	if f.GitHub == nil {
		return nil, fmt.Errorf("github: this instance holds no GitHub App")
	}
	tok, err := f.GitHub(ctx, s.Repo, perms)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tok.Revoke(context.WithoutCancel(ctx)) }()
	c := tok.Client()
	var v any
	var n int
	switch s.Resource {
	case "issues":
		items, err := c.ListIssues(ctx, s.Repo, s.State, s.Limit)
		v, n = items, len(items)
		if err != nil {
			return nil, err
		}
	case "pulls":
		limit := s.Limit
		if limit == 0 {
			limit = 100
		}
		items, err := c.ListPulls(ctx, s.Repo, s.State, limit)
		v, n = items, len(items)
		if err != nil {
			return nil, err
		}
	case "releases":
		items, err := c.ListReleases(ctx, s.Repo, s.Limit)
		v, n = items, len(items)
		if err != nil {
			return nil, err
		}
	case "checks":
		runs, err := c.CheckRuns(ctx, s.Repo, s.Ref)
		if err != nil {
			return nil, err
		}
		statuses, err := c.Statuses(ctx, s.Repo, s.Ref)
		if err != nil {
			return nil, err
		}
		checks := make([]map[string]any, len(runs))
		for i, r := range runs {
			checks[i] = map[string]any{"name": r.Name, "status": r.Status, "conclusion": r.Conclusion, "url": r.HTMLURL}
		}
		sts := make([]map[string]any, len(statuses))
		for i, st := range statuses {
			sts[i] = map[string]any{"context": st.Context, "state": st.State, "url": st.TargetURL}
		}
		v, n = map[string]any{"ref": s.Ref, "check_runs": checks, "statuses": sts}, len(runs)+len(statuses)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	max := s.MaxBytes
	if int64(len(b)) > max {
		return nil, fmt.Errorf("source exceeds max_bytes (%d)", max)
	}
	return &Fetched{Name: s.Name, MediaType: "application/json", Content: b,
		Meta: map[string]any{"repo": s.Repo, "resource": s.Resource, "ref": s.Ref, "state": s.State, "items": n}}, nil
}
