package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
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
	"time"
)

// clientHello is the first flight of a real TLS client naming sni ("" sends no server name).
func clientHello(t *testing.T, sni string) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer b.Close()
	go func() {
		defer a.Close()
		_ = tls.Client(a, &tls.Config{ServerName: sni, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}).Handshake() //nolint:gosec // only its first flight is used
	}()
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64<<10)
	n, err := b.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}

func TestPolicyFromEnv(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	pol, err := PolicyFromEnv(env("ORBIT_EGRESS_ALLOW", "proxy.golang.org"))
	if err != nil || pol.Profile != ProfileBuild || !pol.Allow["proxy.golang.org"] {
		t.Fatalf("%+v %v", pol, err)
	}
	pol, err = PolicyFromEnv(env("ORBIT_EGRESS_PROFILE", "workstation", "ORBIT_EGRESS_DENY_HOSTS", "orbit.kanishksachdev.com, .Kanishksachdev.com.",
		"ORBIT_EGRESS_DENY_NETS", "46.224.149.5 2a01:4f8:1c1c::/64 10.0.0.0/8"))
	if err != nil || pol.Profile != ProfileWorkstation || pol.Allow != nil ||
		strings.Join(pol.DenyHosts, ",") != "orbit.kanishksachdev.com,.kanishksachdev.com" || len(pol.DenyNets) != 3 ||
		pol.DenyNets[0] != netip.MustParsePrefix("46.224.149.5/32") {
		t.Fatalf("%+v %v", pol, err)
	}
	for name, e := range map[string]func(string) string{
		"no allowlist":              env(),
		"workstation with a list":   env("ORBIT_EGRESS_PROFILE", "workstation", "ORBIT_EGRESS_ALLOW", "*"),
		"unknown profile":           env("ORBIT_EGRESS_PROFILE", "open", "ORBIT_EGRESS_ALLOW", "*"),
		"wildcard deny host":        env("ORBIT_EGRESS_PROFILE", "workstation", "ORBIT_EGRESS_DENY_HOSTS", "*.kanishksachdev.com"),
		"deny host with a port":     env("ORBIT_EGRESS_PROFILE", "workstation", "ORBIT_EGRESS_DENY_HOSTS", "orbit.kanishksachdev.com:443"),
		"deny net that is not one":  env("ORBIT_EGRESS_PROFILE", "workstation", "ORBIT_EGRESS_DENY_NETS", "hetzner"),
		"github on a build profile": env("ORBIT_EGRESS_ALLOW", "github.com"),
	} {
		if _, err := PolicyFromEnv(e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fakeNet stands in for the network: names resolve to answers[name] (else to backend's address), and any dial to the backend's address, on any port, reaches backend. It records lookups and dials.
type fakeNet struct {
	mu      sync.Mutex
	answers map[string][]netip.Addr
	backend netip.Addr
	lookups []string
	dials   []string
}

func (f *fakeNet) install(p *Proxy, backendAddr string) {
	f.backend = netip.MustParseAddrPort(backendAddr).Addr()
	p.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lookups = append(f.lookups, host)
		if a, ok := f.answers[host]; ok {
			return a, nil
		}
		return []netip.Addr{f.backend}, nil
	}
	p.public = func(a netip.Addr) bool { return a == f.backend || Public(a) }
	p.dial = func(ctx context.Context, addr string) (net.Conn, error) {
		f.mu.Lock()
		f.dials = append(f.dials, addr)
		f.mu.Unlock()
		if netip.MustParseAddrPort(addr).Addr() != f.backend {
			return nil, &net.OpError{Op: "dial", Err: io.ErrUnexpectedEOF}
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", backendAddr)
	}
}

func (f *fakeNet) dialed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dials...)
}

func echoListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func workstationProxy(t *testing.T, backend string, pol Policy) (*httptest.Server, *fakeNet, *lockedBuf) {
	t.Helper()
	logs := &lockedBuf{}
	p := NewPolicy(pol, slog.New(slog.NewTextHandler(logs, nil)))
	f := &fakeNet{answers: map[string][]netip.Addr{}}
	f.install(p, backend)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, f, logs
}

var testDeny = Policy{
	Profile:   ProfileWorkstation,
	DenyHosts: []string{"orbit.kanishksachdev.com", ".kanishksachdev.com"},
	DenyNets:  []netip.Prefix{netip.MustParsePrefix("46.224.149.5/32")},
}

// echoes reports whether a tunnel carries p there and back.
func echoes(t *testing.T, c net.Conn, br *bufio.Reader, p []byte) bool {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(p); err != nil {
		return false
	}
	buf := make([]byte, len(p))
	_, err := io.ReadFull(br, buf)
	return err == nil && bytes.Equal(buf, p)
}

func TestWorkstationProfile(t *testing.T) {
	srv, f, logs := workstationProxy(t, echoListener(t), testDeny)
	f.answers["metadata.example"] = []netip.Addr{netip.MustParseAddr("169.254.169.254")}
	f.answers["tailnet.example"] = []netip.Addr{netip.MustParseAddr("100.79.4.41")}
	f.answers["docker.example"] = []netip.Addr{netip.MustParseAddr("172.18.0.2")}
	f.answers["lan.example"] = []netip.Addr{netip.MustParseAddr("192.168.1.10")}
	f.answers["loop.example"] = []netip.Addr{netip.MustParseAddr("127.0.0.2")}
	f.answers["v6.example"] = []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::1"), netip.MustParseAddr("::1")}
	f.answers["origin.example"] = []netip.Addr{netip.MustParseAddr("46.224.149.5")}

	// The open web: any port but SMTP's, GitHub included, a public address literal too.
	for _, target := range []string{"example.org:22", "github.com:80", "pypi.org:8080", "127.0.0.1:9418"} {
		code, c, br := connect(t, srv.URL, target)
		if code != http.StatusOK || !echoes(t, c, br, []byte("SSH-2.0-x\r\n")) {
			t.Errorf("%s: %d, or the tunnel did not carry bytes", target, code)
		}
		c.Close()
	}
	for _, target := range []string{
		"example.org:25", "example.org:465", "example.org:587",
		"orbit.kanishksachdev.com:443", "secrets.kanishksachdev.com:443", "kanishksachdev.com:22",
		"metadata.example:80", "tailnet.example:5434", "docker.example:443", "lan.example:22", "loop.example:80", "v6.example:443",
		"origin.example:5432", "46.224.149.5:22", "169.254.169.254:80", "10.0.0.1:22", "[::1]:22", "example.org:0", "example.org:70000",
	} {
		code, c, _ := connect(t, srv.URL, target)
		c.Close()
		if code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", target, code)
		}
	}
	if !strings.Contains(logs.String(), "resolves to a denied address") || !strings.Contains(logs.String(), "SMTP ports are never proxied") {
		t.Errorf("logs: %s", logs.String())
	}
	if code, c, br := connect(t, srv.URL, "notkanishksachdev.com:443"); code != http.StatusOK || !echoes(t, c, br, clientHello(t, "notkanishksachdev.com")) {
		t.Errorf("a name only ending like the denied zone: %d", code)
	} else {
		c.Close()
	}
}

// TestRebinding: the address checked at resolution is the one dialed, and the name is resolved once, so a second answer cannot move the tunnel.
func TestRebinding(t *testing.T) {
	backend := echoListener(t)
	srv, f, _ := workstationProxy(t, backend, testDeny)
	f.answers["rebind.example"] = []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("169.254.169.254"), f.backend}
	code, c, br := connect(t, srv.URL, "rebind.example:22")
	if code != http.StatusOK || !echoes(t, c, br, []byte("x")) {
		t.Fatalf("%d", code)
	}
	c.Close()
	if d := f.dialed(); len(d) != 1 || netip.MustParseAddrPort(d[0]).Addr() != f.backend {
		t.Fatalf("dialed %v", d)
	}
	if len(f.lookups) != 1 {
		t.Fatalf("resolved %d times", len(f.lookups))
	}
}

