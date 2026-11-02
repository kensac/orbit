package spec

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

// A webhook trigger starts a run when a signed delivery reaches POST /hooks/{agent}/{webhook} (internal/api/hooks.go). Its secret is an environment variable orbit.yaml binds to the agents that may use it, so an agent written through the API cannot name an arbitrary variable as a key, and the sender's body reaches the run only as the input of a `webhook` source.

// WebhookSecret is a signing secret inbound webhooks are verified with: the environment variable named by its key in orbit.yaml's webhook_secrets, the sender's signature scheme, and the agents whose webhook triggers may name it.
type WebhookSecret struct {
	// Scheme is how the sender signs: "github" (X-Hub-Signature-256, X-GitHub-Delivery) or "standard" (Standard Webhooks: webhook-id, webhook-timestamp, webhook-signature).
	Scheme string `yaml:"scheme" json:"scheme"`
	// Agents are the agents whose webhook triggers may name this secret.
	Agents []string `yaml:"agents" json:"agents"`
}

// WebhookSchemes are the signature schemes a webhook secret may name.
var WebhookSchemes = []string{"github", "standard"}

// Bounds on an inbound delivery's body. GitHub caps a payload at 25 MB, but the body becomes a run's input, read by a model, so the default is what a typical event needs and the most is a source's default bound (applyDefaults).
const (
	DefaultWebhookMaxBytes = 1 << 20
	MaxWebhookMaxBytes     = 8 << 20
)

// WebhookTolerance is how far a sender's signed timestamp may be from orbit's clock before a delivery is refused as a replay: Standard Webhooks' recommended tolerance.
const WebhookTolerance = 5 * time.Minute

// ErrWebhookTrigger is TriggerSchedule's answer for a webhook trigger: it has no schedule, and the scheduler passes it by.
var ErrWebhookTrigger = errors.New("a webhook trigger has no schedule")

// IsWebhook reports whether the trigger is a webhook's.
func (t Trigger) IsWebhook() bool { return t.Webhook != "" }

// WebhookMaxBytes is the most a delivery's body may be for this trigger.
func (t Trigger) WebhookMaxBytes() int64 {
	if t.MaxBytes > 0 {
		return t.MaxBytes
	}
	return DefaultWebhookMaxBytes
}

// WebhookTrigger finds the agent's webhook trigger named name, and its index in Triggers.
func (a *Agent) WebhookTrigger(name string) (Trigger, int, bool) {
	for i, t := range a.Triggers {
		if t.Webhook == name {
			return t, i, true
		}
	}
	return Trigger{}, -1, false
}

func (c *Config) validateWebhookSecrets() []error {
	var errs []error
	names := make([]string, 0, len(c.Deployment.WebhookSecrets))
	for n := range c.Deployment.WebhookSecrets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ws := c.Deployment.WebhookSecrets[n]
		if !credentialNameRE.MatchString(n) {
			errs = append(errs, fmt.Errorf("webhook secret %q: the name is an environment variable (%s)", n, credentialNameRE))
		}
		if !slices.Contains(WebhookSchemes, ws.Scheme) {
			errs = append(errs, fmt.Errorf("webhook secret %q: scheme must be one of %v", n, WebhookSchemes))
		}
		if len(ws.Agents) == 0 {
			errs = append(errs, fmt.Errorf("webhook secret %q: agents must name the agents that may use it", n))
		}
		for _, a := range ws.Agents {
			if !nameRE.MatchString(a) {
				errs = append(errs, fmt.Errorf("webhook secret %q: %q is not an agent name", n, a))
			}
		}
	}
	return errs
}

// validateWebhookTrigger checks trigger i of a, a webhook trigger.
func (c *Config) validateWebhookTrigger(a *Agent, i int, t Trigger) []error {
	var errs []error
	at := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agent %q: trigger %d: "+format, append([]any{a.Name, i}, args...)...))
	}
	if !nameRE.MatchString(t.Webhook) {
		at("webhook %q must match %s (it is the route's last segment)", t.Webhook, nameRE)
	}
	if _, first, _ := a.WebhookTrigger(t.Webhook); first != i {
		at("webhook %q is named by trigger %d already", t.Webhook, first)
	}
	if t.Schedule != "" || t.RRule != "" || t.Start != "" {
		at("a webhook trigger takes no schedule, rrule or start")
	}
	ws, ok := c.Deployment.WebhookSecrets[t.Secret]
	switch {
	case t.Secret == "":
		at("a webhook trigger needs a secret, a variable in orbit.yaml's webhook_secrets")
	case !ok:
		at("secret %s is not in orbit.yaml's webhook_secrets", t.Secret)
	case !slices.Contains(ws.Agents, a.Name):
		at("secret %s is not bound to this agent (webhook_secrets.%s.agents)", t.Secret, t.Secret)
	}
	if t.MaxBytes < 0 || t.MaxBytes > MaxWebhookMaxBytes {
		at("max_bytes must be between 0 (the default, %d) and %d", DefaultWebhookMaxBytes, MaxWebhookMaxBytes)
	}
	return errs
}

// hasWebhook reports whether any of a's triggers is a webhook's.
func (a *Agent) hasWebhook() bool {
	return slices.ContainsFunc(a.Triggers, Trigger.IsWebhook)
}
