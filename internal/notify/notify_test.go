package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/spec"
)

func TestNtfy(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	env := func(k string) (string, bool) { return map[string]string{"TOPIC": "secret-topic", "TOK": "tk"}[k], true }
	ch, err := New("n", spec.Channel{Kind: "ntfy", URL: srv.URL, TopicEnv: "TOPIC", TokenEnv: "TOK"}, env, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("é", 3000) // multi-byte, over the body limit
	err = ch.Send(context.Background(), Message{Title: "Café spend", Body: long, Priority: High,
		Link: "http://x/run", Buttons: []Button{{Label: "Review, now", URL: "http://x/a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/secret-topic" || got.Header.Get("Authorization") != "Bearer tk" || got.Header.Get("Priority") != "high" {
		t.Fatalf("request = %s %v", got.URL.Path, got.Header)
	}
	if !strings.HasPrefix(got.Header.Get("Title"), "=?utf-8?") {
		t.Errorf("a non-ASCII title must be RFC 2047 encoded, got %q", got.Header.Get("Title"))
	}
	if got.Header.Get("Actions") != "view, Review  now, http://x/a" {
		t.Errorf("actions = %q", got.Header.Get("Actions"))
	}
	if !utf8.ValidString(body) || len(body) > ntfyMaxBody+64 {
		t.Errorf("body must be cut on a rune boundary under the limit (len %d, valid %v)", len(body), utf8.ValidString(body))
	}
}

func TestMissingCredentialFailsAtBuild(t *testing.T) {
	_, err := New("n", spec.Channel{Kind: "ntfy", URL: "http://x", TopicEnv: "MISSING"}, func(string) (string, bool) { return "", false }, http.DefaultClient)
	if err == nil {
		t.Fatal("a channel with an unset credential must not build")
	}
}

func TestWebhookFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	ch, _ := New("w", spec.Channel{Kind: "webhook", URL: srv.URL}, func(string) (string, bool) { return "", false }, srv.Client())
	if err := ch.Send(context.Background(), Message{Title: "t"}); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
}

func TestTelegramUsesPrivateReplyTargetAndBoundsText(t *testing.T) {
	var gotPath string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()
	ch := &telegramChannel{token: "bot-secret", chatID: 42, client: srv.Client(), api: srv.URL}
	if err := ch.Send(context.Background(), Message{Title: "reply", Body: strings.Repeat("x", 5000), TelegramChatID: 77}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/botbot-secret/sendMessage" || payload["chat_id"] != float64(77) {
		t.Fatalf("path=%q payload=%v", gotPath, payload)
	}
	text, ok := payload["text"].(string)
	if !ok || len(text) > 4000 || !strings.Contains(text, "continued in orbit") {
		t.Fatalf("text len=%d", len(text))
	}
}
