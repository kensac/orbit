// Package egress is `orbit egress`: the only way out of a sandbox. It is an HTTP proxy with two profiles (Policy). A build's tunnels (CONNECT) to an allowlisted host on port 443, carrying TLS that names that host, and its allowlist may not name GitHub: a build's checkout is already inside, and a sandbox that cannot reach GitHub cannot push, even with a token it found. A workstation's is the open web: any public host on any port but SMTP's, and plain HTTP. Neither dials a private, loopback, link-local, tailnet or denied address, and every decision is logged.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
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

// AnyPublic is the allowlist "*": every hostname that resolves only to
// public addresses, GitHub included. It is a workstation sandbox's egress
// (sandboxd's ORBIT_SANDBOX_WORKSTATION_PROXY), whose box holds no GitHub
// token; a build's proxy keeps its exact list.
const AnyPublic = "*"

// ParseAllowlist reads a comma- or space-separated list of exact hostnames,
// or "*" alone (AnyPublic). Wildcards in names, IP literals, ports and GitHub
// are refused, as is an empty list.
func ParseAllowlist(s string) (map[string]bool, error) {
	if strings.TrimSpace(s) == AnyPublic {
		return map[string]bool{AnyPublic: true}, nil
	}
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

// HealthPath is egress's own liveness probe.
const HealthPath = "/healthz"

// Profiles. A build proxy tunnels to the names on its allowlist (or any public name, "*"), on port 443, and only TLS whose server name is the name asked for. A workstation proxy is a laptop's open web: any public host on any port but SMTP's, plain HTTP forwarded too. Both never dial a private, loopback, link-local or tailnet address, nor one on the deny lists.
const (
	ProfileBuild       = "build"
	ProfileWorkstation = "workstation"
)

// smtpPorts are refused in every profile: a box is not a mail relay, and mail out goes through orbit's channels.
var smtpPorts = map[string]bool{"25": true, "465": true, "587": true}

// Policy is what a proxy allows.
type Policy struct {
	Profile string
	// Allow is a build profile's allowlist (ParseAllowlist).
	Allow map[string]bool
	// DenyHosts are refused whatever else allows them: "name" exactly, ".name" for it and every name under it (ORBIT_EGRESS_DENY_HOSTS: orbit's own hostnames, the person's zone).
	DenyHosts []string
	// DenyNets are addresses never dialed, beyond the non-public ranges (ORBIT_EGRESS_DENY_NETS: the host's own public addresses, where its services listen).
	DenyNets []netip.Prefix
}

// PolicyFromEnv reads ORBIT_EGRESS_PROFILE (default build), ORBIT_EGRESS_ALLOW (a build's list; unset for a workstation), ORBIT_EGRESS_DENY_HOSTS and ORBIT_EGRESS_DENY_NETS.
func PolicyFromEnv(getenv func(string) string) (Policy, error) {
	pol := Policy{Profile: getenv("ORBIT_EGRESS_PROFILE")}
	if pol.Profile == "" {
		pol.Profile = ProfileBuild
	}
	allow := strings.TrimSpace(getenv("ORBIT_EGRESS_ALLOW"))
	switch pol.Profile {
	case ProfileBuild:
		a, err := ParseAllowlist(allow)
		if err != nil {
			return pol, err
		}
		pol.Allow = a
	case ProfileWorkstation:
		if allow != "" {
			return pol, errors.New("ORBIT_EGRESS_ALLOW is a build profile's: a workstation allows every public host")
		}
	default:
		return pol, fmt.Errorf("ORBIT_EGRESS_PROFILE %q: want %s or %s", pol.Profile, ProfileBuild, ProfileWorkstation)
	}
	var errs []error
	for _, h := range fields(getenv("ORBIT_EGRESS_DENY_HOSTS")) {
		h = strings.TrimSuffix(strings.ToLower(h), ".")
		if !hostRE.MatchString(strings.TrimPrefix(h, ".")) {
			errs = append(errs, fmt.Errorf("ORBIT_EGRESS_DENY_HOSTS entry %q: want name or .name", h))
			continue
		}
		pol.DenyHosts = append(pol.DenyHosts, h)
	}
	for _, n := range fields(getenv("ORBIT_EGRESS_DENY_NETS")) {
		pre, err := netip.ParsePrefix(n)
		if err != nil {
			a, aerr := netip.ParseAddr(n)
			if aerr != nil {
				errs = append(errs, fmt.Errorf("ORBIT_EGRESS_DENY_NETS entry %q: want an address or a prefix", n))
				continue
			}
			pre = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		pol.DenyNets = append(pol.DenyNets, pre.Masked())
	}
	return pol, errors.Join(errs...)
}

func fields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

// hostDenied reports whether a name is on the deny list.
func (pol Policy) hostDenied(host string) bool {
	for _, d := range pol.DenyHosts {
		if host == d || (strings.HasPrefix(d, ".") && (host == d[1:] || strings.HasSuffix(host, d))) {
			return true
		}
	}
	return false
}

// netDenied reports whether an address is on the deny list.
func (pol Policy) netDenied(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range pol.DenyNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Proxy is the egress proxy.
type Proxy struct {
	pol Policy
	log *slog.Logger
	// lookup and dial are the network; tests replace them.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	dial   func(ctx context.Context, addr string) (net.Conn, error)
	// public decides which resolved addresses may be dialed.
	public func(netip.Addr) bool
}

// New returns a proxy for a build profile allowing exactly the hosts in allow.
func New(allow map[string]bool, log *slog.Logger) *Proxy {
	return NewPolicy(Policy{Profile: ProfileBuild, Allow: allow}, log)
}

// NewPolicy returns a proxy enforcing pol.
func NewPolicy(pol Policy, log *slog.Logger) *Proxy {
	d := &net.Dialer{Timeout: dialTimeout}
	return &Proxy{
		pol: pol,
		log: log,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial:   func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) },
		public: Public,
	}
}

// Hosts lists the allowlist, sorted, for the startup log.
func (p *Proxy) Hosts() []string {
	out := make([]string, 0, len(p.pol.Allow))
	for h := range p.pol.Allow {
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

// target decides a destination: "" and the address to dial, or why not. The address is chosen once, at resolution, from the answers that are public and not denied, and it is what is dialed, so a later answer (DNS rebinding) never is.
func (p *Proxy) target(ctx context.Context, host, port string) (netip.Addr, string) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	n, err := strconv.Atoi(port)
	ws := p.pol.Profile == ProfileWorkstation
	lit, litErr := netip.ParseAddr(host)
	switch {
	case err != nil || n < 1 || n > 65535:
		return netip.Addr{}, "malformed port"
	case smtpPorts[port]:
		return netip.Addr{}, "SMTP ports are never proxied"
	case !ws && port != Port:
		return netip.Addr{}, "port is not " + Port
	case litErr == nil && !ws:
		return netip.Addr{}, "not a hostname"
	case litErr != nil && !hostRE.MatchString(host):
		return netip.Addr{}, "not a hostname"
	case p.pol.hostDenied(host):
		return netip.Addr{}, "host is on the deny list"
	case ws || p.pol.Allow[AnyPublic] || litErr == nil:
		// Any public host: the address checks below still refuse every private, loopback, link-local, tailnet and denied address.
	case Forbidden(host):
		return netip.Addr{}, "GitHub is never allowed"
	case !p.pol.Allow[host]:
		return netip.Addr{}, "host is not on the allowlist"
	}
	addrs := []netip.Addr{lit}
	if litErr != nil {
		lctx, cancel := context.WithTimeout(ctx, dialTimeout)
		addrs, err = p.lookup(lctx, host)
		cancel()
		if err != nil {
			return netip.Addr{}, "resolve: " + err.Error()
		}
	}
	denied := false
	for _, a := range addrs {
		if !p.public(a) {
			continue
		}
		if p.pol.netDenied(a) {
			denied = true
			continue
		}
		return a, ""
	}
	if denied {
		return netip.Addr{}, "resolves to a denied address"
	}
	return netip.Addr{}, "resolves to no public address"
}

// ServeHTTP handles one proxy request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	client := r.RemoteAddr
	// The liveness probe (a docker healthcheck): an origin-form GET for egress itself, never a proxy request, which is absolute-form and names a host.
	if r.Method == http.MethodGet && r.RequestURI == HealthPath {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	if r.Method != http.MethodConnect {
		if p.pol.Profile == ProfileWorkstation && r.URL.IsAbs() && r.URL.Scheme == "http" {
			p.forward(w, r)
			return
		}
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
	target, reason := p.target(r.Context(), host, port)
	if reason != "" {
		p.deny(w, client, r.Host, reason)
		return
	}
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
	// On 443 the client speaks first: its TLS server name is read before anything is dialed, so a tunnel to an allowed address cannot carry a request for another name on the same front (a CDN serving a denied host).
	var first []byte
	if port == Port {
		hello, sni, isTLS := peekHello(down, buf.Reader)
		first = hello
		_, litErr := netip.ParseAddr(host)
		isName := litErr != nil
		switch {
		case !isTLS && p.pol.Profile == ProfileBuild:
			p.log.Info("egress: denied", "client", client, "method", http.MethodConnect, "target", r.Host, "reason", "a build tunnel must start with TLS")
			return
		case isTLS && sni != "" && isName && !strings.EqualFold(sni, host):
			p.log.Info("egress: denied", "client", client, "method", http.MethodConnect, "target", r.Host, "sni", sni, "reason", "TLS server name is not the host asked for")
			return
		case isTLS && sni != "" && p.pol.hostDenied(strings.ToLower(sni)):
			p.log.Info("egress: denied", "client", client, "method", http.MethodConnect, "target", r.Host, "sni", sni, "reason", "TLS server name is on the deny list")
			return
		case isTLS && sni == "" && p.pol.Profile == ProfileBuild:
			p.log.Info("egress: denied", "client", client, "method", http.MethodConnect, "target", r.Host, "reason", "a build tunnel must name its server")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	up, err := p.dial(ctx, netip.AddrPortFrom(target, mustPort(port)).String())
	cancel()
	if err != nil {
		p.log.Warn("egress: upstream failed", "client", client, "target", r.Host, "addr", target.String(), "err", err)
		return
	}
	defer func() { _ = up.Close() }()
	if len(first) > 0 {
		if _, err := up.Write(first); err != nil {
			return
		}
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
	p.log.Info("egress: closed", "client", client, "target", r.Host, "sent", sent+int64(len(first)), "received", received, "duration_ms", time.Since(start).Milliseconds())
}

// mustPort is a port target has already checked.
func mustPort(port string) uint16 {
	n, _ := strconv.ParseUint(port, 10, 16)
	return uint16(n)
}

// forward proxies one plain HTTP request (a workstation's), to the address target chose.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	client := r.RemoteAddr
	port := r.URL.Port()
	if port == "" {
		port = "80"
	}
	target, reason := p.target(r.Context(), r.URL.Hostname(), port)
	if reason != "" {
		p.deny(w, client, r.URL.Host, reason)
		return
	}
	addr := netip.AddrPortFrom(target, mustPort(port)).String()
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The Host header is the URL's, so a request cannot name a host other than the one checked.
			pr.Out.Host = pr.In.URL.Host
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("Proxy-Connection")
		},
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return p.dial(ctx, addr) },
			DisableKeepAlives: true,
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			p.log.Warn("egress: upstream failed", "client", client, "target", r.URL.Host, "addr", target.String(), "err", err)
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
		},
	}
	p.log.Info("egress: allowed", "client", client, "method", r.Method, "target", r.URL.Host, "addr", target.String())
	rp.ServeHTTP(w, r)
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
	p.log.Info("egress: serving", "listen", addr, "profile", p.pol.Profile, "allow", p.Hosts(), "deny_hosts", p.pol.DenyHosts, "deny_nets", p.pol.DenyNets)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
