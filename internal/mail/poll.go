package mail

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// MaxMailBytes is the largest mail orbit reads: past it the mail is recorded as ignored with its envelope only. 25 MiB is the common provider limit on a sent mail, so a person's reply fits.
const MaxMailBytes = 25 << 20

// perPoll bounds the mails one poll reads; the rest wait for the next.
const perPoll = 50

// Poller reads orbit's mailbox and acts on each unseen mail once.
type Poller struct {
	Store  *store.Store
	Config func() *spec.Config
	// Secret signs conversation tokens (the operator token, as for approval links).
	Secret []byte
	Env    func(string) (string, bool)
	Log    *slog.Logger
	// Wake nudges the executor when a mail woke a run.
	Wake func()
	// Open reaches the mailbox; nil is IMAP as configured.
	Open func(ctx context.Context, m *spec.Mail, user, pass string) (Mailbox, error)
}

// Mailbox is the folder orbit reads.
type Mailbox interface {
	// Unseen returns up to n unseen mails, oldest first.
	Unseen(n int) ([]Fetched, error)
	// MarkSeen flags mails orbit has acted on, so they are not read again.
	MarkSeen(uids []uint32) error
	Close() error
}

// Fetched is one mail from the mailbox. Raw is nil for one larger than MaxMailBytes, which carries only its envelope's Message-ID and sender.
type Fetched struct {
	UID       uint32
	Raw       []byte
	Size      uint32
	MessageID string
	From      string
	Subject   string
}

