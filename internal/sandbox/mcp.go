package sandbox

import (
	"encoding/json"
	"fmt"
	"net/url"
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
var credentialArgRE = regexp.MustCompile(`(?i)(^|[-_])(user|token|secret|password|passwd|api[-_]?key|authorization|credential|bearer)(=|$)`)
var credentialValueRE = regexp.MustCompile(`(?i)(?:authorization\s*:\s*(?:basic|bearer)\s+\S+|bearer\s+\S+|(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-(?:ant-)?[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}))`)

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
		if err := rejectMCPCredentials(c); err != nil {
			return fmt.Errorf("MCP server %s: %w", n, err)
		}
		switch c["type"] {
		case "stdio":
			if s, _ := c["command"].(string); s == "" {
				return fmt.Errorf("MCP server %s: a stdio server needs a command", n)
			}
		case "http", "sse":
			s, _ := c["url"].(string)
			u, err := url.Parse(s)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
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

func rejectMCPCredentials(v any) error {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			key := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(k))
			if key == "env" || key == "headers" || strings.Contains(key, "auth") || strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "credential") || key == "apikey" {
				return fmt.Errorf("credential-bearing field %q is not allowed; use per-account OAuth login", k)
			}
			if err := rejectMCPCredentials(value); err != nil {
				return err
			}
		}
	case []any:
		for i, value := range x {
			if _, ok := value.(string); ok && i > 0 {
				if flag, ok := x[i-1].(string); ok && credentialValueFlagRE.MatchString(flag) {
					return fmt.Errorf("credential-bearing argument is not allowed; use per-account OAuth login")
				}
			}
			if err := rejectMCPCredentials(value); err != nil {
				return err
			}
		}
	case string:
		if credentialArgRE.MatchString(x) || credentialValueRE.MatchString(x) {
			return fmt.Errorf("argument resembles a credential; use per-account OAuth login")
		}
		if u, err := url.Parse(x); err == nil && u.User != nil {
			return fmt.Errorf("URL userinfo is not allowed; use per-account OAuth login")
		}
	}
	return nil
}

var credentialValueFlagRE = regexp.MustCompile(`(?i)^(?:-u|--user|--username|--password|--passwd|--token|--secret|--api[-_]?key|--authorization|--credential)$`)

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
	// Transport is the CLI's transport label; the command and target are
	// deliberately omitted because they can contain credentials.
	Transport string `json:"transport"`
	Status    string `json:"status"`
	// Connector is a claude.ai connector, which the account's login
	// brings (named "claude.ai <name>").
	Connector bool `json:"connector"`
}

// The marks and the dash between a status and its detail, as the CLI
// prints them.
const (
	markOK      = "\u2714"
	markOKAlt   = "\u2713"
	markFail    = "\u2718"
	markFailAlt = "\u2717"
	markPause   = "\u23f8"
	detailDash  = " \u2014 "
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
			s.Transport = m[1]
		}
		if st, _, ok := strings.Cut(state, detailDash); ok {
			state = st
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
		}
		servers = append(servers, s)
		if len(servers) >= MaxMCPServers*4 {
			break
		}
	}
	return servers
}
