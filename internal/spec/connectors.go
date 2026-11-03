package spec

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Connector is an MCP server orbit reaches for its agents (docs/agents/configuration.md, Connectors). orbit is the gateway: a run's CLI sees the server's tools as its own, and every call goes through orbit's process, which adds the credential, so no token enters the CLI or a sandbox.
type Connector struct {
	// Transport is how orbit speaks to the server: "http" (MCP streamable HTTP), the only one today.
	Transport string `yaml:"transport,omitempty" json:"transport,omitempty"`
	// URL is the server's MCP endpoint.
	URL string `yaml:"url" json:"url"`
	// Description tells the model what the server is for.
	Description string        `yaml:"description,omitempty" json:"description,omitempty"`
	Auth        ConnectorAuth `yaml:"auth,omitempty" json:"auth,omitempty"`
	// WriteTools are the server's tools that change the world. A call to one waits for a person unless the agent's rules say otherwise; every other tool runs at once.
	WriteTools []string `yaml:"write_tools,omitempty" json:"write_tools,omitempty"`
	// DefaultToolRule overrides the static connector default for every tool.
	// The catalog uses approve until a person creates a narrower agent rule.
	DefaultToolRule Rule `yaml:"default_tool_rule,omitempty" json:"default_tool_rule,omitempty"`
	// CatalogManaged marks a connector loaded from the person-added catalog.
	CatalogManaged bool `yaml:"-" json:"catalog_managed,omitempty"`
}

// ConnectorAuth is how orbit signs a connector's requests.
type ConnectorAuth struct {
	// Kind is "none" (the default), "header" or "oauth".
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"`
	// Header and Value are a header kind's request header; Value's ${VAR}s must be credentials whose url_prefix covers the connector's URL.
	Header string `yaml:"header,omitempty" json:"header,omitempty"`
	Value  string `yaml:"value,omitempty" json:"value,omitempty"`
	// Scopes are what an oauth kind asks the authorization server for; none asks for what the server advertises.
	Scopes []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
	// ClientID is an oauth client registered by hand; without it orbit registers itself (dynamic client registration). ClientSecretEnv names its secret, for a confidential client.
	ClientID        string `yaml:"client_id,omitempty" json:"client_id,omitempty"`
	ClientSecretEnv string `yaml:"client_secret_env,omitempty" json:"client_secret_env,omitempty"`
}

// Connector auth kinds.
const (
	ConnectorAuthNone   = "none"
	ConnectorAuthHeader = "header"
	ConnectorAuthOAuth  = "oauth"
)

// ConnectorTransportHTTP is MCP's streamable HTTP transport.
const ConnectorTransportHTTP = "http"

// ConnectorRulePrefix starts an agent's per-tool rule, mcp:<connector>:<tool>, in its actions.
const ConnectorRulePrefix = "mcp:"

// ConnectorActionPrefix starts the action type a connector call that waits for approval is recorded as: connector:<connector>/<tool>.
const ConnectorActionPrefix = "connector:"

// ConnectorKeyEnv holds the key that seals connector tokens in the database. Every host that calls an oauth connector needs it.
const ConnectorKeyEnv = "ORBIT_CONNECTOR_KEY"

// ReservedConnectorNames are taken by orbit's own MCP server.
var ReservedConnectorNames = []string{"orbit"}

// ConnectorToolRE is a tool name orbit offers: the Messages API's tool name alphabet. A tool named otherwise is not offered.
var ConnectorToolRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ConnectorNameRE validates names used for server definitions and catalog entries.
var ConnectorNameRE = regexp.MustCompile(AgentNamePattern)

// ConnectorRuleKey is the actions key of one connector tool's rule.
func ConnectorRuleKey(conn, tool string) string { return ConnectorRulePrefix + conn + ":" + tool }

// ConnectorActionType is the action type of a call to tool waiting for approval.
func ConnectorActionType(conn, tool string) string { return ConnectorActionPrefix + conn + "/" + tool }

// ParseConnectorAction splits a connector action type into its connector and tool.
func ParseConnectorAction(typ string) (conn, tool string, ok bool) {
	rest, ok := strings.CutPrefix(typ, ConnectorActionPrefix)
	if !ok {
		return "", "", false
	}
	conn, tool, ok = strings.Cut(rest, "/")
	return conn, tool, ok && conn != "" && tool != ""
}

// ToolRule is the rule for one of a connector's tools in an agent's runs: the agent's mcp:<conn>:<tool> when it names one; else approve for a tool the deployment lists in write_tools, and auto for any other. A write_tools entry wins over a server's readOnlyHint: the deployment, not the server, says what changes the world here.
func (a *Agent) ToolRule(c Connector, conn, tool string) Rule {
	if r, ok := a.Actions[ConnectorRuleKey(conn, tool)]; ok {
		return r
	}
	if c.DefaultToolRule != "" {
		return c.DefaultToolRule
	}
	if slices.Contains(c.WriteTools, tool) {
		return RuleApprove
	}
	return RuleAuto
}

// AuthKind is the auth kind with its default filled in.
func (c Connector) AuthKind() string {
	if c.Auth.Kind == "" {
		return ConnectorAuthNone
	}
	return c.Auth.Kind
}

