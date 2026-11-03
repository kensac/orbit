package github_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/spec"
)

// TestBoxTokenLeavesOutTheHostRepos mints the box token over the
// installation's repositories less the homelab host repositories, with the
// default permissions, listing them with a metadata token revoked after.
func TestBoxTokenLeavesOutTheHostRepos(t *testing.T) {
	f := newFake(t)
	f.InstallationRepos = []string{"kensac/orbit", "kensac/hetzner-vps-config", "kensac/oci-vps-config", "kensac/homelab-config",
		"kensac/dns-config", "kensac/personal-portfolio"}
	for i := range 120 {
		f.InstallationRepos = append(f.InstallationRepos, "kensac/r"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	g := &spec.GitHub{Repositories: []string{"kensac/orbit"}}
	tok, repos, err := app(f).BoxToken(ctx, g)
	if err != nil || tok.Value != "ghs_test" {
		t.Fatalf("%+v %v", tok, err)
	}
	if len(repos) != 122 || strings.Contains(strings.Join(repos, " "), "-config") {
		t.Fatalf("%d repos: %v", len(repos), repos[:4])
	}
	if len(f.TokenReqs) != 2 || len(f.Revoked) != 1 {
		t.Fatalf("token requests %v revoked %v", f.TokenReqs, f.Revoked)
	}
	if b, _ := json.Marshal(f.TokenReqs[0]); string(b) != `{"permissions":{"metadata":"read"}}` {
		t.Fatalf("listing token %s", b)
	}
	var req struct {
		Permissions  map[string]string `json:"permissions"`
		Repositories []string          `json:"repositories"`
	}
	b, _ := json.Marshal(f.TokenReqs[1])
	_ = json.Unmarshal(b, &req)
	if !reflect.DeepEqual(req.Permissions, spec.DefaultBoxPermissions) || len(req.Repositories) != 122 ||
		!slices.Contains(req.Repositories, "orbit") || slices.Contains(req.Repositories, "hetzner-vps-config") {
		t.Fatalf("box token request %s", b)
	}

	// Named repositories are exactly those, with no listing; a host repo
	// is reachable when named.
	f.TokenReqs, f.Revoked = nil, nil
	g.Box = &spec.GitHubBox{Repositories: []string{"kensac/hetzner-vps-config", "kensac/orbit"}, Permissions: map[string]string{"contents": "read"}}
	_, repos, err = app(f).BoxToken(ctx, g)
	if err != nil || !reflect.DeepEqual(repos, []string{"kensac/hetzner-vps-config", "kensac/orbit"}) || len(f.TokenReqs) != 1 {
		t.Fatalf("named: %v %v %v", repos, err, f.TokenReqs)
	}
	if b, _ := json.Marshal(f.TokenReqs[0]); string(b) != `{"permissions":{"contents":"read"},"repositories":["hetzner-vps-config","orbit"]}` {
		t.Fatalf("named token request %s", b)
	}
	// A grant beyond the ask is refused and revoked.
	f.Granted = map[string]string{"contents": "write", "administration": "write"}
	if _, _, err := app(f).BoxToken(ctx, g); err == nil || len(f.Revoked) != 1 {
		t.Fatalf("over-scoped: %v %v", err, f.Revoked)
	}
}

func TestBoxRepos(t *testing.T) {
	g := &spec.GitHub{Box: &spec.GitHubBox{Exclude: []string{"kensac/secret*"}}}
	got := github.BoxRepos(g, []string{"kensac/b", "kensac/secret-x", "kensac/a-config", "kensac/a"})
	if !reflect.DeepEqual(got, []string{"kensac/a", "kensac/a-config", "kensac/b"}) {
		t.Fatalf("%v", got)
	}
}
