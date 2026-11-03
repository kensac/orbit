package telegram

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestPollAcceptsAllowlistedPrivateTextAndRecordsOtherUpdates(t *testing.T) {
	s := testdb.New(t)
	if err := s.SyncAgents(context.Background(), map[string]store.AgentSpec{"assistant": {SHA: "sha-assistant", JSON: []byte(`{"name":"assistant"}`), Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	var requestPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		if r.URL.Query().Get("offset") != "1" {
			t.Errorf("offset=%q", r.URL.Query().Get("offset"))
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"message":{"chat":{"id":7,"type":"private"},"from":{"id":9,"is_bot":false},"text":"hello"}},{"update_id":2,"message":{"chat":{"id":7,"type":"private"},"from":{"id":10,"is_bot":false},"text":"ignored"}},{"update_id":3,"message":{"chat":{"id":8,"type":"group"},"from":{"id":9,"is_bot":false},"text":"group"}}]}`)
	}))
	defer srv.Close()
	cfg := &spec.Config{Agents: map[string]*spec.Agent{"assistant": {Name: "assistant", SHA: "sha-assistant"}}, Deployment: spec.Deployment{Telegram: &spec.Telegram{Agent: "assistant", Channel: "tg", AllowUsers: []int64{9}}, Channels: map[string]spec.Channel{"tg": {Kind: "telegram", TokenEnv: "BOT"}}}}
	p := &Poller{Store: s, Config: func() *spec.Config { return cfg }, Env: func(string) (string, bool) { return "123:secret", true }, Client: srv.Client(), API: srv.URL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := p.Poll(context.Background(), cfg, "123:secret"); err != nil {
		t.Fatal(err)
	}
	if requestPath != "/bot123:secret/getUpdates" {
		t.Fatalf("request path=%q", requestPath)
	}
	var posted, ignored int
	if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE outcome='posted'),count(*) FILTER (WHERE outcome='ignored') FROM telegram_in`).Scan(&posted, &ignored); err != nil || posted != 1 || ignored != 2 {
		t.Fatalf("posted=%d ignored=%d err=%v", posted, ignored, err)
	}
	conv, err := s.GetConversation(context.Background(), mustConversation(t, s))
	if err != nil || conv.Agent != "assistant" {
		t.Fatalf("conversation=%+v err=%v", conv, err)
	}
}

func mustConversation(t *testing.T, s *store.Store) string {
	t.Helper()
	var id string
	if err := s.Pool.QueryRow(context.Background(), `SELECT conversation_id::text FROM telegram_chats WHERE chat_id=7`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
