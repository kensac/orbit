package spec

import (
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// What an agent may reach depends on who wrote it. A file agent is reviewed in git with the deployment, so it may name any source, variable and tool. A database agent is written through the API or by an agent's agent:define, and the built-in one is changed the same way, so a model steered by a poisoned input can write either: they are held to what orbit.yaml offers (docs/agents/security.md, What a model can define).

// Credential is a secret a source or an endpoint may send: the environment variable named by its key in orbit.yaml's credentials, and the only URLs it may be sent to.
type Credential struct {
	// URLPrefix is where the credential may go: every request it is rendered into must start with it. It is an absolute http(s) URL with a path ending in "/", so the host and the path segment it names are whole.
	URLPrefix string `yaml:"url_prefix" json:"url_prefix"`
}

// Credentials are orbit.yaml's credentials, keyed by variable.
type Credentials map[string]Credential

// UnsandboxedTools are the only tools a database or built-in agent without a sandbox may be given. Such a run is in orbit's own container, which holds every ORBIT_* secret, so a tool that runs a command or writes a file (Bash, Edit, Write, NotebookEdit) would reach all of them. In a sandbox (Agent.Sandbox) the container is the boundary instead. This is the one list; validation reads it.
var UnsandboxedTools = []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch", "Task", "TodoWrite"}

var credentialNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// Restricted reports whether the agent was written outside the configuration files (the API, agent:define or code), so the limits above apply. An agent a worker rebuilt from the database has no origin; control validated it before recording it.
func (a *Agent) Restricted() bool { return a.Origin == OriginDatabase || a.Origin == OriginBuiltin }

// EnvRefs lists the variables a source renders into its requests, sorted and without repeats.
func (s Source) EnvRefs() []string {
	b := []string{s.URL, s.Expr}
	for _, v := range s.Headers {
		b = append(b, v)
	}
	for _, v := range s.Query {
		b = append(b, v)
	}
	var out []string
	for _, r := range EnvRefs(strings.Join(b, "\x00")) {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// Check refuses to send credential name to rawURL unless the URL starts with the credential's prefix and has no dot segment in its path, which a server could resolve outside the prefix. A variable that is not a credential is not checked.
func (cs Credentials) Check(name, rawURL string) error {
	cr, ok := cs[name]
	if !ok {
		return nil
	}
	if !strings.HasPrefix(rawURL, cr.URLPrefix) {
		// The URL is not quoted: it may hold the credential.
		return fmt.Errorf("credential %s may be sent only to %s", name, cr.URLPrefix)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("credential %s: the URL does not parse", name)
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("credential %s: the URL's path has a dot segment", name)
		}
	}
	return nil
}

func (c *Config) validateCredentials() []error {
	var errs []error
	names := make([]string, 0, len(c.Deployment.Credentials))
	for n := range c.Deployment.Credentials {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := c.Deployment.Credentials[n].URLPrefix
		if !credentialNameRE.MatchString(n) {
			errs = append(errs, fmt.Errorf("credential %q: the name is an environment variable (%s)", n, credentialNameRE))
		}
		u, err := url.Parse(p)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
			u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/") || u.String() != p {
			errs = append(errs, fmt.Errorf("credential %q: url_prefix %q must be an http(s) URL with a host and a path ending in /, and no user, query or fragment", n, p))
		}
	}
	return errs
}

// validateTrust holds a restricted agent to what orbit.yaml offers, and checks every agent's credentials against their prefixes where the URL is known before the fetch.
func (c *Config) validateTrust(a *Agent) []error {
	var errs []error
	at := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agent %q: "+format, append([]any{a.Name}, args...)...))
	}
	for _, s := range a.Sources {
		if a.Restricted() && s.Kind == "file" {
			at("source %q: a file source reads orbit's own filesystem, so only an agent in the configuration files may have one", s.Name)
		}
		for _, r := range s.EnvRefs() {
			if _, ok := c.Deployment.Credentials[r]; !ok {
				if a.Restricted() {
					at("source %q: ${%s} is not in orbit.yaml's credentials, and an agent outside the configuration files may reference only those", s.Name, r)
				}
				continue
			}
			u := s.URL
			switch s.Kind {
			case "prometheus":
				u = strings.TrimRight(u, "/") + "/api/v1/query"
			case "loki":
				u = strings.TrimRight(u, "/") + "/loki/api/v1/query_range"
			}
			if strings.Contains(u, "{{") || strings.Contains(u, "${") {
				continue // checked on every request (sources.Fetcher)
			}
			if err := c.Deployment.Credentials.Check(r, u); err != nil {
				at("source %q: %v", s.Name, err)
			}
		}
	}
	if a.Restricted() {
		for i, h := range a.Hooks {
			if len(h.Run) > 0 {
				// Like a precheck: a command on orbit's host, as orbit's user, who can read /proc/<orbit>/environ.
				at("hooks[%d]: a run hook runs a command on orbit's host, so only an agent in the configuration files may have one", i)
			}
		}
	}
	if a.Restricted() && a.Sandbox == "" {
		for _, t := range a.Tools {
			if !slices.Contains(UnsandboxedTools, t) {
				at("tools: %q is not allowed outside a sandbox: without one, an agent outside the configuration files runs in orbit's container (allowed: %s)", t, strings.Join(UnsandboxedTools, ", "))
			}
		}
	}
	return errs
}

