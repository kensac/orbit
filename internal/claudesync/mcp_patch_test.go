package claudesync

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewMCPConfigPatchIsCredentialFreeAndDeterministic(t *testing.T) {
	local := []byte(`{"theme":"dark","oauthAccount":{"accessToken":"private"},"mcpServers":{"docs":{"type":"http","url":"https://docs.example/mcp"},"events":{"type":"sse","url":"https://events.example/sse"}}}`)
	one, err := NewMCPConfigPatch(local)
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewMCPConfigPatch(local)
	if err != nil {
		t.Fatal(err)
	}
	if one.Digest == "" || one.Digest != two.Digest || string(one.Config) != string(two.Config) || one.Servers != 2 {
		t.Fatalf("patch is not stable: first=%+v second=%+v", one, two)
	}
	for _, private := range []string{"private", "oauthAccount", "theme"} {
		if strings.Contains(string(one.Config), private) {
			t.Fatalf("local private config %q leaked into patch: %s", private, one.Config)
		}
	}
	var patch struct {
		Deployment struct {
			Box struct {
				Servers map[string]json.RawMessage `json:"mcp_servers"`
			} `json:"box"`
		} `json:"deployment"`
	}
	if err := json.Unmarshal(one.Config, &patch); err != nil {
		t.Fatal(err)
	}
	if len(patch.Deployment.Box.Servers) != 2 {
		t.Fatalf("servers = %s", one.Config)
	}
}

func TestNewMCPConfigPatchRejectsSecretsAndExecutableServers(t *testing.T) {
	for _, input := range []string{
		`{"mcpServers":{"docs":{"type":"http","url":"https://user:pass@docs.example/mcp"}}}`,
		`{"mcpServers":{"docs":{"type":"http","url":"https://docs.example/mcp?token=secret"}}}`,
		`{"mcpServers":{"docs":{"type":"http","url":"https://docs.example/mcp","headers":{"Authorization":"Bearer secret"}}}}`,
		`{"mcpServers":{"docs":{"type":"http","url":"https://docs.example/mcp","env":{"TOKEN":"secret"}}}}`,
		`{"mcpServers":{"local":{"type":"stdio","command":"sh","args":["-c","curl bad"]}}}`,
		`{"mcpServers":{"docs":{"type":"http","url":"http://docs.example/mcp"}}}`,
		`{"mcpServers":{"docs":{"type":"http","url":"https://docs.example/mcp"}}} {}`,
	} {
		if _, err := NewMCPConfigPatch([]byte(input)); err == nil {
			t.Errorf("accepted unsafe MCP config: %s", input)
		}
	}
}

func TestNewMCPConfigPatchRequiresServersAndValidName(t *testing.T) {
	for _, input := range []string{
		`{"theme":"dark"}`,
		`{"mcpServers":{"bad name":{"type":"http","url":"https://docs.example/mcp"}}}`,
		`{"mcpServers":{"docs":{"type":"http","url":"https://docs.example/mcp#frag"}}}`,
	} {
		if _, err := NewMCPConfigPatch([]byte(input)); err == nil {
			t.Errorf("accepted config: %s", input)
		}
	}
}
