// Package notify delivers messages to people. A channel is an adapter plus
// its settings from orbit.yaml; adding a destination means adding an adapter
// here and its kind to spec's list.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/spec"
)

// Priority is how urgently a person should see a message.
type Priority string

// Priorities, in ntfy's vocabulary since it is the richest adapter.
const (
	Low     Priority = "low"
	Default Priority = "default"
	High    Priority = "high"
)

// Button is a link a person can follow from the notification.
type Button struct {
	Label string
	URL   string
}

// Message is what a channel delivers.
type Message struct {
	Title    string
	Body     string // markdown
	Priority Priority
	Tags     []string
	Link     string
	Buttons  []Button
	// Mail headers, used by smtp only. ReplyTo and MessageID name the conversation (internal/mail), so a reply continues it; InReplyTo is the person's mail a report answers; References threads them; Subject, when set, replaces Title as the subject.
	ReplyTo, MessageID, InReplyTo, References, Subject string
	// To, when set, replaces the channel's recipients (smtp only): a reply to a mail goes to its sender.
	To []string `json:",omitempty"`
	// TelegramChatID routes a direct reply to the private chat that sent the message.
	TelegramChatID int64 `json:"telegram_chat_id,omitempty"`
}

// Channel delivers messages.
type Channel interface {
	Send(ctx context.Context, m Message) error
}

// Env resolves environment variables.
type Env func(string) (string, bool)

// New builds a channel from its spec.
func New(name string, c spec.Channel, env Env, client *http.Client) (Channel, error) {
	need := func(field, v string) (string, error) {
		if v == "" {
			return "", nil
		}
		s, ok := env(v)
		if !ok || s == "" {
			return "", fmt.Errorf("channel %q: %s %s is not set", name, field, v)
		}
		return s, nil
	}
	switch c.Kind {
	case "ntfy":
		topic := c.Topic
		if c.TopicEnv != "" {
			t, err := need("topic_env", c.TopicEnv)
			if err != nil {
				return nil, err
			}
			topic = t
		}
		token, err := need("token_env", c.TokenEnv)
		if err != nil {
			return nil, err
		}
		return &ntfy{url: strings.TrimRight(c.URL, "/") + "/" + topic, token: token, client: client}, nil
	case "smtp":
		user, err := need("username_env", c.UsernameEnv)
		if err != nil {
			return nil, err
		}
		pass, err := need("password_env", c.PasswordEnv)
		if err != nil {
			return nil, err
		}
		return &smtpChannel{host: c.Host, port: c.Port, user: user, pass: pass, from: c.From, to: c.To}, nil
	case "telegram":
		token, err := need("token_env", c.TokenEnv)
		if err != nil {
			return nil, err
		}
		return &telegramChannel{token: token, chatID: c.ChatID, client: client}, nil
	case "webhook":
		h := map[string]string{}
		for k, v := range c.Headers {
			rv, err := spec.ExpandEnv(v, env)
			if err != nil {
				return nil, fmt.Errorf("channel %q header %s: %w", name, k, err)
			}
			h[k] = rv
		}
		return &webhook{url: c.URL, headers: h, client: client}, nil
	case "log":
		return logChannel{name: name}, nil
	}
	return nil, fmt.Errorf("channel %q: unknown kind %q", name, c.Kind)
}

type telegramChannel struct {
	token  string
	chatID int64
	client *http.Client
	api    string
}

func (t *telegramChannel) Send(ctx context.Context, m Message) error {
	id := m.TelegramChatID
	if id == 0 {
		id = t.chatID
	}
	if id == 0 {
		return fmt.Errorf("telegram channel has no target chat")
	}
	body, err := json.Marshal(map[string]any{"chat_id": id, "text": telegramText(m)})
	if err != nil {
		return err
	}
	api := t.api
	if api == "" {
		api = "https://api.telegram.org"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(api, "/")+"/bot"+t.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram send request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram send request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram send: %s", resp.Status)
	}
	return nil
}

func telegramText(m Message) string {
	text := m.Body
	if text == "" {
		text = m.Title
	} else if m.Title != "" {
		text = m.Title + "\n\n" + text
	}
	if m.Link != "" {
		text += "\n\n" + m.Link
	}
	const limit = 4000
	if len(text) > limit {
		text = truncate(text, limit-32) + "\n\n(continued in orbit)"
	}
	return text
}

// ---------------------------------------------------------------- ntfy

type ntfy struct {
	url, token string
	client     *http.Client
}

// ntfyMaxBody is below ntfy's default message limit (4096 bytes); a longer
// body is turned into an attachment by ntfy, which phones show as a file
// rather than text. The full report stays in orbit and the link opens it.
const ntfyMaxBody = 3900

