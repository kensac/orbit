package notify

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/spec"
)

type telegramRoundTrip func(*http.Request) (*http.Response, error)

func (f telegramRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTelegramChannelUsesConfiguredRecipientAndReturnsReceipt(t *testing.T) {
	const token = "123456:abcdefghijklmnopqrstuvwxyzABCDEFGH"
	var request string
	client, err := New("telegram", spec.Channel{Kind: "telegram", TokenEnv: "TG", ChatID: "42"}, func(k string) (string, bool) {
		return token, k == "TG"
	}, &http.Client{Transport: telegramRoundTrip(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		request = r.URL.String() + "\n" + string(b)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":17,"chat":{"id":42,"type":"private"}}}`))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	rc, ok := client.(ReceiptChannel)
	if !ok {
		t.Fatal("Telegram channel does not return provider receipts")
	}
	receipt, err := rc.SendWithReceipt(context.Background(), Message{Title: "hello", Body: "world", TelegramReplyTo: 9})
	if err != nil || receipt.ChatID != "42" || receipt.MessageID != 17 || receipt.Channel != "telegram" {
		t.Fatalf("receipt = %+v, %v", receipt, err)
	}
	if !strings.Contains(request, "/bot"+token+"/sendMessage") || !strings.Contains(request, `"chat_id":"42"`) || !strings.Contains(request, `"message_id":9`) {
		t.Fatalf("Telegram request = %q", request)
	}
}
