// Package egress is `orbit egress`: the only way out of a build sandbox. It
// is an HTTP CONNECT proxy that tunnels to an allowlisted host on port 443
// and refuses everything else, logging every decision. It never allows
// GitHub: a build's checkout is already inside, and a sandbox that cannot
// reach GitHub cannot push, even with a token it found.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Port is the only destination port: every allowed service is HTTPS, and
// a CONNECT to anything else (SSH, SMTP, a database) is a way out it has no
// reason to take.
const Port = "443"

// dialTimeout bounds a connection to an allowed host.
const dialTimeout = 10 * time.Second

var hostRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// forbidden are registrable domains no allowlist may name, at any depth.
var forbidden = []string{
	"github.com", "githubusercontent.com", "github.io", "githubapp.com",
	"github.dev", "ghcr.io", "githubassets.com", "githubcopilot.com",
}

// Forbidden reports whether host is GitHub's or under one of its domains.
func Forbidden(host string) bool {
	for _, f := range forbidden {
		if host == f || strings.HasSuffix(host, "."+f) {
			return true
		}
	}
	return false
}

// ParseAllowlist reads a comma- or space-separated list of exact hostnames.
// Wildcards, IP literals, ports and GitHub are refused, as is an empty list.
func ParseAllowlist(s string) (map[string]bool, error) {
	allow := map[string]bool{}
	for _, h := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		h = strings.TrimSuffix(strings.ToLower(h), ".")
		if !hostRE.MatchString(h) {
			return nil, fmt.Errorf("allowlist entry %q is not a hostname (exact names only: no wildcard, port or IP)", h)
		}
		if Forbidden(h) {
			return nil, fmt.Errorf("allowlist entry %q is GitHub, which a sandbox must never reach", h)
		}
		allow[h] = true
	}
	if len(allow) == 0 {
		return nil, errors.New("allowlist is empty")
	}
	return allow, nil
}

// Proxy is the allowlisting CONNECT proxy.
type Proxy struct {
	allow map[string]bool
	log   *slog.Logger
	// lookup and dial are the network; tests replace them.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	dial   func(ctx context.Context, addr string) (net.Conn, error)
	// public decides which resolved addresses may be dialed.
	public func(netip.Addr) bool
}

// New returns a proxy allowing exactly the hosts in allow.
func New(allow map[string]bool, log *slog.Logger) *Proxy {
	d := &net.Dialer{Timeout: dialTimeout}
	return &Proxy{
		allow: allow,
		log:   log,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial:   func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) },
		public: Public,
	}
}

// Hosts lists the allowlist, sorted, for the startup log.
func (p *Proxy) Hosts() []string {
	out := make([]string, 0, len(p.allow))
	for h := range p.allow {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT, and the tailnet
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// Public reports whether a is a globally routable unicast address: not
// loopback, private, link-local, CGNAT (which includes every tailnet
// address), multicast or reserved. An allowed name that resolves to one of
// these, by accident or by DNS rebinding, is refused rather than becoming a
// route into the host's networks.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// ServeHTTP handles one proxy request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	client := r.RemoteAddr
	if r.Method != http.MethodConnect {
		p.log.Info("egress: denied", "client", client, "method", r.Method, "target", r.URL.String(), "reason", "only CONNECT is proxied")
		http.Error(w, "only HTTPS (CONNECT) is proxied", http.StatusMethodNotAllowed)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		p.deny(w, client, r.Host, "malformed target")
		return
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	switch {
	case port != Port:
		p.deny(w, client, r.Host, "port is not "+Port)
		return
	case Forbidden(host):
		p.deny(w, client, r.Host, "GitHub is never allowed")
		return
	case !p.allow[host]:
		p.deny(w, client, r.Host, "host is not on the allowlist")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	addrs, err := p.lookup(ctx, host)
	cancel()
	if err != nil {
		p.deny(w, client, r.Host, "resolve: "+err.Error())
		return
	}
	var target netip.Addr
	for _, a := range addrs {
		if p.public(a) {
			target = a
			break
		}
	}
	if !target.IsValid() {
		p.deny(w, client, r.Host, "resolves to no public address")
		return
	}
	// Dial the address checked, not the name, so a second resolution
	// cannot return a different one.
	ctx, cancel = context.WithTimeout(r.Context(), dialTimeout)
	up, err := p.dial(ctx, netip.AddrPortFrom(target, 443).String())
	cancel()
	if err != nil {
		p.log.Warn("egress: upstream failed", "client", client, "target", r.Host, "addr", target.String(), "err", err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = up.Close() }()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot tunnel", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = down.Close() }()
	if _, err := down.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	p.log.Info("egress: allowed", "client", client, "target", r.Host, "addr", target.String())
	var wg sync.WaitGroup
	var sent, received int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		sent, _ = io.Copy(up, buf.Reader)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	received, _ = io.Copy(down, up)
	if cw, ok := down.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	wg.Wait()
	p.log.Info("egress: closed", "client", client, "target", r.Host, "sent", sent, "received", received, "duration_ms", time.Since(start).Milliseconds())
}

func (p *Proxy) deny(w http.ResponseWriter, client, target, reason string) {
	p.log.Info("egress: denied", "client", client, "method", http.MethodConnect, "target", target, "reason", reason)
	http.Error(w, "egress denied: "+reason, http.StatusForbidden)
}

// Serve runs the proxy on addr until ctx ends.
func (p *Proxy) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { //nolint:gosec // shutdown runs after ctx has ended, so it cannot use ctx
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	p.log.Info("egress: serving", "listen", addr, "allow", p.Hosts())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
