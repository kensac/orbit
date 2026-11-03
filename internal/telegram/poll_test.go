package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestHandleAllowlistIngestionReplyRoutingAndIdempotency(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `defaults: {runner: r, channels: [log]}
runners: {r: {kind: claude-code}}
channels:
  log: {kind: log}
  telegram: {kind: telegram, token_env: TELEGRAM_TOKEN, chat_id: "42"}
telegram: {channel: telegram, agent: helper, allow_users: [42]}
`
	if err := os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents", "helper.yaml"), []byte("name: helper\ncharter: x\nwake_on_message: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	agent := cfg.Agents["helper"]
	b, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"helper": {SHA: agent.SHA, JSON: b, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	woke := 0
	p := &Poller{Store: s, Config: func() *spec.Config { return cfg },
		Env: func(string) (string, bool) { return "", false }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Wake: func() { woke++ }}
	update := func(id, msgID int64, text string, replyTo *Reply, userID, chatID int64, chatType string) ReceivedUpdate {
		return ReceivedUpdate{Update: Update{ID: id, Message: &Message{MessageID: msgID, Text: text,
			From: &User{ID: userID}, Chat: Chat{ID: chatID, Type: chatType}, ReplyTo: replyTo}}, Raw: []byte(`{"update_id":1}`)}
	}
	first := update(1, 11, "start this thread", nil, 42, 42, "private")
	done, err := p.Handle(ctx, cfg, first)
	if err != nil || !done || woke != 1 {
		t.Fatalf("first message done=%v woke=%d err=%v", done, woke, err)
	}
	result, seen, err := s.TelegramSeen(ctx, 1)
	if err != nil || !seen || result.Outcome != "posted" || result.ConversationID == "" || result.RunID == "" {
		t.Fatalf("first result=%+v seen=%v err=%v", result, seen, err)
	}
	if done, err := p.Handle(ctx, cfg, first); err != nil || !done || woke != 1 {
		t.Fatalf("duplicate done=%v woke=%d err=%v", done, woke, err)
	}
	if err := s.RecordTelegramOutbound(ctx, "42", 91, result.ConversationID); err != nil {
		t.Fatal(err)
	}
	followup := update(2, 12, "continue here", &Reply{MessageID: 91}, 42, 42, "private")
	if done, err := p.Handle(ctx, cfg, followup); err != nil || !done || woke != 2 {
		t.Fatalf("followup done=%v woke=%d err=%v", done, woke, err)
	}
	page, err := s.ConversationThread(ctx, result.ConversationID, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, item := range page.Items {
		if item.Message != nil && item.Message.From == "person" {
			bodies = append(bodies, item.Message.Body)
		}
	}
	if len(bodies) != 2 || bodies[0] != "start this thread" || bodies[1] != "continue here" {
		t.Fatalf("inbound messages = %q", bodies)
	}
	for _, ignored := range []ReceivedUpdate{
		update(3, 13, "not allowed", nil, 99, 99, "private"),
		update(4, 14, "group chat", nil, 42, -100, "group"),
	} {
		if done, err := p.Handle(ctx, cfg, ignored); err != nil || !done {
			t.Fatalf("ignored update done=%v err=%v", done, err)
		}
	}
	for _, id := range []int64{3, 4} {
		got, seen, err := s.TelegramSeen(ctx, id)
		if err != nil || !seen || got.Outcome != "ignored" {
			t.Fatalf("ignored update %d = %+v seen=%v err=%v", id, got, seen, err)
		}
	}
}
