package build

import (
	"reflect"
	"testing"

	"github.com/kensac/orbit/internal/sandbox"
)

func TestSandboxEnvChoosesAsOrdinaryRunsDo(t *testing.T) {
	const tok, key = "sk-ant-oat01-token", "sk-ant-api03-key"
	for name, tc := range map[string]struct {
		env  map[string]string
		want map[string]string
	}{
		"token":         {map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tok}, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tok}},
		"api key":       {map[string]string{"ANTHROPIC_API_KEY": key}, map[string]string{"ANTHROPIC_API_KEY": key}},
		"token wins":    {map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tok, "ANTHROPIC_API_KEY": key}, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tok}},
		"empty token":   {map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "", "ANTHROPIC_API_KEY": key}, map[string]string{"ANTHROPIC_API_KEY": key}},
		"scrubbed rest": {map[string]string{"ANTHROPIC_API_KEY": key, "ANTHROPIC_AUTH_TOKEN": "a", "AWS_BEARER_TOKEN_BEDROCK": "b", "CLAUDECODE": "1", "GITHUB_TOKEN": "g"}, map[string]string{"ANTHROPIC_API_KEY": key}},
		"neither":       {map[string]string{"ANTHROPIC_AUTH_TOKEN": "a"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := SandboxEnv(func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok })
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SandboxEnv = %v, want %v", got, tc.want)
			}
			if got != nil {
				if err := (sandbox.AttachRequest{Argv: []string{"claude"}, Env: got}).Validate(); err != nil {
					t.Fatalf("sandboxd would refuse it: %v", err)
				}
			}
		})
	}
}