// TestServerName: on 443 the TLS server name is read before anything is dialed; a tunnel to an allowed name cannot ask its front for another one.
func TestServerName(t *testing.T) {
	for _, c := range []struct {
		name, profile, target, sni string
		plain, ok                  bool
	}{
		{"matching name", ProfileWorkstation, "example.org:443", "example.org", false, true},
		{"fronting a denied host", ProfileWorkstation, "example.org:443", "orbit.kanishksachdev.com", false, false},
		{"another name", ProfileWorkstation, "example.org:443", "other.org", false, false},
		{"no server name", ProfileWorkstation, "example.org:443", "", false, true},
		{"not TLS", ProfileWorkstation, "example.org:443", "", true, true},
		{"an address naming a host", ProfileWorkstation, "127.0.0.1:443", "pypi.org", false, true},
		{"an address naming a denied host", ProfileWorkstation, "127.0.0.1:443", "orbit.kanishksachdev.com", false, false},
		{"build: matching name", ProfileBuild, "example.org:443", "example.org", false, true},
		{"build: another name", ProfileBuild, "example.org:443", "www.google.com", false, false},
		{"build: no server name", ProfileBuild, "example.org:443", "", false, false},
		{"build: not TLS", ProfileBuild, "example.org:443", "", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			pol := testDeny
			pol.Profile = c.profile
			if c.profile == ProfileBuild {
				pol.Allow = map[string]bool{"example.org": true}
			}
			srv, f, _ := workstationProxy(t, echoListener(t), pol)
			code, conn, br := connect(t, srv.URL, c.target)
			defer conn.Close()
			if code != http.StatusOK {
				t.Fatalf("CONNECT %d", code)
			}
			first := []byte("GET / HTTP/1.1\r\nHost: orbit.kanishksachdev.com\r\n\r\n")
			if !c.plain {
				first = clientHello(t, c.sni)
			}
			if got := echoes(t, conn, br, first); got != c.ok {
				t.Fatalf("tunnel carried bytes: %v, want %v", got, c.ok)
			}
			if !c.ok && len(f.dialed()) != 0 {
				t.Fatalf("a refused tunnel dialed %v", f.dialed())
			}
		})
	}
}

