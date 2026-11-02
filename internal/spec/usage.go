package spec

import (
	"fmt"
	"net/url"
	"slices"
	"time"
)

// UsagePoll (orbit.yaml `usage_poll`) asks the endpoint Claude Code's own
// /usage reads for the subscription's windows, every Every, with the
// subscription's OAuth token (internal/usage). Off unless the block is
// present. Best effort: the endpoint is undocumented, so a change to it
// makes polls fail (recorded as failed samples) and /usage falls back to
// what runs' streams reported.
type UsagePoll struct {
	// Every is the interval (default DefaultUsagePollEvery).
	Every Duration `yaml:"every,omitempty" json:"every,omitempty"`
	// TokenEnv names the variable holding the OAuth token (default
	// CLAUDE_CODE_OAUTH_TOKEN, the one claude-code runners use).
	TokenEnv string `yaml:"token_env,omitempty" json:"token_env,omitempty"`
	// URL overrides DefaultUsageURL (tests).
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
}

// DefaultUsageURL is the endpoint Claude Code reads for its /usage screen.
const DefaultUsageURL = "https://api.anthropic.com/api/oauth/usage"

// DefaultUsagePollEvery is the poll interval unless configured: a five-hour
// window moves a few percent in five minutes of heavy use, and a request
// every five minutes is less than one run makes.
const DefaultUsagePollEvery = 5 * time.Minute

// MinUsagePollEvery bounds the interval from below, so a typo cannot hammer
// an endpoint orbit has no agreement to call.
const MinUsagePollEvery = time.Minute

// Agent priorities. A low-priority agent's scheduled runs wait while the
// subscription is nearly spent, when the deployment defers them
// (defer_low_priority).
const (
	PriorityNormal = "normal"
	PriorityLow    = "low"
)

// Priorities lists every priority.
var Priorities = []string{PriorityNormal, PriorityLow}

func (c *Config) applyUsageDefaults() {
	d := &c.Deployment
	if p := d.UsagePoll; p != nil {
		if p.Every == 0 {
			p.Every = Duration(DefaultUsagePollEvery)
		}
		if p.TokenEnv == "" {
			p.TokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"
		}
		if p.URL == "" {
			p.URL = DefaultUsageURL
		}
	}
}

// Low reports whether the agent is priority: low. An unset priority is
// normal and stays unset, so adding the field changed no agent's spec hash
// (a changed hash starts every session fresh).
func (a *Agent) Low() bool { return a.Priority == PriorityLow }

func (c *Config) validateUsage() []error {
	var errs []error
	if p := c.Deployment.UsagePoll; p != nil {
		if time.Duration(p.Every) < MinUsagePollEvery {
			errs = append(errs, fmt.Errorf("usage_poll: every is %s; at least %s", time.Duration(p.Every), MinUsagePollEvery))
		}
		if u, err := url.Parse(p.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, fmt.Errorf("usage_poll: url %q is not an http(s) URL", p.URL))
		}
	}
	for _, a := range c.Agents {
		if a.Priority != "" && !slices.Contains(Priorities, a.Priority) {
			errs = append(errs, fmt.Errorf("agent %q: priority %q is not one of %v", a.Name, a.Priority, Priorities))
		}
	}
	return errs
}
