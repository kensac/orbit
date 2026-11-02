package credproxy

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testKey = strings.Repeat("k", 40)

var loopback = netip.MustParseAddr("127.0.0.1")

func sealer(t *testing.T) *Sealer {
	t.Helper()
	s, err := NewSealer(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func grant(kind string) Grant {
	return Grant{Kind: kind, Credential: "real-" + kind, Addr: loopback, Expires: time.Now().Add(time.Hour), Sandbox: strings.Repeat("ab", 32), RunID: "run-1"}
}

func TestSealOpen(t *testing.T) {
	s := sealer(t)
	g := grant(KindOAuth)
	tok, err := s.Seal(g)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, prefix) || strings.Contains(tok, g.Credential) {
		t.Fatalf("token %q", tok)
	}
	got, err := s.Open(tok)
	if err != nil || got.Credential != g.Credential || got.Addr != g.Addr || !got.Expires.Equal(g.Expires) || got.Kind != KindOAuth {
		t.Fatalf("open: %+v, %v", got, err)
	}
	if again, _ := s.Seal(g); again == tok {
		t.Error("two seals of one grant are equal: the nonce is not random")
	}
	other, _ := NewSealer(strings.Repeat("z", 40))
	if _, err := other.Open(tok); err == nil {
		t.Error("a token opened under another key")
	}
	raw := []byte(tok)
	raw[len(raw)-3] ^= 1
	for _, bad := range []string{string(raw), "", prefix, "sk-ant-oat01-x", prefix + "!!!", strings.TrimPrefix(tok, prefix)} {
		if _, err := s.Open(bad); err == nil {
			t.Errorf("%q opened", bad)
		}
	}
	for _, g := range []Grant{
		{Kind: "AWS_BEARER_TOKEN_BEDROCK", Credential: "x", Addr: loopback, Expires: time.Now()},
		{Kind: KindOAuth, Addr: loopback, Expires: time.Now()},
		{Kind: KindOAuth, Credential: "x", Expires: time.Now()},
		{Kind: KindOAuth, Credential: "x", Addr: loopback},
	} {
		if _, err := s.Seal(g); err == nil {
			t.Errorf("sealed %+v", g)
		}
	}
	if _, err := NewSealer("short"); err == nil {
		t.Error("a short key was accepted")
	}
}

// upstreamSeen is what the fake backend received.
type upstreamSeen struct {
	method, uri, auth, apiKey, cookie, beta, xff string
}

// lockedLog is the proxy's log: it is written after the answer is sent, so a test reads it under a lock, after closing the server.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func harness(t *testing.T, backend http.HandlerFunc) (*Proxy, *httptest.Server, *lockedLog) {
	t.Helper()
	up := httptest.NewServer(backend)
	t.Cleanup(up.Close)
	logs := &lockedLog{}
	p := New(sealer(t), slog.New(slog.NewTextHandler(logs, nil)))
	p.upstream, _ = url.Parse(up.URL)
	// The real transport refuses loopback (TestUpstreamMustBePublic); the fake backend is on it.
	p.rp.Transport = http.DefaultTransport
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, srv, logs
}

func post(t *testing.T, srv *httptest.Server, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(`{"model":"m"}`))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestForwardsWithTheRealCredential(t *testing.T) {
	var seen upstreamSeen
	p, srv, logs := harness(t, func(w http.ResponseWriter, r *http.Request) {
		seen = upstreamSeen{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("Cookie"), r.Header.Get("Anthropic-Beta"), r.Header.Get("X-Forwarded-For")}
		w.Header().Set("Set-Cookie", "a=b")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		_, _ = io.WriteString(w, "ok")
	})
	oauth, _ := p.sealer.Seal(grant(KindOAuth))
	resp := post(t, srv, "/v1/messages?beta=true", map[string]string{"Authorization": "Bearer " + oauth, "Cookie": "c=d", "Anthropic-Beta": "oauth-2025-04-20"})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if seen != (upstreamSeen{"POST", "/v1/messages?beta=true", "Bearer real-" + KindOAuth, "", "", "oauth-2025-04-20", ""}) {
		t.Fatalf("upstream saw %+v", seen)
	}
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" {
		t.Errorf("response headers %v", resp.Header)
	}

	key, _ := p.sealer.Seal(grant(KindAPIKey))
	resp = post(t, srv, "/v1/messages/count_tokens", map[string]string{"X-Api-Key": key})
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || seen.apiKey != "real-"+KindAPIKey || seen.auth != "" || seen.uri != "/v1/messages/count_tokens" {
		t.Fatalf("%d, upstream saw %+v", resp.StatusCode, seen)
	}
	srv.Close()
	if strings.Contains(logs.String(), "real-") || strings.Contains(logs.String(), oauth) {
		t.Errorf("a credential or token reached the log: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "credproxy: forwarded") || !strings.Contains(logs.String(), "run_id=run-1") {
		t.Errorf("logs: %s", logs.String())
	}
}

