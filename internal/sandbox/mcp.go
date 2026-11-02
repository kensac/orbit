package sandbox

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MCP servers and skills of a box's accounts (docs/workspace.md, Connectors
// and MCP).

// MaxMCPConfig bounds one managed server's JSON, and MaxMCPServers how many
// orbit manages.
const (
	MaxMCPConfig  = 8 << 10
	MaxMCPServers = 32
)

// TTYMCPLogin is the TTY preset that relays `claude mcp login --no-browser
// <server>` under an account: it prints the authorization URL and takes
// the pasted redirect back, for an HTTP server or a claude.ai connector.
const TTYMCPLogin = "mcp-login"

// ValidMCPName reports whether n can name a server to log in to: what
// `claude mcp list` prints before its colon (a claude.ai connector's name
// has spaces), as one argument, never an option.
func ValidMCPName(n string) bool {
	if n == "" || len(n) > 100 || !utf8.ValidString(n) || strings.HasPrefix(n, "-") || strings.TrimSpace(n) != n {
		return false
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

var managedNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ValidManagedName reports whether n can name a server orbit manages.
func ValidManagedName(n string) bool { return managedNameRE.MatchString(n) }

// ValidateMCPServers checks the servers orbit mirrors into accounts: names,
// sizes, a type Claude Code knows, and no ${VAR}, which the CLI would
// expand from the session's environment (orbit writes literal values).
func ValidateMCPServers(m map[string]json.RawMessage) error {
	if len(m) > MaxMCPServers {
		return fmt.Errorf("%d MCP servers, more than %d", len(m), MaxMCPServers)
	}
	for n, raw := range m {
		if !ValidManagedName(n) {
			return fmt.Errorf("MCP server %q: letters, digits, dot, dash and underscore", n)
		}
		if len(raw) > MaxMCPConfig {
			return fmt.Errorf("MCP server %s: more than %d bytes", n, MaxMCPConfig)
		}
		var c map[string]any
		if err := json.Unmarshal(raw, &c); err != nil || c == nil {
			return fmt.Errorf("MCP server %s: not a JSON object", n)
		}
		switch c["type"] {
		case "stdio":
			if s, _ := c["command"].(string); s == "" {
				return fmt.Errorf("MCP server %s: a stdio server needs a command", n)
			}
		case "http", "sse":
			if s, _ := c["url"].(string); !strings.HasPrefix(s, "https://") && !strings.HasPrefix(s, "http://") {
				return fmt.Errorf("MCP server %s: an %s server needs an http(s) url", n, c["type"])
			}
		default:
			return fmt.Errorf("MCP server %s: type %v, want stdio, http or sse", n, c["type"])
		}
		if strings.Contains(string(raw), "${") {
			return fmt.Errorf("MCP server %s: ${...} would be expanded from the box's environment; write literal values", n)
		}
	}
	return nil
}

// MCP statuses, from the mark `claude mcp list` prints.
const (
	MCPConnected = "connected"
	MCPNeedsAuth = "needs_auth"
	MCPFailed    = "failed"
	MCPPending   = "pending"
	MCPUnknown   = "unknown"
)

// MCPServer is one line of `claude mcp list`.
type MCPServer struct {
	Name string `json:"name"`
	// Target is its URL, or its command and arguments.
	Target string `json:"target"`
	// Transport is the CLI's (HTTP, SSE), "" when it printed none.
	Transport string `json:"transport"`
	Status    string `json:"status"`
	// Detail is what followed the status (a failure's reason).
	Detail string `json:"detail"`
	// Connector is a claude.ai connector, which the account's login
	// brings (named "claude.ai <name>").
	Connector bool `json:"connector"`
}

// The marks and the dash between a status and its detail, as the CLI
// prints them.
const (
	markOK      = "✔"
	markOKAlt   = "✓"
	markFail    = "✘"
	markFailAlt = "✗"
	markPause   = "⏸"
	detailDash  = " — "
)

var transportRE = regexp.MustCompile(` \((HTTP|SSE|stdio|WS|WebSocket)\)$`)

// ParseMCPList reads `claude mcp list`: one "name: target [(TYPE)] - mark
// status[ - detail]" line per server, after a header the CLI prints first.
// A line it cannot read is left out; at most MaxMCPServers*4 are kept.
func ParseMCPList(out []byte) []MCPServer {
	var servers []MCPServer
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimRight(line, "\r ")
		name, rest, ok := strings.Cut(line, ": ")
		if !ok || name == "" || strings.HasPrefix(line, "Checking ") {
			continue
		}
		i := strings.LastIndex(rest, " - ")
		if i < 0 {
			continue
		}
		target, state := rest[:i], strings.TrimSpace(rest[i+3:])
		s := MCPServer{Name: name, Connector: strings.HasPrefix(name, "claude.ai ")}
		if m := transportRE.FindStringSubmatch(target); m != nil {
			s.Transport, target = m[1], strings.TrimSuffix(target, m[0])
		}
		s.Target = target
		if st, detail, ok := strings.Cut(state, detailDash); ok {
			state, s.Detail = st, detail
		}
		mark, word, _ := strings.Cut(state, " ")
		switch {
		case (mark == markOK || mark == markOKAlt) && strings.HasPrefix(word, "Connected"):
			s.Status = MCPConnected
		case mark == "!" && strings.Contains(word, "auth"):
			s.Status = MCPNeedsAuth
		case mark == markFail || mark == markFailAlt:
			s.Status = MCPFailed
		case mark == markPause:
			s.Status = MCPPending
		default:
			s.Status = MCPUnknown
			s.Detail = strings.TrimSpace(state + " " + s.Detail)
		}
		servers = append(servers, s)
		if len(servers) >= MaxMCPServers*4 {
			break
		}
	}
	return servers
}
