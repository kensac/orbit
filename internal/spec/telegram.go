package spec

import (
	"fmt"
	"slices"
)

// Telegram configures Orbit's Telegram bot reader. The bot token is named by
// the selected notification channel and is read only by Orbit's service.
type Telegram struct {
	Channel    string  `yaml:"channel" json:"channel"`
	Agent      string  `yaml:"agent" json:"agent"`
	AllowUsers []int64 `yaml:"allow_users" json:"allow_users"`
}

func (c *Config) validateTelegram() []error {
	t := c.Deployment.Telegram
	if t == nil {
		return nil
	}
	var errs []error
	at := func(format string, args ...any) { errs = append(errs, fmt.Errorf("telegram: "+format, args...)) }
	ch, ok := c.Deployment.Channels[t.Channel]
	if !ok {
		at("channel %q is not configured", t.Channel)
	} else if ch.Kind != "telegram" {
		at("channel %q must have kind telegram", t.Channel)
	}
	if _, ok := c.Agents[t.Agent]; !ok {
		at("unknown agent %q", t.Agent)
	}
	if len(t.AllowUsers) == 0 {
		at("allow_users must list at least one Telegram user id")
	}
	seen := map[int64]bool{}
	for _, id := range t.AllowUsers {
		if id <= 0 {
			at("allow_users id %d must be positive", id)
		}
		if seen[id] {
			at("allow_users repeats id %d", id)
		}
		seen[id] = true
	}
	return errs
}

// AllowsUser reports whether Telegram's sender id is allowed to message Orbit.
func (t Telegram) AllowsUser(id int64) bool { return slices.Contains(t.AllowUsers, id) }
