package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kensac/orbit/internal/auth"
)

// OAuthProxy lets an MCP client sign in with authentik. authentik has no
// dynamic client registration, which the Claude apps need, so orbit is the
// authorization server the client sees: it registers clients itself, sends
// the person to authentik under orbit's own confidential client, and hands
// the client authentik's access token, which /mcp verifies like any other.
// Nothing is stored: client ids, states, codes and refresh tokens are
// sealed (AES-GCM) under a key derived from the operator token, so any
// instance can finish a flow another one started, and rotating the token
// ends every registration and refresh token.
type OAuthProxy struct {
	// Issuer is the authentik provider's issuer (orbit-mcp), with its
	// trailing slash, as in ORBIT_OIDC_ISSUER.
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectHosts are the hosts a registered client may redirect to
	// (exact, or a subdomain of one). A loopback address is always allowed,
	// for local clients.
	RedirectHosts []string
	Client        *http.Client

	key      []byte
	mu       sync.Mutex
	upstream *upstreamMeta
	used     map[string]time.Time // codes already exchanged, until they expire
}

type upstreamMeta struct {
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
}

// Lifetimes of what the proxy seals. A state covers one sign-in at
// authentik; a code is exchanged by the client within seconds of the
// redirect (RFC 6749 recommends at most 10 minutes).
const (
	stateTTL = 15 * time.Minute
	codeTTL  = 2 * time.Minute
)

// upstreamScope is what orbit asks authentik for: profile carries
// preferred_username and groups, which the verifier requires, and
// offline_access a refresh token so a connector stays signed in.
const upstreamScope = "openid profile email offline_access"

// DefaultRedirectHosts are the Claude apps' callback hosts.
var DefaultRedirectHosts = []string{"claude.ai", "claude.com"}

// SetKey derives the sealing key from secret (the operator token).
func (o *OAuthProxy) SetKey(secret []byte) {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("orbit mcp oauth"))
	o.key = m.Sum(nil)
}

func (s *Server) publicURL() string {
	return strings.TrimRight(s.Engine.Current().Config.Deployment.PublicURL, "/")
}

func (s *Server) resourceMetadataURL() string {
	if s.OAuth == nil {
		return ""
	}
	return s.publicURL() + "/.well-known/oauth-protected-resource/mcp"
}

func (s *Server) oauthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.authServerMeta)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/mcp", s.authServerMeta)
	mux.HandleFunc("POST /oauth/register", s.oauthRegister)
	mux.HandleFunc("GET /oauth/authorize", s.oauthAuthorize)
	mux.HandleFunc("GET /oauth/callback", s.oauthCallback)
	mux.HandleFunc("POST /oauth/token", s.oauthToken)
}

func metaHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "max-age=300")
}

// protectedResource is RFC 9728 metadata: /mcp is protected by orbit's
// own authorization server.
func (s *Server) protectedResource(w http.ResponseWriter, _ *http.Request) {
	metaHeaders(w)
	base := s.publicURL()
	respond(w, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "orbit",
	}, nil)
}

// authServerMeta is RFC 8414 metadata for orbit's authorization server.
func (s *Server) authServerMeta(w http.ResponseWriter, _ *http.Request) {
	metaHeaders(w)
	base := s.publicURL()
	respond(w, map[string]any{
		"issuer":                                         base,
		"authorization_endpoint":                         base + "/oauth/authorize",
		"token_endpoint":                                 base + "/oauth/token",
		"registration_endpoint":                          base + "/oauth/register",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"authorization_response_iss_parameter_supported": true,
	}, nil)
}

// oauthClient is what a client id seals: the redirect URIs it registered.
type oauthClient struct {
	Kind      string   `json:"k"`
	Redirects []string `json:"r"`
	Name      string   `json:"n,omitempty"`
}

// oauthRegister is RFC 7591 registration. Every client is public (PKCE,
// no secret); its id seals its redirect URIs, so nothing is stored.
func (s *Server) oauthRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}
	if len(body.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range body.RedirectURIs {
		if err := s.OAuth.redirectAllowed(u); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	id, err := s.OAuth.seal(oauthClient{Kind: "client", Redirects: body.RedirectURIs, Name: body.ClientName}, 0)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respondStatus(w, http.StatusCreated, map[string]any{
		"client_id": id, "client_id_issued_at": time.Now().Unix(), "client_name": body.ClientName,
		"redirect_uris": body.RedirectURIs, "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
	}, nil)
}

func (o *OAuthProxy) redirectAllowed(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return fmt.Errorf("redirect uri %q is not an absolute URL", raw)
	}
	host := u.Hostname()
	if (u.Scheme == "http" || u.Scheme == "https") && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("redirect uri %q must be https", raw)
	}
	hosts := o.RedirectHosts
	if len(hosts) == 0 {
		hosts = DefaultRedirectHosts
	}
	for _, h := range hosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return nil
		}
	}
	return fmt.Errorf("redirect uri host %q is not one orbit allows (ORBIT_MCP_REDIRECT_HOSTS)", host)
}

