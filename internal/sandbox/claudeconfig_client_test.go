package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApplyClaudeConfigSendsCASHeadersAndArchive(t *testing.T) {
	const (
		wantExpected = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		wantBundle   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	archive := []byte("deterministic tar")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/boxes/orbit/accounts/0123456789ab/claude-config" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer box-token" || r.Header.Get("Content-Type") != "application/x-tar" ||
			r.Header.Get("If-Match") != wantExpected || r.Header.Get("X-Orbit-Bundle-SHA256") != wantBundle {
			t.Fatalf("headers = %v", r.Header)
		}
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, archive) {
			t.Fatalf("archive = %q", got)
		}
		_ = json.NewEncoder(w).Encode(ClaudeConfigStatus{Digest: wantExpected, Files: []ClaudeConfigFile{}})
	}))
	defer server.Close()
	c := &Client{token: "box-token", host: strings.TrimPrefix(server.URL, "http://"), hc: server.Client()}
	got, err := c.ApplyClaudeConfig(context.Background(), "orbit", "0123456789ab", wantExpected, wantBundle, archive)
	if err != nil || got.Digest != wantExpected {
		t.Fatalf("result = %+v, %v", got, err)
	}
}

func TestApplyClaudeConfigRejectsInvalidMetadataBeforeRequest(t *testing.T) {
	c := &Client{token: "box-token", host: "invalid"}
	if _, err := c.ApplyClaudeConfig(context.Background(), "orbit", "bad-slot", strings.Repeat("a", 64), strings.Repeat("b", 64), []byte("x")); err == nil {
		t.Fatal("invalid account slot was accepted")
	}
	if _, err := c.ApplyClaudeConfig(context.Background(), "orbit", "0123456789ab", "not-sha256", strings.Repeat("b", 64), []byte("x")); err == nil {
		t.Fatal("invalid target digest was accepted")
	}
	if _, err := c.ApplyClaudeConfig(context.Background(), "orbit", "0123456789ab", strings.Repeat("a", 64), strings.Repeat("b", 64), nil); err == nil {
		t.Fatal("empty bundle was accepted")
	}
}
