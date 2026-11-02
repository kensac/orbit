package egress

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestParseAllowlist(t *testing.T) {
	got, err := ParseAllowlist("api.anthropic.com, proxy.golang.org,sum.golang.org REGISTRY.npmjs.org.")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"api.anthropic.com", "proxy.golang.org", "sum.golang.org", "registry.npmjs.org"} {
		if !got[h] {
			t.Errorf("%s missing", h)
		}
	}
	for _, bad := range []string{
		"", " , ", "github.com", "api.github.com", "codeload.github.com", "raw.githubusercontent.com",
		"objects.githubusercontent.com", "ghcr.io", "kensac.github.io", "*.golang.org", "1.2.3.4",
		"proxy.golang.org:443", "localhost", "http://proxy.golang.org",
	} {
		if _, err := ParseAllowlist(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPublic(t *testing.T) {
	for s, want := range map[string]bool{
		"34.36.0.1":         true,
		"2606:4700::1111":   true,
		"127.0.0.1":         false,
		"10.0.0.1":          false,
		"172.18.0.2":        false,
		"192.168.1.1":       false,
		"100.79.4.41":       false, // a tailnet address (hetzner)
		"169.254.169.254":   false, // cloud metadata
		"0.0.0.0":           false,
		"::1":               false,
		"fd7a:115c:a1e0::1": false,
		"fe80::1":           false,
		"::ffff:10.0.0.1":   false,
		"224.0.0.1":         false,
	} {
		if got := Public(netip.MustParseAddr(s)); got != want {
			t.Errorf("Public(%s) = %v", s, got)
		}
	}
}

// testProxy allows example.org, which resolves to an echo server on
// loopback; the public check is relaxed only for that server's address.
func testProxy(t *testing.T, resolveTo string) (*httptest.Server, *lockedBuf) {
	return testProxyAllowing(t, resolveTo, map[string]bool{"example.org": true})
}

func testProxyAllowing(t *testing.T, resolveTo string, allow map[string]bool) (*httptest.Server, *lockedBuf) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	logs := &lockedBuf{}
	p := New(allow, slog.New(slog.NewTextHandler(logs, nil)))
	echoAddr := echo.Addr().(*net.TCPAddr).AddrPort()
	p.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(resolveTo)}, nil
	}
	p.public = func(a netip.Addr) bool { return a == echoAddr.Addr() || Public(a) }
	p.dial = func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		if ap := netip.MustParseAddrPort(addr); ap.Addr() == echoAddr.Addr() && ap.Port() == 443 {
			addr = echoAddr.String()
		}
		return d.DialContext(ctx, "tcp", addr)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, logs
}

func connect(t *testing.T, proxy, target string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(proxy, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, c, br
}

func TestProxyTunnelsAllowedHost(t *testing.T) {
	srv, logs := testProxy(t, "127.0.0.1")
	code, c, br := connect(t, srv.URL, "example.org:443")
	defer c.Close()
	if code != http.StatusOK {
		t.Fatalf("CONNECT: %d", code)
	}
	if _, err := io.WriteString(c, "hello"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("tunnel: %q %v", buf, err)
	}
	if !strings.Contains(logs.String(), "egress: allowed") {
		t.Fatalf("decision not logged: %s", logs.String())
	}
}

func TestProxyDenies(t *testing.T) {
	srv, logs := testProxy(t, "127.0.0.1")
	for _, target := range []string{
		"github.com:443", "api.github.com:443", "evil.example:443",
		"example.org:22", "example.org:80", "127.0.0.1:443", "100.79.4.41:8085",
	} {
		code, c, _ := connect(t, srv.URL, target)
		c.Close()
		if code != http.StatusForbidden {
			t.Errorf("%s: %d", target, code)
		}
	}
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("plain GET: %d", resp.StatusCode)
	}
	if n := strings.Count(logs.String(), "egress: denied"); n != 8 {
		t.Fatalf("%d denials logged:\n%s", n, logs.String())
	}
}

// TestAnyPublicHost: "*" tunnels to any public hostname, GitHub included,
// and still never dials into the host's networks.
func TestAnyPublicHost(t *testing.T) {
	allow, err := ParseAllowlist(" * ")
	if err != nil || !allow[AnyPublic] {
		t.Fatalf("%v %v", allow, err)
	}
	srv, _ := testProxyAllowing(t, "127.0.0.1", allow)
	for _, target := range []string{"github.com:443", "pypi.org:443"} {
		code, c, _ := connect(t, srv.URL, target)
		c.Close()
		if code != http.StatusOK {
			t.Errorf("%s: %d", target, code)
		}
	}
	for _, target := range []string{"pypi.org:22", "127.0.0.1:443"} {
		code, c, _ := connect(t, srv.URL, target)
		c.Close()
		if code != http.StatusForbidden {
			t.Errorf("%s: %d", target, code)
		}
	}
	private, _ := testProxyAllowing(t, "100.79.4.41", allow)
	if code, c, _ := connect(t, private.URL, "pypi.org:443"); code != http.StatusForbidden {
		t.Errorf("a tailnet address: %d", code)
	} else {
		c.Close()
	}
}

// TestProxyRefusesPrivateResolution: an allowed name that resolves into the
// host's networks (a tailnet address here) is not dialed.
func TestProxyRefusesPrivateResolution(t *testing.T) {
	srv, logs := testProxy(t, "100.79.4.41")
	code, c, _ := connect(t, srv.URL, "example.org:443")
	c.Close()
	if code != http.StatusForbidden || !strings.Contains(logs.String(), "no public address") {
		t.Fatalf("%d %s", code, logs.String())
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The health probe answers only an origin-form GET for egress itself; a proxied GET naming the same path on another host is still refused.
func TestHealthz(t *testing.T) {
	srv, _ := testProxy(t, "127.0.0.1")
	resp, err := http.Get(srv.URL + HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "ok\n" {
		t.Fatalf("healthz = %d %q", resp.StatusCode, b)
	}
	proxy, _ := url.Parse(srv.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
	resp, err = c.Get("http://example.org" + HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("proxied GET for /healthz = %d", resp.StatusCode)
	}
}
