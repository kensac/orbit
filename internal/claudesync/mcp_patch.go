package claudesync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/kensac/orbit/internal/sandbox"
)

const maxLocalMCPConfig = 1 << 20

// MCPConfigPatch is a deterministic, credential-free deployment config patch.
// The patch is JSON, which is also valid YAML, so it can be reviewed and merged
// into orbit.yaml without rewriting that file automatically.
type MCPConfigPatch struct {
	Config  []byte
	Digest  string
	Servers int
}

// NewMCPConfigPatch extracts remote MCP declarations from Claude Code's local
// config. Only HTTPS URL and SSE declarations containing type and url are
// copied. Local process commands, environment, headers, OAuth state, URL
// credentials, query strings, and every unknown field are excluded.
func NewMCPConfigPatch(data []byte) (MCPConfigPatch, error) {
	if len(data) == 0 || len(data) > maxLocalMCPConfig {
		return MCPConfigPatch{}, fmt.Errorf("claude MCP config must be between 1 and %d bytes", maxLocalMCPConfig)
	}
	var root struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return MCPConfigPatch{}, fmt.Errorf("read claude MCP config: invalid JSON object")
	}
	if raw, ok := fields["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &root.MCPServers); err != nil {
			return MCPConfigPatch{}, fmt.Errorf("read claude MCP servers: %w", err)
		}
	}
	if len(root.MCPServers) == 0 {
		return MCPConfigPatch{}, fmt.Errorf("claude MCP config has no servers")
	}
	if len(root.MCPServers) > sandbox.MaxMCPServers {
		return MCPConfigPatch{}, fmt.Errorf("claude MCP config has more than %d servers", sandbox.MaxMCPServers)
	}
	servers := make(map[string]json.RawMessage, len(root.MCPServers))
	for name, raw := range root.MCPServers {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return MCPConfigPatch{}, fmt.Errorf("MCP server %q must be an object", name)
		}
		for field := range fields {
			if field != "type" && field != "url" {
				return MCPConfigPatch{}, fmt.Errorf("MCP server %q has unsupported field %q; only type and url can be imported", name, field)
			}
		}
		var declaration struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		}
		if err := json.Unmarshal(raw, &declaration); err != nil {
			return MCPConfigPatch{}, fmt.Errorf("MCP server %q: %w", name, err)
		}
		if declaration.Type != "http" && declaration.Type != "sse" {
			return MCPConfigPatch{}, fmt.Errorf("MCP server %q uses %q; only remote http and sse declarations can be imported", name, declaration.Type)
		}
		u, err := url.Parse(declaration.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return MCPConfigPatch{}, fmt.Errorf("MCP server %q needs an https URL without credentials, query, or fragment", name)
		}
		canonical, err := json.Marshal(declaration)
		if err != nil {
			return MCPConfigPatch{}, fmt.Errorf("MCP server %q: %w", name, err)
		}
		servers[name] = canonical
	}
	if err := sandbox.ValidateMCPServers(servers); err != nil {
		return MCPConfigPatch{}, err
	}
	config := struct {
		Deployment struct {
			Box struct {
				MCPServers map[string]json.RawMessage `json:"mcp_servers"`
			} `json:"box"`
		} `json:"deployment"`
	}{}
	config.Deployment.Box.MCPServers = servers
	b, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return MCPConfigPatch{}, fmt.Errorf("encode MCP config patch: %w", err)
	}
	b = append(b, '\n')
	sum := sha256.Sum256(b)
	return MCPConfigPatch{Config: b, Digest: hex.EncodeToString(sum[:]), Servers: len(servers)}, nil
}
