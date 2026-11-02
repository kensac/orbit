package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

var ctx = context.Background()

var secret = []byte(strings.Repeat("t", 32))

// imapServer is a real IMAP server over the memory backend (its one user is username/password), on loopback.
func imapServer(t *testing.T) (addr string, inbox backend.Mailbox) {
	t.Helper()
	be := memory.New()
	s := server.New(be)
	s.AllowInsecureAuth = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Close() })
	u, err := be.Login(nil, "username", "password")
	if err != nil {
		t.Fatal(err)
	}
	inbox, err = u.GetMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	return ln.Addr().String(), inbox
}

func deliver(t *testing.T, inbox backend.Mailbox, mail []byte) {
	t.Helper()
	if err := inbox.CreateMessage(nil, time.Now(), bytes.NewReader(mail)); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	s    *store.Store
	cfg  *spec.Config
	p    *Poller
	woke int
}

func setup(t *testing.T, addr string) *fixture {
	t.Helper()
	s := testdb.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(`
public_url: http://orbit.test
defaults: {runner: r, channels: [email]}
runners: {r: {kind: claude-code}}
channels:
  email: {kind: smtp, host: mail.test, port: 587, from: orbit <orbit@example.com>, to: ["Kanishk <me@example.com>"]}
mail:
  address: orbit@example.com
  imap: {host: mail.test, username_env: U, password_env: P}
  agent: helper
`), 0o644)
	for _, n := range []string{"helper", "finance"} {
		_ = os.WriteFile(filepath.Join(dir, "agents", n+".yaml"), []byte("name: "+n+"\ncharter: x\nwake_on_message: true\n"), 0o644)
	}
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	specs := map[string]store.AgentSpec{}
	for n, a := range cfg.Agents {
		b, _ := json.Marshal(a)
		specs[n] = store.AgentSpec{SHA: a.SHA, JSON: b, Enabled: true}
	}
	if err := s.SyncAgents(ctx, specs); err != nil {
		t.Fatal(err)
	}
	f := &fixture{s: s, cfg: cfg}
	f.p = &Poller{Store: s, Config: func() *spec.Config { return cfg }, Secret: secret,
		Env:  func(k string) (string, bool) { return map[string]string{"U": "username", "P": "password"}[k], true },
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Wake: func() { f.woke++ },
		Open: func(ctx context.Context, m *spec.Mail, user, pass string) (Mailbox, error) {
			return openIMAP(ctx, addr, nil, false, user, pass, m.IMAP.Mailbox)
		}}
	return f
}

func mailFrom(from, id, to, subject, extra, body string) []byte {
	return []byte("From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject + "\r\nMessage-ID: " + id +
		"\r\nDate: Sun, 1 Nov 2026 21:00:00 -0500\r\n" + extra + "Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		strings.ReplaceAll(body, "\n", "\r\n"))
}

func TestPollRepliesNewMailsAndIgnores(t *testing.T) {
	addr, inbox := imapServer(t)
	f := setup(t, addr)
	fin := f.cfg.Agents["finance"]
	conv, _, err := f.s.CreateConversation(ctx, store.NewConversation{Agent: "finance", SpecSHA: fin.SHA, Title: "October"})
	if err != nil {
		t.Fatal(err)
	}
	tok := Token(secret, conv.ConversationID)

	// A reply to orbit's mail, by its plus address.
	deliver(t, inbox, mailFrom("Kanishk <ME@example.com>", "<r1@mail.example.com>", "orbit+"+tok+"@example.com",
		"Re: finance: October", "", "The $500 was the deductible.\n\nOn Sun, orbit wrote:\n> You spent $84.\n"))
	// A reply that names the conversation only in In-Reply-To.
	deliver(t, inbox, mailFrom("me@example.com", "<r2@mail.example.com>", "orbit@example.com", "Re: x",
		"In-Reply-To: "+MessageID("orbit@example.com", tok)+"\r\n", "And the $20?"))
	// A new mail: a new conversation with mail.agent, titled by its subject.
	deliver(t, inbox, mailFrom("me@example.com", "<n1@mail.example.com>", "orbit@example.com", "Fwd: Plan my week", "", "Mostly meetings."))
	// Not acted on: a stranger, an auto-reply, a forged token.
	deliver(t, inbox, mailFrom("x@evil.test", "<e1@evil.test>", "orbit+"+tok+"@example.com", "hi", "", "pay me"))
	deliver(t, inbox, mailFrom("me@example.com", "<a1@mail.example.com>", "orbit@example.com", "Out of office",
		"Auto-Submitted: auto-replied\r\n", "away"))

	if err := f.p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := f.s.ConversationThread(ctx, conv.ConversationID, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, it := range page.Items {
		if it.Message != nil {
			bodies = append(bodies, it.Message.Body+" by "+deref(it.Message.Author))
		}
	}
	if strings.Join(bodies, " | ") != "The $500 was the deductible. by me@example.com | And the $20? by me@example.com" {
		t.Fatalf("conversation = %q", bodies)
	}
	convs, _ := f.s.ListConversations(ctx, "helper", false, nil, 10)
	var titled *store.Conversation
	for i, c := range convs.Conversations {
		if !c.IsDefault {
			titled = &convs.Conversations[i]
		}
	}
	if titled == nil || titled.Title != "Plan my week" {
		t.Fatalf("new mail's conversation = %+v", convs.Conversations)
	}
	for id, want := range map[string]string{"<e1@evil.test>": "ignored", "<a1@mail.example.com>": "ignored",
		"<r1@mail.example.com>": "posted", "<n1@mail.example.com>": "posted"} {
		if r, ok, _ := f.s.MailSeen(ctx, id); !ok || r.Outcome != want {
			t.Errorf("%s = %+v (%v), want %s", id, r, ok, want)
		}
	}
	if f.woke != 3 {
		t.Fatalf("woke %d runs, want 3", f.woke)
	}

	// Everything is now seen: a second poll does nothing. The same mail delivered again (a second copy, unseen) is not acted on twice.
	deliver(t, inbox, mailFrom("me@example.com", "<r2@mail.example.com>", "orbit@example.com", "Re: x",
		"In-Reply-To: "+MessageID("orbit@example.com", tok)+"\r\n", "And the $20?"))
	if err := f.p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	page, _ = f.s.ConversationThread(ctx, conv.ConversationID, nil, 50)
	n := 0
	for _, it := range page.Items {
		if it.Message != nil {
			n++
		}
	}
	if n != 2 || f.woke != 3 {
		t.Fatalf("after a duplicate: %d messages, %d wakes", n, f.woke)
	}
	crit := imap.NewSearchCriteria()
	crit.WithoutFlags = []string{imap.SeenFlag}
	if unseen, _ := inbox.SearchMessages(true, crit); len(unseen) != 0 {
		t.Fatalf("unseen after polling: %v", unseen)
	}

	// The run the reply woke knows which mail it answers, once it has read it.
	c, err := f.s.ClaimRun(ctx, "w")
	if err != nil || c == nil || c.Agent != "finance" {
		t.Fatalf("claim: %+v %v", c, err)
	}
	if _, err := f.s.StartRunContext(ctx, c.ID, c.Agent); err != nil {
		t.Fatal(err)
	}
	if id, subj, err := f.s.MailForRun(ctx, c.ID); err != nil || !strings.HasPrefix(id, "<r") || !strings.HasPrefix(subj, "Re: ") {
		t.Fatalf("mail for run = %q %q %v", id, subj, err)
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