func (n *ntfy) Send(ctx context.Context, m Message) error {
	body := m.Body
	if len(body) > ntfyMaxBody {
		body = truncate(body, ntfyMaxBody) + "\n\n(continued in orbit)"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", headerSafe(m.Title))
	req.Header.Set("Markdown", "yes")
	if m.Priority != "" {
		req.Header.Set("Priority", string(m.Priority))
	}
	if len(m.Tags) > 0 {
		req.Header.Set("Tags", strings.Join(m.Tags, ","))
	}
	if m.Link != "" {
		req.Header.Set("Click", m.Link)
	}
	if len(m.Buttons) > 0 {
		var acts []string
		for i, b := range m.Buttons {
			if i == 3 { // ntfy allows three actions
				break
			}
			acts = append(acts, "view, "+strings.ReplaceAll(b.Label, ",", " ")+", "+b.URL)
		}
		req.Header.Set("Actions", strings.Join(acts, "; "))
	}
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	return do(n.client, req)
}

// ---------------------------------------------------------------- webhook

type webhook struct {
	url     string
	headers map[string]string
	client  *http.Client
}

func (w *webhook) Send(ctx context.Context, m Message) error {
	b, err := json.Marshal(map[string]any{
		"title": m.Title, "body": m.Body, "priority": m.Priority, "tags": m.Tags, "link": m.Link,
		"buttons": m.Buttons,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}
	return do(w.client, req)
}

// ---------------------------------------------------------------- smtp

type smtpChannel struct {
	host       string
	port       int
	user, pass string
	from       string
	to         []string
}

func (s *smtpChannel) Send(ctx context.Context, m Message) error {
	to := s.to
	if len(m.To) > 0 {
		to = m.To
	}
	var body strings.Builder
	body.WriteString(m.Body)
	if m.Link != "" {
		body.WriteString("\n\n" + m.Link)
	}
	for _, b := range m.Buttons {
		fmt.Fprintf(&body, "\n%s: %s", b.Label, b.URL)
	}
	subject := m.Title
	if m.Subject != "" {
		subject = m.Subject
	}
	hdr := []string{
		"From: " + s.from,
		"To: " + headerLine(strings.Join(to, ", ")),
		"Subject: " + mime.QEncoding.Encode("utf-8", headerLine(subject)),
		"Date: " + time.Now().Format(time.RFC1123Z),
	}
	for _, h := range [][2]string{{"Reply-To", m.ReplyTo}, {"Message-ID", m.MessageID},
		{"In-Reply-To", m.InReplyTo}, {"References", m.References}} {
		if v := headerLine(h[1]); v != "" {
			hdr = append(hdr, h[0]+": "+v)
		}
	}
	hdr = append(hdr, "MIME-Version: 1.0", "Content-Type: text/plain; charset=utf-8", "Content-Transfer-Encoding: 8bit",
		"", strings.ReplaceAll(body.String(), "\n", "\r\n"))
	msg := strings.Join(hdr, "\r\n")

	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	d := net.Dialer{Timeout: 30 * time.Second}
	var conn net.Conn
	var err error
	if s.port == 465 {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: s.host})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Close()
	if s.port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: s.host}); err != nil {
				return err
			}
		} else if s.user != "" {
			// Never send credentials over a connection that refused TLS.
			return fmt.Errorf("smtp %s: server does not offer STARTTLS", addr)
		}
	}
	if s.user != "" {
		if err := c.Auth(smtp.PlainAuth("", s.user, s.pass, s.host)); err != nil {
			return err
		}
	}
	if err := c.Mail(addrOnly(s.from)); err != nil {
		return err
	}
	for _, t := range to {
		if err := c.Rcpt(addrOnly(t)); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// headerLine keeps a header value on one line, so a value can never add a header of its own.
func headerLine(v string) string { return strings.Join(strings.Fields(v), " ") }

func addrOnly(a string) string {
	if i := strings.LastIndexByte(a, '<'); i >= 0 {
		return strings.Trim(a[i:], "<>")
	}
	return a
}

// ---------------------------------------------------------------- log

type logChannel struct{ name string }

func (l logChannel) Send(_ context.Context, m Message) error {
	slog.Info("notification", "channel", l.name, "title", m.Title, "priority", m.Priority, "link", m.Link, "body", m.Body)
	return nil
}

// ---------------------------------------------------------------- helpers

func do(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", req.Method, req.URL.Host, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// headerSafe makes a title fit an HTTP header: one line, ASCII-safe. ntfy
// decodes RFC 2047, so non-ASCII titles are encoded rather than dropped.
func headerSafe(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for _, r := range s {
		if r > 126 {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}
