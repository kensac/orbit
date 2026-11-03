package telegram

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestClientParsesUpdatesWithoutLeakingToken(t *testing.T) {
	const token = "123456:abcdefghijklmnopqrstuvwxyzABCDEFGH"
	client, err := NewClient(token, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.telegram.org" || !strings.Contains(r.URL.Path, "/bot"+token+"/getUpdates") {
			t.Fatalf("unexpected request: %s", r.URL)
		}
		return response(http.StatusOK, `{"ok":true,"result":[{"update_id":9,"message":{"message_id":3,"text":"hello","from":{"id":42,"is_bot":false},"chat":{"id":42,"type":"private"}}}]}`), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	updates, err := client.GetUpdates(context.Background(), 8)
	if err != nil || len(updates) != 1 {
		t.Fatalf("GetUpdates = %v, %v", updates, err)
	}
	if updates[0].Update.ID != 9 || updates[0].Update.Message.From.ID != 42 || string(updates[0].Raw) == "" {
		t.Fatalf("update = %+v", updates[0])
	}
}

func TestClientDoesNotFollowTokenBearingRedirect(t *testing.T) {
	const token = "123456:abcdefghijklmnopqrstuvwxyzABCDEFGH"
	calls := 0
	client, err := NewClient(token, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://attacker.invalid/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetUpdates(context.Background(), 0)
	if err == nil || strings.Contains(err.Error(), token) || calls != 1 {
		t.Fatalf("redirect result err=%v calls=%d", err, calls)
	}
}

func TestClientRedactsTransportError(t *testing.T) {
	const token = "123456:abcdefghijklmnopqrstuvwxyzABCDEFGH"
	client, err := NewClient(token, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetUpdates(context.Background(), 0)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("transport error was not safely reported: %v", err)
	}
}

func TestTruncateRunes(t *testing.T) {
	got := truncateRunes(strings.Repeat("界", maxTextRunes+5), maxTextRunes)
	if n := len([]rune(got)); n != maxTextRunes || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated text has %d runes and suffix %q", n, got[len(got)-3:])
	}
}