func (c *Config) validateConnectors() []error {
	var errs []error
	names := make([]string, 0, len(c.Deployment.Connectors))
	for n := range c.Deployment.Connectors {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		cn := c.Deployment.Connectors[n]
		at := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("connector %q: "+format, append([]any{n}, args...)...))
		}
		if !nameRE.MatchString(n) {
			at("name must match %s", nameRE)
		}
		if cn.DefaultToolRule != "" && cn.DefaultToolRule != RuleApprove && cn.DefaultToolRule != RuleAuto {
			at("default_tool_rule must be auto or approve")
		}
		if slices.Contains(ReservedConnectorNames, n) {
			at("name is orbit's own MCP server's")
		}
		switch cn.Transport {
		case "", ConnectorTransportHTTP:
		case "stdio":
			at("transport stdio is not supported yet: a stdio server would run in sandboxd, and only http is built")
		default:
			at("transport must be http")
		}
		u, err := url.Parse(cn.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" {
			at("url %q must be an http(s) URL with a host, and no user or fragment", cn.URL)
		}
		for _, t := range cn.WriteTools {
			if !ConnectorToolRE.MatchString(t) {
				at("write_tools: %q is not a tool name (%s)", t, ConnectorToolRE)
			}
		}
		au := cn.Auth
		switch cn.AuthKind() {
		case ConnectorAuthNone:
			if au.Header != "" || au.Value != "" || len(au.Scopes) > 0 || au.ClientID != "" || au.ClientSecretEnv != "" {
				at("auth: kind none takes no other field")
			}
		case ConnectorAuthHeader:
			if au.Header == "" || au.Value == "" {
				at("auth: kind header needs header and value")
			}
			if len(au.Scopes) > 0 || au.ClientID != "" || au.ClientSecretEnv != "" {
				at("auth: kind header takes header and value only")
			}
			for _, r := range EnvRefs(au.Value) {
				if _, ok := c.Deployment.Credentials[r]; !ok {
					at("auth: ${%s} is not in orbit.yaml's credentials; a connector's header may carry only a credential, bound to the connector's URL", r)
					continue
				}
				if err := c.Deployment.Credentials.Check(r, cn.URL); err != nil {
					at("auth: %v", err)
				}
			}
		case ConnectorAuthOAuth:
			if au.Header != "" || au.Value != "" {
				at("auth: kind oauth takes scopes, client_id and client_secret_env")
			}
			if au.ClientSecretEnv != "" && au.ClientID == "" {
				at("auth: client_secret_env needs client_id")
			}
		default:
			at("auth: kind must be none, header or oauth")
		}
	}
	return errs
}

// validateGoogleActions makes each typed Google write resolve to one fixed
// configured write tool, with its credential retained by orbit.
func (c *Config) validateGoogleActions() []error {
	g := c.Deployment.GoogleActions
	if g == nil {
		return nil
	}
	var errs []error
	if g.GmailDraft == nil && g.CalendarCreate == nil {
		errs = append(errs, fmt.Errorf("google_actions needs gmail_draft or calendar_create"))
	}
	validate := func(name, connector, tool string) {
		at := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("google_actions.%s: "+format, append([]any{name}, args...)...))
		}
		cn, ok := c.Deployment.Connectors[connector]
		if connector == "" || !ok {
			at("connector %q is not configured", connector)
			return
		}
		if !ConnectorToolRE.MatchString(tool) {
			at("tool %q is not a valid connector tool name", tool)
		}
		if !slices.Contains(cn.WriteTools, tool) {
			at("tool %q must be listed in connectors.%s.write_tools", tool, connector)
		}
		if cn.AuthKind() == ConnectorAuthNone {
			at("connector %q must have authentication configured", connector)
		}
	}
	if b := g.GmailDraft; b != nil {
		validate("gmail_draft", b.Connector, b.Tool)
	}
	if b := g.CalendarCreate; b != nil {
		validate("calendar_create", b.Connector, b.Tool)
		if strings.ContainsAny(b.CalendarID, "\r\n") || len(b.CalendarID) > 320 {
			errs = append(errs, fmt.Errorf("google_actions.calendar_create.calendar_id must be at most 320 bytes without line breaks"))
		}
	}
	return errs
}

// validateAgentConnectors checks an agent's connectors and its per-tool rules.
func (c *Config) validateAgentConnectors(a *Agent) []error {
	var errs []error
	at := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agent %q: "+format, append([]any{a.Name}, args...)...))
	}
	seen := map[string]bool{}
	for _, n := range a.Connectors {
		if _, ok := c.Deployment.Connectors[n]; !ok {
			at("connectors: %q is not a connector in orbit.yaml", n)
		}
		if seen[n] {
			at("connectors: %q is listed twice", n)
		}
		seen[n] = true
	}
	if len(a.Connectors) > 0 && c.Deployment.Runners[a.Runner].Kind != "claude-code" {
		at("connectors: need a claude-code runner")
	}
	for typ := range a.Actions {
		rest, ok := strings.CutPrefix(typ, ConnectorRulePrefix)
		if !ok {
			continue
		}
		conn, tool, ok := strings.Cut(rest, ":")
		if !ok || !ConnectorToolRE.MatchString(tool) {
			at("action %q: want mcp:<connector>:<tool>", typ)
			continue
		}
		if !seen[conn] {
			at("action %q: the agent does not list connector %q", typ, conn)
		}
	}
	if g := c.Deployment.GoogleActions; g != nil {
		if b := g.GmailDraft; b != nil && a.RuleFor("gmail:draft") != RuleDeny && !seen[b.Connector] {
			at("action %q: the agent must list connector %q", "gmail:draft", b.Connector)
		}
		if b := g.CalendarCreate; b != nil && a.RuleFor("calendar:create") != RuleDeny && !seen[b.Connector] {
			at("action %q: the agent must list connector %q", "calendar:create", b.Connector)
		}
	}
	return errs
}
