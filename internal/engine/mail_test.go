package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/mail"
	"github.com/kensac/orbit/internal/notify"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// fakeMail is an smtp channel that keeps what it is asked to send.
type fakeMail struct {
	mu   sync.Mutex
	sent []notify.Message
}

func (f *fakeMail) Send(_ context.Context, m notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeMail) all() []notify.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// mailHarness is an engine with a mailbox, an smtp channel that records (email) and a webhook (hook), and the agent helper whose notify.when is when.
func mailHarness(t *testing.T, fr *fakeRunner, when string) (*engine.Engine, *store.Store, *fakeMail, *hooks) {
	t.Helper()
	s := testdb.New(t)
	h := &hooks{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		h.mu.Lock()
		h.messages = append(h.messages, m)
		h.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(fmt.Sprintf(`
public_url: http://orbit.test
defaults: {runner: fake, channels: [hook, email]}
runners: {fake: {kind: claude-code}}
channels:
  hook: {kind: webhook, url: %s/notify}
  email: {kind: smtp, host: mail.test, port: 587, from: orbit <orbit@example.com>, to: ["Kanishk <me@example.com>"]}
mail:
  address: orbit@example.com
  imap: {host: mail.test, username_env: U, password_env: P}
  allow: [me@example.com, other@example.com]
  agent: helper
`, srv.URL)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents", "helper.yaml"),
		[]byte("name: helper\ncharter: x\nwake_on_message: true\nnotify: {when: "+when+"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	fm := &fakeMail{}
	e := &engine.Engine{
		Store: s, DataDir: t.TempDir(), ConfigDir: dir, Client: srv.Client(), Secret: []byte("secret"),
		Env:       func(string) (string, bool) { return "", false },
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) { return fr, nil },
		NewChannel: func(name string, c spec.Channel, env notify.Env, client *http.Client) (notify.Channel, error) {
			if c.Kind == "smtp" {
				return fm, nil
			}
			return notify.New(name, c, env, client)
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := e.SetConfig(cfg); err != nil {
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
	return e, s, fm, h
}

// runNext claims the queued run and executes it.
func runNext(t *testing.T, e *engine.Engine, s *store.Store) *store.ClaimedRun {
	t.Helper()
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	_ = e.Execute(ctx, c)
	return c
}

func TestAMailIsAnsweredByMail(t *testing.T) {
	// never is the built-in orbit agent's setting; always would send a report to every channel. A reply to a mail ignores both.
	for _, when := range []string{"never", "always"} {
		t.Run(when, func(t *testing.T) {
			fr := &fakeRunner{output: `{"summary": "Week planned", "report": "Mostly meetings; Thursday is free.", "questions": ["Gym on Thursday?"],
				"notify": {"send": false, "priority": "default", "title": ""}, "memory": {"update": false, "content": ""}, "actions": []}`}
			e, s, fm, h := mailHarness(t, fr, when)
			sha := e.Current().Config.Agents["helper"].SHA
			in, _, err := s.IngestMail(ctx, store.MailIn{MessageID: "<n1@mail.example.com>", From: "other@example.com", Subject: "Plan my week",
				Raw: []byte("x"), Agent: "helper", SpecSHA: sha, Title: "Plan my week", Body: "Mostly meetings.", Wake: true})
			if err != nil {
				t.Fatal(err)
			}
			c := runNext(t, e, s)
			if run, _ := s.GetRun(ctx, c.ID); run.Status != "succeeded" {
				t.Fatalf("run = %s %s", run.Status, deref(run.Error))
			}
			sent := fm.all()
			if len(sent) != 1 {
				t.Fatalf("mails sent = %d, want 1: %+v", len(sent), sent)
			}
			tok := mail.Token([]byte("secret"), in.ConversationID)
			m := sent[0]
			if !slices.Equal(m.To, []string{"other@example.com"}) || m.Subject != "Re: Plan my week" || m.InReplyTo != "<n1@mail.example.com>" ||
				m.ReplyTo != mail.ReplyAddress("orbit@example.com", tok) || m.References != mail.ThreadRef("orbit@example.com", tok)+" <n1@mail.example.com>" ||
				!strings.HasPrefix(m.MessageID, "<orbit."+tok+".") {
				t.Fatalf("reply = %+v", m)
			}
			if !strings.Contains(m.Body, "Thursday is free") || !strings.Contains(m.Body, "Gym on Thursday?") {
				t.Fatalf("reply body = %q", m.Body)
			}

			// A chat message in the same conversation is answered in the chat, not by mail.
			if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "helper", SpecSHA: sha, ConversationID: in.ConversationID,
				Body: "and Friday?", Author: "kanishk", Wake: true}); err != nil {
				t.Fatal(err)
			}
			c = runNext(t, e, s)
			if run, _ := s.GetRun(ctx, c.ID); run.Status != "succeeded" || c.Trigger != "message" {
				t.Fatalf("chat run = %s %s", run.Status, c.Trigger)
			}
			if n := len(fm.all()); n != 1 {
				t.Fatalf("mails after a chat message = %d, want still 1", n)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.messages) != 0 {
				t.Fatalf("a reply to a mail goes to the mail channel alone: %v", h.messages)
			}
		})
	}
}

func TestAFailedReplyToAMailIsMailedOnce(t *testing.T) {
	fr := &fakeRunner{err: fmt.Errorf("%w: not logged in\nmore detail", runner.ErrBackend)}
	e, s, fm, h := mailHarness(t, fr, "never")
	sha := e.Current().Config.Agents["helper"].SHA
	if _, _, err := s.IngestMail(ctx, store.MailIn{MessageID: "<n2@mail.example.com>", From: "me@example.com", Subject: "Re: October",
		Raw: []byte("x"), Agent: "helper", SpecSHA: sha, Title: "October", Body: "Why?", Wake: true}); err != nil {
		t.Fatal(err)
	}
	c := runNext(t, e, s)
	if run, _ := s.GetRun(ctx, c.ID); run.Status != "failed" {
		t.Fatalf("run = %s", run.Status)
	}
	sent := fm.all()
	if len(sent) != 1 {
		t.Fatalf("mails sent = %d, want 1: %+v", len(sent), sent)
	}
	m := sent[0]
	if !slices.Equal(m.To, []string{"me@example.com"}) || m.Subject != "Re: October" || m.InReplyTo != "<n2@mail.example.com>" ||
		!strings.Contains(m.Body, "not logged in") || strings.Contains(m.Body, "more detail") {
		t.Fatalf("failure reply = %+v", m)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.messages) != 0 {
		t.Fatalf("the failure goes to the mail channel alone: %v", h.messages)
	}
}