// oauthState travels through authentik as its state parameter.
type oauthState struct {
	Kind      string `json:"k"`
	ClientID  string `json:"c"`
	Redirect  string `json:"r"`
	State     string `json:"s"`
	Challenge string `json:"p"`
}

func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var c oauthClient
	if err := s.OAuth.open(q.Get("client_id"), "client", &c); err != nil {
		http.Error(w, "unknown client_id: register again", http.StatusBadRequest)
		return
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(c.Redirects) == 1 {
		redirect = c.Redirects[0]
	}
	if !slices.Contains(c.Redirects, redirect) {
		// Never redirect to a URI the client did not register.
		http.Error(w, "redirect_uri is not one this client registered", http.StatusBadRequest)
		return
	}
	fail := func(code, desc string) {
		u, _ := url.Parse(redirect)
		v := u.Query()
		v.Set("error", code)
		v.Set("error_description", desc)
		if st := q.Get("state"); st != "" {
			v.Set("state", st)
		}
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only code is supported")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with S256 is required")
		return
	}
	up, err := s.OAuth.meta(r.Context())
	if err != nil {
		fail("temporarily_unavailable", err.Error())
		return
	}
	state, err := s.OAuth.seal(oauthState{Kind: "state", ClientID: q.Get("client_id"), Redirect: redirect,
		State: q.Get("state"), Challenge: q.Get("code_challenge")}, stateTTL)
	if err != nil {
		fail("server_error", err.Error())
		return
	}
	v := url.Values{
		"response_type": {"code"}, "client_id": {s.OAuth.ClientID}, "redirect_uri": {s.publicURL() + "/oauth/callback"},
		"scope": {upstreamScope}, "state": {state},
	}
	http.Redirect(w, r, up.Authorization+"?"+v.Encode(), http.StatusFound)
}

// oauthCode is the code orbit gives the client: authentik's tokens, bound
// to the client, its redirect and its PKCE challenge.
type oauthCode struct {
	Kind      string      `json:"k"`
	ClientID  string      `json:"c"`
	Redirect  string      `json:"r"`
	Challenge string      `json:"p"`
	Tokens    tokenAnswer `json:"t"`
}

// oauthRefresh is the refresh token orbit gives the client: authentik's,
// usable only through orbit (it needs orbit's client secret anyway).
type oauthRefresh struct {
	Kind     string `json:"k"`
	ClientID string `json:"c"`
	Upstream string `json:"u"`
}

type tokenAnswer struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var st oauthState
	if err := s.OAuth.open(q.Get("state"), "state", &st); err != nil {
		http.Error(w, "this sign-in expired or is not orbit's; start again from the app", http.StatusBadRequest)
		return
	}
	back := func(v url.Values) {
		u, _ := url.Parse(st.Redirect)
		qv := u.Query()
		for k, vs := range v {
			qv[k] = vs
		}
		if st.State != "" {
			qv.Set("state", st.State)
		}
		qv.Set("iss", s.publicURL())
		u.RawQuery = qv.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	if e := q.Get("error"); e != "" {
		back(url.Values{"error": {e}, "error_description": {q.Get("error_description")}})
		return
	}
	tok, err := s.OAuth.exchange(r.Context(), url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")},
		"redirect_uri": {s.publicURL() + "/oauth/callback"}})
	if err != nil {
		back(url.Values{"error": {"access_denied"}, "error_description": {err.Error()}})
		return
	}
	// Check the person now, so a sign-in orbit would refuse fails here with
	// the reason rather than as a connector that never works.
	if s.Verifier != nil {
		if _, err := s.Verifier.Verify(r.Context(), tok.AccessToken); err != nil {
			var ae *auth.Error
			if errors.As(err, &ae) {
				back(url.Values{"error": {"access_denied"}, "error_description": {ae.Error()}})
				return
			}
			back(url.Values{"error": {"temporarily_unavailable"}, "error_description": {err.Error()}})
			return
		}
	}
	code, err := s.OAuth.seal(oauthCode{Kind: "code", ClientID: st.ClientID, Redirect: st.Redirect,
		Challenge: st.Challenge, Tokens: *tok}, codeTTL)
	if err != nil {
		back(url.Values{"error": {"server_error"}, "error_description": {err.Error()}})
		return
	}
	back(url.Values{"code": {code}})
}

