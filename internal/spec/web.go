package spec

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
)

// WebSpec limits WebFetch to the domains an agent names, for agents that run on orbit's host (a sandboxed agent's network is its egress profile). Without it a listed WebFetch reaches any host.
type WebSpec struct {
	// Allow are the hosts WebFetch may fetch from: a name (example.com) or every subdomain of one (*.example.com), as Claude Code's WebFetch(domain:...) rules match them.
	Allow []string `yaml:"allow" json:"allow"`
}

// MaxWebDomains bounds web.allow.
const MaxWebDomains = 50

// webDomainRE is a lowercase DNS name with at least two labels, optionally behind "*.".
var webDomainRE = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// PrivateWebNames are the names WebFetch is denied whenever an agent has web.allow: loopback, link-local metadata, and the suffixes of names that resolve inside a network (mDNS, the tailnet's MagicDNS, home and corporate zones). Claude Code's domain rules match the URL's host, never the address it resolves to, so an allowed public name that resolves to a private address is not caught here; only a sandbox's egress, which never dials one, catches that.
var PrivateWebNames = []string{
	"localhost", "*.localhost", "127.0.0.1", "0.0.0.0", "[::1]", "169.254.169.254", "metadata.google.internal",
	"*.local", "*.internal", "*.lan", "*.home.arpa", "*.ts.net",
}

// privateWebName reports whether d is or is under one of PrivateWebNames.
func privateWebName(d string) bool {
	d = strings.TrimPrefix(d, "*.")
	for _, p := range PrivateWebNames {
		if suffix, ok := strings.CutPrefix(p, "*."); ok {
			if d == suffix || strings.HasSuffix(d, "."+suffix) {
				return true
			}
		} else if d == p {
			return true
		}
	}
	return false
}

func (a *Agent) validateWeb() []error {
	w := a.Web
	if w == nil {
		return nil
	}
	var errs []error
	at := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agent %q: web: "+format, append([]any{a.Name}, args...)...))
	}
	if a.Sandbox != "" {
		at("a sandboxed agent's network is its sandbox's egress, not web.allow")
	}
	if !slices.Contains(a.Tools, "WebFetch") {
		at("allow needs WebFetch in tools")
	}
	if len(w.Allow) == 0 {
		at("allow names no domain (leave web out for none, or remove WebFetch)")
	}
	if len(w.Allow) > MaxWebDomains {
		at("allow has %d domains, at most %d", len(w.Allow), MaxWebDomains)
	}
	for _, d := range w.Allow {
		switch {
		case net.ParseIP(strings.Trim(d, "[]")) != nil:
			at("%q is an address: name a host", d)
		case !webDomainRE.MatchString(d):
			at("%q is not a lowercase domain (example.com, or *.example.com for its subdomains)", d)
		case privateWebName(d):
			at("%q is a private name, which WebFetch is always denied", d)
		}
	}
	return errs
}

// webWidening is how next's web reaches further than cur's, for an agent that has WebFetch: no allowlist where there was one, or a domain added to it.
func webWidening(cur, next *Agent) []string {
	if !slices.Contains(next.Tools, "WebFetch") || next.Sandbox != "" {
		return nil
	}
	curAny := slices.Contains(cur.Tools, "WebFetch") && (cur.Web == nil || cur.Sandbox != "")
	if next.Web == nil {
		if cur.Web != nil && !curAny {
			return []string{"web: any domain"}
		}
		return nil
	}
	if curAny || !slices.Contains(cur.Tools, "WebFetch") {
		return nil // the tool itself is reported, or it reached every domain already
	}
	var out []string
	for _, d := range next.Web.Allow {
		if cur.Web == nil || !slices.Contains(cur.Web.Allow, d) {
			out = append(out, "web domain "+d)
		}
	}
	return out
}
