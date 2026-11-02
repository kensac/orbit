// Package credproxy is `orbit credproxy`: the way a sandboxed Claude Code reaches its backend without holding the credential. sandboxd gives the sandbox's process a token instead of the credential (Seal: the credential encrypted under a key the sandbox never sees, bound to the sandbox's address and expiry) and points the CLI here with ANTHROPIC_BASE_URL. The proxy opens the token, checks the request came from that address before the expiry and names a Messages API path, and forwards it to api.anthropic.com with the real credential, streaming the answer back as it arrives.
//
// Seen against Claude Code 2.1.316 (docs/tickets.md, Credential proxy: what the CLI does): with a placeholder credential and ANTHROPIC_BASE_URL it sends only POST /v1/messages?beta=true there, as `Authorization: Bearer` for a subscription token and `x-api-key` for an API key.
package credproxy

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/egress"
)

// Upstream is the one backend the proxy forwards to.
const Upstream = "https://api.anthropic.com"

// The two credential kinds, named by the variable the CLI reads.
const (
	KindOAuth  = claudeproto.OAuthToken
	KindAPIKey = claudeproto.APIKey
)

// MinKeyLen matches ORBIT_API_TOKEN's floor.
const MinKeyLen = 32

// MaxBody is the Messages API's own request limit (32 MB): a larger body is refused before it is read.
const MaxBody = 32 << 20

// prefix marks a token, so a log line or a leaked value says what it is.
const prefix = "orbit-cp1."

// paths are the only requests forwarded: the Messages API and its token count, both POST.
var paths = map[string]bool{"/v1/messages": true, "/v1/messages/count_tokens": true}

// Grant is what a token carries: one credential, for requests from one address until one time.
type Grant struct {
	Kind       string     `json:"k"`
	Credential string     `json:"c"`
	Addr       netip.Addr `json:"a"`
	Expires    time.Time  `json:"e"`
	// Sandbox and RunID name the sandbox in the proxy's logs.
	Sandbox string `json:"s"`
	RunID   string `json:"r,omitempty"`
}

// Sealer seals and opens tokens with a key sandboxd and the proxy share.
type Sealer struct{ aead cipher.AEAD }

// NewSealer derives the AES-256-GCM key from key (ORBIT_CREDPROXY_KEY).
func NewSealer(key string) (*Sealer, error) {
	if len(key) < MinKeyLen {
		return nil, fmt.Errorf("ORBIT_CREDPROXY_KEY must be at least %d characters", MinKeyLen)
	}
	k := sha256.Sum256([]byte("orbit credproxy v1\x00" + key))
	b, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: a}, nil
}

var aad = []byte("orbit credproxy token v1")

// Seal returns the token for g.
func (s *Sealer) Seal(g Grant) (string, error) {
	if g.Kind != KindOAuth && g.Kind != KindAPIKey {
		return "", fmt.Errorf("credential kind %q", g.Kind)
	}
	if g.Credential == "" || !g.Addr.IsValid() || g.Expires.IsZero() {
		return "", errors.New("a grant needs a credential, an address and an expiry")
	}
	plain, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(s.aead.Seal(nonce, nonce, plain, aad)), nil
}

// errToken is every token that does not open: the caller learns nothing about why.
var errToken = errors.New("not a credential proxy token")

// Open returns the grant a token carries, or an error if it was not sealed with this key.
func (s *Sealer) Open(tok string) (Grant, error) {
	body, ok := strings.CutPrefix(tok, prefix)
	if !ok {
		return Grant{}, errToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return Grant{}, errToken
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], aad)
	if err != nil {
		return Grant{}, errToken
	}
	var g Grant
	if err := json.Unmarshal(plain, &g); err != nil {
		return Grant{}, errToken
	}
	return g, nil
}

// Proxy forwards allowed requests with the credential their token carries.
type Proxy struct {
	sealer *Sealer
	log    *slog.Logger
	rp     *httputil.ReverseProxy
	now    func() time.Time
	// upstream is Upstream; tests replace it.
	upstream *url.URL
}

// dialTimeout bounds a connection to the backend.
const dialTimeout = 10 * time.Second

