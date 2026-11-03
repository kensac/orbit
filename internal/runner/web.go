package runner

import "github.com/kensac/orbit/internal/spec"

// webRules turns an allowlist of tools into Claude Code rules for a run with web.allow: the bare WebFetch, which would let it fetch any URL, becomes one WebFetch(domain:<d>) allow rule per domain, and deny is the private names (spec.PrivateWebNames), passed as --disallowedTools, which win over any allow, a person's included. Without an allowlist both are unchanged. Seen on 2.1.316: in dontAsk an allowed domain and a "*." subdomain were fetched, another domain was denied, and a deny rule beat a wildcard allow.
func webRules(allowed []string, web []string) (out, deny []string) {
	if web == nil {
		return allowed, nil
	}
	for _, t := range allowed {
		if t != "WebFetch" {
			out = append(out, t)
		}
	}
	for _, d := range web {
		out = append(out, "WebFetch(domain:"+d+")")
	}
	for _, d := range spec.PrivateWebNames {
		deny = append(deny, "WebFetch(domain:"+d+")")
	}
	return out, deny
}