func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	f := r.PostForm
	clientID := f.Get("client_id")
	var c oauthClient
	if err := s.OAuth.open(clientID, "client", &c); err != nil {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id: register again")
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		raw := f.Get("code")
		var code oauthCode
		if err := s.OAuth.open(raw, "code", &code); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is expired or not orbit's")
			return
		}
		if code.ClientID != clientID || code.Redirect != f.Get("redirect_uri") {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client or redirect_uri")
			return
		}
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != code.Challenge {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code_challenge")
			return
		}
		if !s.OAuth.spend(raw) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was already used")
			return
		}
		s.tokenResponse(w, clientID, code.Tokens, "")
	case "refresh_token":
		var rt oauthRefresh
		if err := s.OAuth.open(f.Get("refresh_token"), "refresh", &rt); err != nil || rt.ClientID != clientID {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is not this client's")
			return
		}
		tok, err := s.OAuth.exchange(r.Context(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt.Upstream}})
		if err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
			return
		}
		s.tokenResponse(w, clientID, *tok, rt.Upstream)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code or refresh_token")
	}
}

// tokenResponse answers with authentik's access token and orbit's sealed
// form of its refresh token (the previous one when authentik sent none).
func (s *Server) tokenResponse(w http.ResponseWriter, clientID string, t tokenAnswer, prevRefresh string) {
	upstream := t.RefreshToken
	if upstream == "" {
		upstream = prevRefresh
	}
	out := map[string]any{"access_token": t.AccessToken, "token_type": "Bearer"}
	if t.ExpiresIn > 0 {
		out["expires_in"] = t.ExpiresIn
	}
	if upstream != "" {
		rt, err := s.OAuth.seal(oauthRefresh{Kind: "refresh", ClientID: clientID, Upstream: upstream}, 0)
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		out["refresh_token"] = rt
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, out, nil)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

// meta is authentik's endpoints, from the provider's discovery document,
// fetched once.
func (o *OAuthProxy) meta(ctx context.Context) (*upstreamMeta, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.upstream != nil {
		return o.upstream, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.Issuer+".well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("authentik discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authentik discovery: %s", resp.Status)
	}
	var m upstreamMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("authentik discovery: %w", err)
	}
	if m.Authorization == "" || m.Token == "" {
		return nil, errors.New("authentik discovery: no authorization or token endpoint")
	}
	o.upstream = &m
	return &m, nil
}

// exchange calls authentik's token endpoint as orbit's client.
func (o *OAuthProxy) exchange(ctx context.Context, v url.Values) (*tokenAnswer, error) {
	up, err := o.meta(ctx)
	if err != nil {
		return nil, err
	}
	v.Set("client_id", o.ClientID)
	v.Set("client_secret", o.ClientSecret)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.Token, strings.NewReader(v.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("authentik token endpoint: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}
		_ = json.Unmarshal(b, &e)
		return nil, fmt.Errorf("authentik token endpoint: %s: %s %s", resp.Status, e.Error, e.Desc)
	}
	var t tokenAnswer
	if err := json.Unmarshal(b, &t); err != nil || t.AccessToken == "" {
		return nil, errors.New("authentik token endpoint: no access_token in the answer")
	}
	return &t, nil
}

func (o *OAuthProxy) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return http.DefaultClient
}

// spend marks a code used; false when it already was. Per instance: a code
// replayed to the other instance of a rollout within codeTTL is the gap.
func (o *OAuthProxy) spend(code string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	if o.used == nil {
		o.used = map[string]time.Time{}
	}
	for k, exp := range o.used {
		if now.After(exp) {
			delete(o.used, k)
		}
	}
	sum := sha256.Sum256([]byte(code))
	k := string(sum[:])
	if _, ok := o.used[k]; ok {
		return false
	}
	o.used[k] = now.Add(codeTTL)
	return true
}

// sealed wraps a payload with its expiry (0: none).
type sealed struct {
	Exp  int64           `json:"e,omitempty"`
	Body json.RawMessage `json:"b"`
}

func (o *OAuthProxy) seal(v any, ttl time.Duration) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	env := sealed{Body: b}
	if ttl > 0 {
		env.Exp = time.Now().Add(ttl).Unix()
	}
	plain, _ := json.Marshal(env)
	gcm, err := o.gcm()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, nil)), nil
}

// open unseals s into v, refusing an expired one or one of another kind
// (a state is never accepted as a code).
func (o *OAuthProxy) open(s, kind string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	gcm, err := o.gcm()
	if err != nil {
		return err
	}
	if len(raw) < gcm.NonceSize() {
		return errors.New("too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return err
	}
	var env sealed
	if err := json.Unmarshal(plain, &env); err != nil {
		return err
	}
	if env.Exp != 0 && time.Now().Unix() > env.Exp {
		return errors.New("expired")
	}
	var k struct {
		Kind string `json:"k"`
	}
	if err := json.Unmarshal(env.Body, &k); err != nil || k.Kind != kind {
		return errors.New("wrong kind")
	}
	return json.Unmarshal(env.Body, v)
}

func (o *OAuthProxy) gcm() (cipher.AEAD, error) {
	if len(o.key) == 0 {
		return nil, errors.New("oauth proxy has no key")
	}
	b, err := aes.NewCipher(o.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}