// TestPlainHTTP: a workstation forwards plain HTTP with the Host the URL names; a build refuses it.
func TestPlainHTTP(t *testing.T) {
	var mu sync.Mutex
	var hosts []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.Host+" "+r.URL.Path+" "+r.Header.Get("Proxy-Authorization"))
		mu.Unlock()
		_, _ = io.WriteString(w, "plain")
	}))
	defer up.Close()
	srv, _, _ := workstationProxy(t, up.Listener.Addr().String(), testDeny)
	proxyURL, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	get := func(u string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("http://example.org/x", map[string]string{"Proxy-Authorization": "Basic eA=="}); code != 200 || body != "plain" {
		t.Fatalf("%d %q", code, body)
	}
	mu.Lock()
	if len(hosts) != 1 || hosts[0] != "example.org /x " {
		t.Fatalf("upstream saw %v", hosts)
	}
	mu.Unlock()
	for _, u := range []string{"http://orbit.kanishksachdev.com/", "http://example.org:25/", "http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/"} {
		if code, _ := get(u, nil); code != http.StatusForbidden {
			t.Errorf("%s: %d", u, code)
		}
	}
	build, _ := testProxy(t, "127.0.0.1")
	bu, _ := url.Parse(build.URL)
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(bu)}}).Get("http://example.org/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("build profile, plain HTTP: %d", resp.StatusCode)
	}
}
