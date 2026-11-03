package spec

import (
	"fmt"
	"slices"
	"time"
)

// Telegram configures private Telegram messages as a two-way conversation surface.
type Telegram struct {
	Channel    string   `yaml:"channel" json:"channel"`
	Agent      string   `yaml:"agent" json:"agent"`
	AllowUsers []int64  `yaml:"allow_users" json:"allow_users"`
	Poll       Duration `yaml:"poll,omitempty" json:"poll,omitempty"`
}

const DefaultTelegramPoll = 5 * time.Second

func (c *Config) applyTelegramDefaults() {
	if c.Deployment.Telegram != nil && c.Deployment.Telegram.Poll == 0 {
		c.Deployment.Telegram.Poll = Duration(DefaultTelegramPoll)
	}
}

func (c *Config) validateTelegram() []error {
	t := c.Deployment.Telegram
	if t == nil {
		return nil
	}
	var errs []error
	at := func(format string, args ...any) { errs = append(errs, fmt.Errorf("telegram: "+format, args...)) }
	ch, ok := c.Deployment.Channels[t.Channel]
	if t.Channel == "" || !ok || ch.Kind != "telegram" {
		at("channel %q must name a telegram channel", t.Channel)
	}
	if _, ok := c.Agents[t.Agent]; t.Agent == "" || !ok {
		at("unknown agent %q", t.Agent)
	}
	if len(t.AllowUsers) == 0 {
		at("allow_users must contain at least one Telegram user id")
	}
	seen := map[int64]bool{}
	for _, id := range t.AllowUsers {
		if id <= 0 || seen[id] {
			at("allow_users must contain unique positive user ids")
			break
		}
		seen[id] = true
	}
	if time.Duration(t.Poll) < time.Second {
		at("poll %s is shorter than 1s", time.Duration(t.Poll))
	}
	return errs
}

// AllowsTelegramUser reports whether a sender is explicitly allowed.
func (t Telegram) AllowsTelegramUser(id int64) bool { return slices.Contains(t.AllowUsers, id) }