// New returns a proxy that opens tokens with sealer.
func New(sealer *Sealer, log *slog.Logger) *Proxy {
	d := &net.Dialer{Timeout: dialTimeout}
	tr := &http.Transport{
		// No proxy: this process's own route out is direct. Every address the backend's name resolves to is checked as egress checks one, and the checked address is dialed, so a resolver answer pointing inside the host's networks is refused.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if egress.Public(ip) {
					return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				}
			}
			return nil, fmt.Errorf("%s resolves to no public address", host)
		},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   dialTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   8,
		ExpectContinueTimeout: time.Second,
	}
	u, _ := url.Parse(Upstream)
	p := &Proxy{sealer: sealer, log: log, now: time.Now, upstream: u}
	p.rp = &httputil.ReverseProxy{
		Rewrite: p.rewrite,
		// Every write is flushed: the CLI reads the Messages API's event stream as it arrives.
		FlushInterval: -1,
		Transport:     tr,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.log.Warn("credproxy: upstream failed", "path", r.URL.Path, "err", err)
			http.Error(w, "credproxy: upstream unreachable", http.StatusBadGateway)
		},
	}
	return p
}

type grantKey struct{}

// rewrite builds the upstream request: the backend's URL, the client's headers without any credential, cookie or proxy header, and the grant's credential in the header its kind uses. Rewrite also drops hop-by-hop and X-Forwarded headers.
func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	g, _ := pr.In.Context().Value(grantKey{}).(Grant)
	pr.SetURL(p.upstream)
	pr.Out.Host = p.upstream.Host
	for _, h := range []string{"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization", "Forwarded"} {
		pr.Out.Header.Del(h)
	}
	switch g.Kind {
	case KindOAuth:
		pr.Out.Header.Set("Authorization", "Bearer "+g.Credential)
	case KindAPIKey:
		pr.Out.Header.Set("X-Api-Key", g.Credential)
	}
}

// presented returns the token a request carries and the kind its header implies: a subscription token as a bearer, an API key as x-api-key. Both, or neither, is no token.
func presented(r *http.Request) (string, string) {
	bearer, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	key := r.Header.Get("X-Api-Key")
	switch {
	case hasBearer && key == "":
		return bearer, KindOAuth
	case !hasBearer && r.Header.Get("Authorization") == "" && key != "":
		return key, KindAPIKey
	}
	return "", ""
}

// ServeHTTP handles one request from a sandbox.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := p.now()
	client, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		p.deny(w, r, http.StatusForbidden, "client address unknown", Grant{})
		return
	}
	if r.Method != http.MethodPost || !paths[r.URL.Path] || (r.URL.RawQuery != "" && r.URL.RawQuery != "beta=true") {
		p.deny(w, r, http.StatusForbidden, "only the Messages API is forwarded", Grant{})
		return
	}
	tok, kind := presented(r)
	if tok == "" {
		p.deny(w, r, http.StatusUnauthorized, "no token", Grant{})
		return
	}
	g, err := p.sealer.Open(tok)
	switch {
	case err != nil:
		p.deny(w, r, http.StatusUnauthorized, "token does not open", Grant{})
		return
	case g.Kind != kind:
		p.deny(w, r, http.StatusUnauthorized, "token sent in the other credential's header", g)
		return
	case !p.now().Before(g.Expires):
		p.deny(w, r, http.StatusUnauthorized, "token expired", g)
		return
	case client.Addr().Unmap() != g.Addr.Unmap():
		p.deny(w, r, http.StatusUnauthorized, "token used from another address ("+client.Addr().String()+")", g)
		return
	}
	if r.ContentLength > MaxBody {
		p.deny(w, r, http.StatusRequestEntityTooLarge, "body over the Messages API's limit", g)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	cw := &countingWriter{ResponseWriter: w}
	p.rp.ServeHTTP(cw, r.WithContext(context.WithValue(r.Context(), grantKey{}, g)))
	p.log.Info("credproxy: forwarded", "sandbox", short(g.Sandbox), "run_id", g.RunID, "path", r.URL.Path,
		"status", cw.status, "received", cw.n.Load(), "duration_ms", p.now().Sub(start).Milliseconds())
}

func (p *Proxy) deny(w http.ResponseWriter, r *http.Request, status int, reason string, g Grant) {
	p.log.Info("credproxy: denied", "client", r.RemoteAddr, "method", r.Method, "path", r.URL.Path,
		"sandbox", short(g.Sandbox), "run_id", g.RunID, "reason", reason)
	http.Error(w, "credproxy: "+reason, status)
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// countingWriter records the status and bytes of a forwarded answer for the log, and keeps the flusher the reverse proxy streams with.
type countingWriter struct {
	http.ResponseWriter
	status int
	n      atomic.Int64
}

func (c *countingWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(b)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// Serve runs the proxy on addr until ctx ends.
func (p *Proxy) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { //nolint:gosec // shutdown runs after ctx has ended, so it cannot use ctx
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	p.log.Info("credproxy: serving", "listen", addr, "upstream", Upstream)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
