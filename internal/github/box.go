package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// The token in an agent's box (docs/workspace.md, GitHub in the box): one
// installation token for the person's gh and git there, over the
// repositories github.box names, or every repository of the installation
// that github.box.exclude does not match (by default not the homelab host
// repositories). orbit writes it into running boxes; it is never stored.

// maxBoxRepos is GitHub's limit on the repositories one token names.
const maxBoxRepos = 500

// installation finds the App's installation on an account (a user's, else
// an organization's).
func (a *App) installation(ctx context.Context, app *Client, owner string) (int64, error) {
	var inst struct {
		ID int64 `json:"id"`
	}
	err := app.do(ctx, http.MethodGet, "/users/"+url.PathEscape(owner)+"/installation", nil, &inst)
	if IsStatus(err, http.StatusNotFound) {
		err = app.do(ctx, http.MethodGet, "/orgs/"+url.PathEscape(owner)+"/installation", nil, &inst)
	}
	if err != nil {
		return 0, fmt.Errorf("github: find the installation on %s: %w", owner, err)
	}
	return inst.ID, nil
}

// mint asks for an installation token with perms, over names (the
// repositories' names without the owner) or, when names is nil, every
// repository of the installation; a grant beyond perms is revoked and
// refused.
func (a *App) mint(ctx context.Context, app *Client, inst int64, names []string, perms Permissions) (*Token, error) {
	body := map[string]any{"permissions": perms}
	if names != nil {
		body["repositories"] = names
	}
	var out struct {
		Token     string      `json:"token"`
		ExpiresAt time.Time   `json:"expires_at"`
		Perms     Permissions `json:"permissions"`
	}
	if err := app.do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst), body, &out); err != nil {
		return nil, fmt.Errorf("github: mint an installation token: %w", err)
	}
	if out.Token == "" {
		return nil, errors.New("github: mint an installation token: empty token")
	}
	t := &Token{Value: out.Token, ExpiresAt: out.ExpiresAt, app: a}
	for k, v := range out.Perms {
		if k != "metadata" && perms[k] != v {
			_ = t.Revoke(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("github: the token came back with %s=%s, which was not asked for", k, v)
		}
	}
	return t, nil
}

// installationRepos lists every repository an installation reaches, with a
// metadata-only token revoked after.
func (a *App) installationRepos(ctx context.Context, app *Client, inst int64) ([]string, error) {
	t, err := a.mint(ctx, app, inst, nil, Permissions{"metadata": "read"})
	if err != nil {
		return nil, err
	}
	defer func() { _ = t.Revoke(context.WithoutCancel(ctx)) }()
	c := t.Client()
	var out []string
	for page := 1; ; page++ {
		var resp struct {
			Total int `json:"total_count"`
			Repos []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), nil, &resp); err != nil {
			return nil, fmt.Errorf("github: list the installation's repositories: %w", err)
		}
		for _, r := range resp.Repos {
			out = append(out, r.FullName)
		}
		if len(resp.Repos) < 100 || len(out) >= resp.Total || page >= 20 {
			return out, nil
		}
	}
}

// BoxRepos is what the box token reaches of all, an installation's
// repositories: g's box repositories when it names them, else all less
// what its exclusion globs match.
func BoxRepos(g *spec.GitHub, all []string) []string {
	if g.Box != nil && len(g.Box.Repositories) > 0 {
		return slices.Clone(g.Box.Repositories)
	}
	var out []string
	for _, r := range all {
		if !slices.ContainsFunc(g.BoxExclude(), func(x string) bool { ok, _ := path.Match(x, r); return ok }) {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out
}

// BoxToken mints the token a box's gh and git use, and returns it with the
// repositories it reaches.
func (a *App) BoxToken(ctx context.Context, g *spec.GitHub) (*Token, []string, error) {
	owner := g.BoxOwner()
	if owner == "" {
		return nil, nil, errors.New("github: no owner for the box token (github.box.owner)")
	}
	jwt, err := a.JWT()
	if err != nil {
		return nil, nil, err
	}
	app := &Client{base: a.BaseURL, auth: "Bearer " + jwt, http: a.HTTP}
	inst, err := a.installation(ctx, app, owner)
	if err != nil {
		return nil, nil, err
	}
	var all []string
	if g.Box == nil || len(g.Box.Repositories) == 0 {
		if all, err = a.installationRepos(ctx, app, inst); err != nil {
			return nil, nil, err
		}
	}
	repos := BoxRepos(g, all)
	if len(repos) == 0 {
		return nil, nil, errors.New("github: the box token would reach no repository")
	}
	if len(repos) > maxBoxRepos {
		return nil, nil, fmt.Errorf("github: the box token would reach %d repositories, more than %d", len(repos), maxBoxRepos)
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		o, n, _ := strings.Cut(r, "/")
		if o != owner {
			return nil, nil, fmt.Errorf("github: %s is not %s's: one token reaches one installation", r, owner)
		}
		names = append(names, n)
	}
	t, err := a.mint(ctx, app, inst, names, Permissions(g.BoxPermissions()))
	if err != nil {
		return nil, nil, err
	}
	return t, repos, nil
}