func TestRefuses(t *testing.T) {
	var calls atomic.Int32
	p, srv, logs := harness(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	tok := func(mod func(*Grant)) string {
		g := grant(KindOAuth)
		mod(&g)
		s, err := p.sealer.Seal(g)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	good := tok(func(*Grant) {})
	key := tok(func(g *Grant) { g.Kind = KindAPIKey })
	for name, c := range map[string]struct {
		method, path string
		hdr          map[string]string
		want         int
	}{
		"GET messages":       {"GET", "/v1/messages", map[string]string{"Authorization": "Bearer " + good}, 403},
		"models":             {"POST", "/v1/models", map[string]string{"Authorization": "Bearer " + good}, 403},
		"oauth endpoint":     {"POST", "/api/oauth/claude_cli/create_api_key", map[string]string{"Authorization": "Bearer " + good}, 403},
		"batches":            {"POST", "/v1/messages/batches", map[string]string{"Authorization": "Bearer " + good}, 403},
		"dot segment":        {"POST", "/v1/messages/../models", map[string]string{"Authorization": "Bearer " + good}, 403},
		"other query":        {"POST", "/v1/messages?beta=true&x=1", map[string]string{"Authorization": "Bearer " + good}, 403},
		"no token":           {"POST", "/v1/messages", nil, 401},
		"the real kind":      {"POST", "/v1/messages", map[string]string{"Authorization": "Bearer sk-ant-oat01-guess"}, 401},
		"both headers":       {"POST", "/v1/messages", map[string]string{"Authorization": "Bearer " + good, "X-Api-Key": key}, 401},
		"key as bearer":      {"POST", "/v1/messages", map[string]string{"Authorization": "Bearer " + key}, 401},
		"oauth as x-api-key": {"POST", "/v1/messages", map[string]string{"X-Api-Key": good}, 401},
		"expired":            {"POST", "/v1/messages", map[string]string{"Authorization": "Bearer " + tok(func(g *Grant) { g.Expires = time.Now().Add(-time.Second) })}, 401},
		"another address":    {"POST", "/v1/messages", map[string]string{"Authorization": "Bearer " + tok(func(g *Grant) { g.Addr = netip.MustParseAddr("172.18.0.5") })}, 401},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader("{}"))
		if name == "dot segment" {
			req.URL.Opaque = "/v1/messages/../models"
		}
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", name, resp.StatusCode, c.want)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d refused requests reached the backend", n)
	}
	srv.Close()
	if !strings.Contains(logs.String(), "token used from another address") {
		t.Errorf("logs: %s", logs.String())
	}
}

func TestBodyLimit(t *testing.T) {
	p, srv, _ := harness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) })
	tok, _ := p.sealer.Seal(grant(KindOAuth))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(strings.Repeat("x", MaxBody+1)))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d", resp.StatusCode)
	}
}

// TestStreams holds the backend's second event until the client has read the first: a proxy that buffered the answer would deadlock here.
func TestStreams(t *testing.T) {
	release := make(chan struct{})
	p, srv, _ := harness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	})
	tok, _ := p.sealer.Seal(grant(KindOAuth))
	resp := post(t, srv, "/v1/messages?beta=true", map[string]string{"Authorization": "Bearer " + tok})
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "event: message_start\n" {
		t.Fatalf("first event: %q, %v", line, err)
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("rest %q", rest)
	}
}

// TestUpstreamMustBePublic uses the real transport: a backend name that resolves to loopback (as a rebinding answer would) is never dialed.
func TestUpstreamMustBePublic(t *testing.T) {
	var reached atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer up.Close()
	p := New(sealer(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	u, _ := url.Parse(up.URL)
	u.Host = "localhost:" + u.Port()
	p.upstream = u
	srv := httptest.NewServer(p)
	defer srv.Close()
	tok, _ := p.sealer.Seal(grant(KindOAuth))
	resp := post(t, srv, "/v1/messages", map[string]string{"Authorization": "Bearer " + tok})
	if resp.StatusCode != http.StatusBadGateway || reached.Load() {
		t.Fatalf("%d, reached %v", resp.StatusCode, reached.Load())
	}
}
