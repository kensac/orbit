package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/client"
)

func TestCommands(t *testing.T) {
	for _, a := range [][]string{{"agents"}, {"auth-smoke"}, {"conversations", "assistant"}, {"thread", "conversation"}, {"stream", "run"}, {"approve", "action"}, {"reject", "action"}, {"send", "conversation", "hello"}, {"send-agent", "assistant", "hello"}} {
		method, path, body, err := command(a)
		if err != nil || method == "" || path == "" {
			t.Fatalf("%v: %s %s %v", a, method, path, err)
		}
		if a[0] == "send" || a[0] == "send-agent" {
			var b map[string]any
			if json.Unmarshal([]byte(body), &b) != nil || b["body"] != "hello" || b["client_message_id"] == "" {
				t.Fatalf("message %s", body)
			}
		}
	}
}

func TestAuthSmokeConfirmsAPIWithoutPrintingTokenOrResponse(t *testing.T) {
	var authorization string
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/agents" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		authorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `[{"name":"private-agent"}]`)
	}))
	defer api.Close()

	hc := client.HTTPClient()
	hc.Transport = api.Client().Transport
	var stdout, stderr strings.Builder
	login := func(_ context.Context, gotHTTP *http.Client, issuer, clientID string, notice io.Writer) (string, error) {
		if gotHTTP != hc || issuer != "https://auth.example/application/o/orbit/" || clientID != "public-client" {
			t.Errorf("login config = %v %q %q", gotHTTP == hc, issuer, clientID)
		}
		_, _ = fmt.Fprintln(notice, "Open https://auth.example/device and enter code ABC-123")
		return "private-access-token", nil
	}
	args := []string{
		"-url", api.URL,
		"-issuer", "https://auth.example/application/o/orbit/",
		"-client-id", "public-client",
		"auth-smoke",
	}
	if err := runWith(context.Background(), args, hc, login, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer private-access-token" {
		t.Fatalf("authorization = %q", authorization)
	}
	if stdout.String() != "Authentik sign-in accepted by Orbit.\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "ABC-123") || strings.Contains(stderr.String(), "private-access-token") || strings.Contains(stdout.String()+stderr.String(), "private-agent") {
		t.Fatalf("smoke output exposed data: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestInvalidCommands(t *testing.T) {
	for _, a := range [][]string{{"unknown", "x"}, {"send", "x"}, {"approve", "../actions"}, {"agents", "extra"}, {"stream", "x", "extra"}} {
		if _, _, _, err := command(a); err == nil {
			t.Fatalf("accepted %v", a)
		}
	}
}
