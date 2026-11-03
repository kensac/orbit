package spec

import (
	"strings"
	"testing"
)

func TestGitHubCommentNeedsCommentRepositories(t *testing.T) {
	agent := "name: a\ncharter: x\nactions: {github:comment: approve}\n"
	for gh, want := range map[string]string{
		"github: {app_id: 1, private_key_env: K, repositories: [kensac/orbit], comment_repositories: [kensac/homelab-docs]}\n": "",
		"github: {app_id: 1, private_key_env: K, repositories: [kensac/orbit]}\n":                                              "needs github.comment_repositories",
		"github: {app_id: 1, private_key_env: K, repositories: [kensac/orbit], comment_repositories: [nope]}\n":                `comment_repositories: "nope" is not owner/name`,
		"": "needs github.comment_repositories",
	} {
		_, err := Load(write(t, map[string]string{"orbit.yaml": base + gh, "agents/a.yaml": agent}))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: %v, want %q", gh, err, want)
		}
	}
}