// Run polls until ctx ends, at the configured interval; a configuration without `mail` polls nothing and checks again a minute later.
func (p *Poller) Run(ctx context.Context) {
	for ctx.Err() == nil {
		every := spec.DefaultMailPoll
		if m := p.Config().Deployment.Mail; m != nil {
			every = time.Duration(m.Poll)
			if err := p.Poll(ctx); err != nil && ctx.Err() == nil {
				p.Log.Warn("mail poll", "err", err)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(every):
		}
	}
}

// Poll reads the mailbox once.
func (p *Poller) Poll(ctx context.Context) error {
	cfg := p.Config()
	m := cfg.Deployment.Mail
	if m == nil {
		return nil
	}
	user, _ := p.Env(m.IMAP.UsernameEnv)
	pass, _ := p.Env(m.IMAP.PasswordEnv)
	if user == "" || pass == "" {
		return fmt.Errorf("mail: %s or %s is not set", m.IMAP.UsernameEnv, m.IMAP.PasswordEnv)
	}
	open := p.Open
	if open == nil {
		open = OpenIMAP
	}
	mb, err := open(ctx, m, user, pass)
	if err != nil {
		return err
	}
	defer mb.Close()
	mails, err := mb.Unseen(perPoll)
	if err != nil {
		return err
	}
	var done []uint32
	var firstErr error
	for _, f := range mails {
		ok, err := p.Handle(ctx, cfg, f)
		if err != nil {
			p.Log.Warn("mail", "message_id", f.MessageID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
		if ok {
			done = append(done, f.UID)
		}
	}
	if len(done) > 0 {
		if err := mb.MarkSeen(done); err != nil {
			return err
		}
	}
	return firstErr
}

// Handle acts on one mail. done is true once the mail is recorded (posted or ignored), so it may be marked seen; an error with done false is transient (the database) and the mail is read again next poll.
func (p *Poller) Handle(ctx context.Context, cfg *spec.Config, f Fetched) (done bool, err error) {
	m := cfg.Deployment.Mail
	rec := store.MailIn{MessageID: f.MessageID, From: strings.ToLower(f.From), Subject: f.Subject, Raw: f.Raw}
	ignore := func(reason string) (bool, error) {
		if rec.MessageID == "" {
			sum := sha256.Sum256(f.Raw)
			rec.MessageID = "sha256:" + hex.EncodeToString(sum[:])
		}
		if rec.Raw == nil {
			rec.Raw = []byte{}
		}
		p.Log.Info("mail ignored", "message_id", rec.MessageID, "from", rec.From, "reason", reason)
		if err := p.Store.IgnoreMail(ctx, rec, reason); err != nil {
			return false, err
		}
		return true, nil
	}
	if f.Raw == nil {
		return ignore(fmt.Sprintf("larger than %d MiB (%d bytes)", MaxMailBytes>>20, f.Size))
	}
	in, err := Parse(f.Raw, m.Address)
	if err != nil {
		return ignore(err.Error())
	}
	rec.MessageID, rec.From, rec.Subject = in.MessageID, in.From, in.Subject
	if len(in.Attachments) > 0 {
		rec.Attachments, _ = json.Marshal(in.Attachments)
	}
	if _, seen, err := p.Store.MailSeen(ctx, in.MessageID); err != nil || seen {
		return err == nil, err
	}
	if !slices.Contains(cfg.Deployment.AllowedSenders(), in.From) {
		return ignore("sender " + in.From + " is not allowed (mail.allow)")
	}
	if in.Rejected != "" {
		return ignore(in.Rejected)
	}

	var agent *spec.Agent
	for _, t := range in.Tokens {
		id := Conversation(p.Secret, t)
		if id == "" {
			continue
		}
		c, err := p.Store.GetConversation(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		a, ok := cfg.Agents[c.Agent]
		if !ok {
			return ignore("the conversation's agent " + strconv.Quote(c.Agent) + " is not configured")
		}
		agent, rec.ConversationID = a, c.ID
		break
	}
	if agent == nil {
		if m.Agent == "" {
			return ignore("not a reply to an orbit conversation, and mail.agent is unset")
		}
		agent = cfg.Agents[m.Agent]
		rec.Title = store.DefaultTitle(cleanSubject(in.Subject))
	}
	body := in.Text
	if body == "" && rec.ConversationID == "" {
		body = cleanSubject(in.Subject)
	}
	if body == "" && len(in.Attachments) == 0 {
		return ignore("no text")
	}
	if len(in.Attachments) > 0 {
		var names []string
		for _, a := range in.Attachments {
			names = append(names, fmt.Sprintf("%s (%s, %d bytes)", orUnnamed(a.Name), a.Type, a.Size))
		}
		body = strings.TrimSpace(body + "\n\n(Attached to the mail, kept in orbit's record of it: " + strings.Join(names, "; ") + ")")
	}
	rec.Agent, rec.SpecSHA, rec.Body, rec.Wake = agent.Name, agent.SHA, body, agent.MessagesTrigger
	res, replayed, err := p.Store.IngestMail(ctx, rec)
	switch {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrInvalid),
		errors.Is(err, store.ErrReplyElsewhere):
		return ignore(err.Error())
	case err != nil:
		return false, err
	}
	p.Log.Info("mail posted", "message_id", in.MessageID, "agent", agent.Name, "conversation", res.ConversationID,
		"run", res.RunID, "replayed", replayed)
	if res.RunID != "" && !replayed && p.Wake != nil {
		p.Wake()
	}
	return true, nil
}

func orUnnamed(s string) string {
	if s == "" {
		return "unnamed"
	}
	return s
}

var replyPrefixRE = regexp.MustCompile(`(?i)^\s*((re|fwd?|aw|wg|sv|antw)\s*(\[\d+\])?\s*:\s*)+`)

// cleanSubject drops reply and forward prefixes.
func cleanSubject(s string) string { return strings.TrimSpace(replyPrefixRE.ReplaceAllString(s, "")) }

// ---------------------------------------------------------------- IMAP

// imapBox is a selected IMAP folder.
type imapBox struct{ c *client.Client }

// OpenIMAP logs in to the configured server: implicit TLS on port 993, STARTTLS on any other (a server that does not offer it is refused, so a password never crosses in the clear), then selects the mailbox.
func OpenIMAP(ctx context.Context, m *spec.Mail, user, pass string) (Mailbox, error) {
	addr := net.JoinHostPort(m.IMAP.Host, strconv.Itoa(m.IMAP.Port))
	tc := &tls.Config{ServerName: m.IMAP.Host, MinVersion: tls.VersionTLS12}
	return openIMAP(ctx, addr, tc, m.IMAP.Port != 993, user, pass, m.IMAP.Mailbox)
}

// openIMAP is OpenIMAP's work; a nil tc is plaintext, for tests only.
func openIMAP(ctx context.Context, addr string, tc *tls.Config, startTLS bool, user, pass, mailbox string) (Mailbox, error) {
	d := &net.Dialer{Timeout: 30 * time.Second}
	var c *client.Client
	var err error
	if tc != nil && !startTLS {
		c, err = client.DialWithDialerTLS(d, addr, tc)
	} else {
		c, err = client.DialWithDialer(d, addr)
	}
	if err != nil {
		return nil, fmt.Errorf("imap %s: %w", addr, err)
	}
	// Every command is bounded, so a stalled server cannot hold the poll.
	c.Timeout = 2 * time.Minute
	if dl, ok := ctx.Deadline(); ok {
		c.Timeout = time.Until(dl)
	}
	if tc != nil && startTLS {
		if ok, _ := c.SupportStartTLS(); !ok {
			_ = c.Logout()
			return nil, fmt.Errorf("imap %s: server does not offer STARTTLS", addr)
		}
		if err := c.StartTLS(tc); err != nil {
			_ = c.Logout()
			return nil, fmt.Errorf("imap %s: starttls: %w", addr, err)
		}
	}
	if err := c.Login(user, pass); err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("imap %s: login: %w", addr, err)
	}
	if _, err := c.Select(mailbox, false); err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("imap %s: select %s: %w", addr, mailbox, err)
	}
	return &imapBox{c: c}, nil
}

func (b *imapBox) Unseen(n int) ([]Fetched, error) {
	crit := imap.NewSearchCriteria()
	crit.WithoutFlags = []string{imap.SeenFlag}
	uids, err := b.c.UidSearch(crit)
	if err != nil {
		return nil, fmt.Errorf("imap search: %w", err)
	}
	slices.Sort(uids)
	if len(uids) > n {
		uids = uids[:n]
	}
	if len(uids) == 0 {
		return nil, nil
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	var out []Fetched
	if err := b.fetch(set, []imap.FetchItem{imap.FetchUid, imap.FetchRFC822Size, imap.FetchEnvelope}, func(m *imap.Message) {
		f := Fetched{UID: m.Uid, Size: m.Size}
		if e := m.Envelope; e != nil {
			f.MessageID, f.Subject = e.MessageId, e.Subject
			if len(e.From) > 0 {
				f.From = e.From[0].Address()
			}
		}
		out = append(out, f)
	}); err != nil {
		return nil, err
	}
	small := new(imap.SeqSet)
	for _, f := range out {
		if f.Size <= MaxMailBytes {
			small.AddNum(f.UID)
		}
	}
	if small.Empty() {
		return out, nil
	}
	section := &imap.BodySectionName{Peek: true}
	bodies := map[uint32][]byte{}
	if err := b.fetch(small, []imap.FetchItem{imap.FetchUid, section.FetchItem()}, func(m *imap.Message) {
		if r := m.GetBody(section); r != nil {
			raw, _ := io.ReadAll(io.LimitReader(r, MaxMailBytes+1))
			bodies[m.Uid] = raw
		}
	}); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Raw = bodies[out[i].UID]
	}
	return out, nil
}

func (b *imapBox) fetch(set *imap.SeqSet, items []imap.FetchItem, each func(*imap.Message)) error {
	ch := make(chan *imap.Message, 16)
	done := make(chan error, 1)
	go func() { done <- b.c.UidFetch(set, items, ch) }()
	for m := range ch {
		each(m)
	}
	if err := <-done; err != nil {
		return fmt.Errorf("imap fetch: %w", err)
	}
	return nil
}

func (b *imapBox) MarkSeen(uids []uint32) error {
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	op := imap.FormatFlagsOp(imap.AddFlags, true)
	if err := b.c.UidStore(set, op, []any{imap.SeenFlag}, nil); err != nil {
		return fmt.Errorf("imap store \\Seen: %w", err)
	}
	return nil
}

func (b *imapBox) Close() error { return b.c.Logout() }
