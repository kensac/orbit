package spec

import (
	"fmt"
	"net/mail"
	"strings"
	"time"
)

// Mail is orbit's mailbox (orbit.yaml `mail`): mails orbit sends carry a reply address naming their conversation, and orbit polls the mailbox over IMAP to turn replies into messages there (internal/mail).
type Mail struct {
	// Address is orbit's own address. Replies go to its plus form, local+<token>@domain, so the mailbox must deliver plus addresses.
	Address string `yaml:"address" json:"address"`
	IMAP    IMAP   `yaml:"imap" json:"imap"`
	// Allow lists the senders whose mail orbit acts on (addresses, any case). Empty: every smtp channel's `to`, the people orbit mails.
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	// Agent receives a mail that is not a reply, in a new conversation. Empty: such mails are recorded and ignored.
	Agent string `yaml:"agent,omitempty" json:"agent,omitempty"`
	// Poll is how often the mailbox is read (default 1m).
	Poll Duration `yaml:"poll,omitempty" json:"poll,omitempty"`
}

// IMAP is how orbit reads its mailbox.
type IMAP struct {
	Host string `yaml:"host" json:"host"`
	// Port 993 is implicit TLS; any other port must offer STARTTLS.
	Port        int    `yaml:"port,omitempty" json:"port,omitempty"`
	UsernameEnv string `yaml:"username_env" json:"username_env"`
	PasswordEnv string `yaml:"password_env" json:"password_env"`
	// Mailbox is the folder read (default INBOX). Every unseen mail in it is read and marked seen, so it must be orbit's own.
	Mailbox string `yaml:"mailbox,omitempty" json:"mailbox,omitempty"`
}

// DefaultMailPoll is how often the mailbox is read unless configured: a reply is acted on within a minute, and a poll is one short IMAP session.
const DefaultMailPoll = time.Minute

// AllowedSenders is Allow, or the smtp channels' recipients, lower-cased.
func (d Deployment) AllowedSenders() []string {
	var out []string
	if d.Mail != nil && len(d.Mail.Allow) > 0 {
		for _, a := range d.Mail.Allow {
			out = append(out, strings.ToLower(addrOf(a)))
		}
		return out
	}
	for _, ch := range d.Channels {
		if ch.Kind == "smtp" {
			for _, t := range ch.To {
				out = append(out, strings.ToLower(addrOf(t)))
			}
		}
	}
	return out
}

func addrOf(s string) string {
	if a, err := mail.ParseAddress(s); err == nil {
		return a.Address
	}
	return strings.TrimSpace(s)
}

func (c *Config) applyMailDefaults() {
	m := c.Deployment.Mail
	if m == nil {
		return
	}
	if m.IMAP.Port == 0 {
		m.IMAP.Port = 993
	}
	if m.IMAP.Mailbox == "" {
		m.IMAP.Mailbox = "INBOX"
	}
	if m.Poll == 0 {
		m.Poll = Duration(DefaultMailPoll)
	}
}

func (c *Config) validateMail() []error {
	m := c.Deployment.Mail
	if m == nil {
		return nil
	}
	var errs []error
	at := func(format string, args ...any) { errs = append(errs, fmt.Errorf("mail: "+format, args...)) }
	if a, err := mail.ParseAddress(m.Address); err != nil || a.Name != "" || !strings.Contains(a.Address, "@") {
		at("address %q must be a bare address (orbit@example.com)", m.Address)
	} else if strings.Contains(a.Address[:strings.IndexByte(a.Address, '@')], "+") {
		at("address %q must not have a plus part: orbit adds its own", m.Address)
	}
	if m.IMAP.Host == "" || m.IMAP.UsernameEnv == "" || m.IMAP.PasswordEnv == "" {
		at("imap needs host, username_env and password_env")
	}
	if m.Agent != "" {
		if _, ok := c.Agents[m.Agent]; !ok {
			at("unknown agent %q", m.Agent)
		}
	}
	if time.Duration(m.Poll) < 10*time.Second {
		at("poll %s is shorter than 10s", time.Duration(m.Poll))
	}
	if len(c.Deployment.AllowedSenders()) == 0 {
		at("allow is empty and no smtp channel names a recipient: nobody could write to orbit")
	}
	return errs
}