// ruleRank orders rules by how readily they execute.
func ruleRank(r Rule) int {
	switch r {
	case RuleAuto:
		return 2
	case RuleApprove:
		return 1
	}
	return 0
}

// Widening lists how next reaches further than cur (nil: an agent that does not exist yet): a tool, source, credential, delegate target, need or connector it adds, a source it changes, a sandbox profile it moves to, an action type it allows more readily, or a WebFetch domain it adds (or the allowlist it drops). agent:define applies such a change only with a person's approval, whatever its rule.
func Widening(cur, next *Agent) []string {
	if cur == nil {
		cur = &Agent{}
	}
	var out []string
	for _, t := range next.Tools {
		if !slices.Contains(cur.Tools, t) {
			out = append(out, "tool "+t)
		}
	}
	var curRefs []string
	for _, s := range cur.Sources {
		curRefs = append(curRefs, s.EnvRefs()...)
	}
	var refs []string
	for _, s := range next.Sources {
		if !slices.ContainsFunc(cur.Sources, func(c Source) bool { return reflect.DeepEqual(c, s) }) {
			out = append(out, "source "+s.Name)
		}
		for _, r := range s.EnvRefs() {
			if !slices.Contains(curRefs, r) && !slices.Contains(refs, r) {
				refs = append(refs, r)
				out = append(out, "credential "+r)
			}
		}
	}
	types := make([]string, 0, len(next.Actions))
	for t := range next.Actions {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		if ruleRank(next.RuleFor(t)) > ruleRank(cur.RuleFor(t)) {
			out = append(out, fmt.Sprintf("action %s: %s, was %s", t, next.RuleFor(t), cur.RuleFor(t)))
		}
	}
	if next.Sandbox != "" && next.Sandbox != cur.Sandbox {
		// A profile's tools and egress are its own (SandboxProfiles), wider than orbit's container allows.
		out = append(out, "sandbox "+next.Sandbox)
	}
	out = append(out, webWidening(cur, next)...)
	for _, d := range next.Delegates {
		if !slices.Contains(cur.Delegates, d) && !slices.Contains(cur.Delegates, "*") {
			out = append(out, "delegate to "+d)
		}
	}
	for _, n := range next.Needs {
		if !slices.Contains(cur.Needs, n) {
			out = append(out, "need "+n)
		}
	}
	for _, n := range next.Connectors {
		if !slices.Contains(cur.Connectors, n) {
			// Its tools run as the agent's, on the person's account at that server.
			out = append(out, "connector "+n)
		}
	}
	for _, t := range next.Triggers {
		// A webhook lets an outside sender start unattended runs with a body it wrote.
		if t.IsWebhook() && !slices.ContainsFunc(cur.Triggers, func(c Trigger) bool { return c.Webhook == t.Webhook && c.Secret == t.Secret }) {
			out = append(out, "webhook "+t.Webhook)
		}
	}
	return out
}
