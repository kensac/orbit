package connectors

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// MCP's authorization (the 2025-06-18 specification): the server is an OAuth protected resource that names its authorization server in its metadata (RFC 9728); orbit reads that server's metadata (RFC 8414, or OpenID discovery), registers itself as a client when no client is configured (RFC 7591), and signs a person in with the authorization code flow and PKCE (S256), naming the server as the token's resource (RFC 8707). Tokens are kept by orbit, sealed, a new version per sign-in or refresh.

// Metadata is what orbit needs from a connector's authorization server.
type Metadata struct {
	// Resource is the MCP server as its metadata names it, sent as the resource parameter.
	Resource      string   `json:"resource"`
	Issuer        string   `json:"issuer"`
	Authorization string   `json:"authorization_endpoint"`
	Token         string   `json:"token_endpoint"`
	Registration  string   `json:"registration_endpoint,omitempty"`
	Scopes        []string `json:"scopes_supported,omitempty"`
}

// Token is a connector's OAuth grant: the tokens and what refreshing them needs.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	Scope        string    `json:"scope,omitempty"`
	ClientID     string    `json:"client_id"`
	// ClientSecret is a registered client's secret (a configured one is read from its env var each time instead).
	ClientSecret  string `json:"client_secret,omitempty"`
	TokenEndpoint string `json:"token_endpoint"`
	Resource      string `json:"resource,omitempty"`
}

// getJSON fetches a metadata document: public, sent no credential, bounded.
func getJSON(ctx context.Context, hc *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// wellKnown is the RFC 8615 location of name for u: the suffix inserted between the host and u's path.
func wellKnown(u *url.URL, name string) string {
	p := strings.TrimRight(u.Path, "/")
	return u.Scheme + "://" + u.Host + "/.well-known/" + name + p
}

// Discover reads the connector's protected resource metadata, then its authorization server's.
func Discover(ctx context.Context, hc *http.Client, mcpURL string) (*Metadata, error) {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return nil, err
	}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		Scopes               []string `json:"scopes_supported"`
	}
	candidates := []string{wellKnown(u, "oauth-protected-resource"), u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"}
	if hint := resourceMetadataHint(ctx, hc, mcpURL); hint != "" {
		candidates = append([]string{hint}, candidates...)
	}
	var perr error
	for _, c := range candidates {
		if perr = getJSON(ctx, hc, c, &prm); perr == nil && len(prm.AuthorizationServers) > 0 {
			break
		}
	}
	if len(prm.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("no protected resource metadata naming an authorization server: %w", perr)
	}
	issuer, err := url.Parse(prm.AuthorizationServers[0])
	if err != nil || (issuer.Scheme != "https" && issuer.Scheme != "http") {
		return nil, fmt.Errorf("authorization server %q is not an http(s) URL", prm.AuthorizationServers[0])
	}
	md := &Metadata{}
	var merr error
	for _, c := range []string{wellKnown(issuer, "oauth-authorization-server"), wellKnown(issuer, "openid-configuration"),
		strings.TrimRight(issuer.String(), "/") + "/.well-known/openid-configuration"} {
		if merr = getJSON(ctx, hc, c, md); merr == nil && md.Authorization != "" && md.Token != "" {
			break
		}
	}
	if md.Authorization == "" || md.Token == "" {
		return nil, fmt.Errorf("authorization server %s: no metadata with both endpoints: %w", issuer, merr)
	}
	md.Resource = prm.Resource
	if md.Resource == "" {
		md.Resource = mcpURL
	}
	if len(prm.Scopes) > 0 {
		md.Scopes = prm.Scopes
	}
	return md, nil
}

// resourceMetadataHint is the resource_metadata URL a 401 names (WWW-Authenticate), or empty.
func resourceMetadataHint(ctx context.Context, hc *http.Client, mcpURL string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, strings.NewReader(`{"jsonrpc":"2.0","id":0,"method":"ping"}`))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return ""
	}
	_ = resp.Body.Close()
	h := resp.Header.Get("WWW-Authenticate")
	_, after, ok := strings.Cut(h, `resource_metadata="`)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(after, `"`)
	return v
}

// Registration is a client orbit registered or was given.
type Registration struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
}

// Register registers orbit as a client of md's authorization server, redirecting to redirect.
func Register(ctx context.Context, hc *http.Client, md *Metadata, redirect string) (*Registration, error) {
	if md.Registration == "" {
		return nil, errors.New("the authorization server offers no dynamic client registration; configure auth.client_id")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name": "orbit", "redirect_uris": []string{redirect},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.Registration, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("register: %s: %s", resp.Status, firstLine(string(b)))
	}
	var reg Registration
	if err := json.Unmarshal(b, &reg); err != nil || reg.ClientID == "" {
		return nil, fmt.Errorf("register: no client_id in the answer")
	}
	return &reg, nil
}

// PKCE returns a code verifier and its S256 challenge.
func PKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// Scopes are what a sign-in asks for: the connector's own, else what the server advertises.
func Scopes(c spec.Connector, md *Metadata) []string {
	if len(c.Auth.Scopes) > 0 {
		return c.Auth.Scopes
	}
	return md.Scopes
}

// AuthorizeURL is where a person signs in.
func AuthorizeURL(md *Metadata, clientID, redirect, state, challenge string, scopes []string) string {
	v := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"resource": {md.Resource},
	}
	if len(scopes) > 0 {
		v.Set("scope", strings.Join(scopes, " "))
	}
	sep := "?"
	if strings.Contains(md.Authorization, "?") {
		sep = "&"
	}
	return md.Authorization + sep + v.Encode()
}

// Exchange trades an authorization code for a token.
func Exchange(ctx context.Context, hc *http.Client, md *Metadata, reg Registration, code, verifier, redirect string) (*Token, error) {
	v := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"code_verifier": {verifier}, "resource": {md.Resource}}
	t, err := tokenRequest(ctx, hc, md.Token, reg, v)
	if err != nil {
		return nil, err
	}
	t.ClientID, t.TokenEndpoint, t.Resource = reg.ClientID, md.Token, md.Resource
	return t, nil
}

// Refreshed is t after a refresh grant; a server that sends no new refresh token keeps the old one valid.
func (t *Token) Refreshed(ctx context.Context, hc *http.Client, secret string) (*Token, error) {
	if t.RefreshToken == "" {
		return nil, errors.New("the token has expired and has no refresh token; a person signs the connector in again")
	}
	v := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}}
	if t.Resource != "" {
		v.Set("resource", t.Resource)
	}
	if secret == "" {
		secret = t.ClientSecret
	}
	n, err := tokenRequest(ctx, hc, t.TokenEndpoint, Registration{ClientID: t.ClientID, ClientSecret: secret}, v)
	if err != nil {
		return nil, err
	}
	n.ClientID, n.ClientSecret, n.TokenEndpoint, n.Resource = t.ClientID, t.ClientSecret, t.TokenEndpoint, t.Resource
	if n.RefreshToken == "" {
		n.RefreshToken = t.RefreshToken
	}
	if n.Scope == "" {
		n.Scope = t.Scope
	}
	return n, nil
}

func tokenRequest(ctx context.Context, hc *http.Client, endpoint string, reg Registration, v url.Values) (*Token, error) {
	v.Set("client_id", reg.ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(v.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if reg.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(reg.ClientID), url.QueryEscape(reg.ClientSecret))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var ans struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	_ = json.Unmarshal(b, &ans)
	if resp.StatusCode != http.StatusOK || ans.AccessToken == "" {
		msg := ans.Error
		if ans.Description != "" {
			msg += ": " + ans.Description
		}
		if msg == "" {
			msg = firstLine(string(b))
		}
		return nil, fmt.Errorf("token endpoint: %s: %s", resp.Status, msg)
	}
	t := &Token{AccessToken: ans.AccessToken, RefreshToken: ans.RefreshToken, TokenType: ans.TokenType, Scope: ans.Scope}
	if ans.ExpiresIn > 0 {
		t.ExpiresAt = time.Now().Add(time.Duration(ans.ExpiresIn) * time.Second)
	}
	return t, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// StoredToken is one sealed version of a connector's token.
type StoredToken struct {
	Version int
	Sealed  []byte
}

// TokenStore keeps a connector's token versions (store.Store).
type TokenStore interface {
	// LatestToken is the newest version, or nil when no person has signed the connector in.
	LatestToken(ctx context.Context, conn string) (*StoredToken, error)
	// RefreshToken writes the version refresh returns unless a version newer than seen exists, which it returns instead; one refresh at a time per connector, across instances.
	RefreshToken(ctx context.Context, conn string, seen int, refresh func(cur StoredToken) (sealed []byte, expires *time.Time, err error)) (*StoredToken, error)
}

// SealKind is the kind a connector's token is sealed as.
const SealKind = "connector-token"

// OAuthAuth sends a connector's bearer token, refreshing it when it is about to expire or the server refuses it.
type OAuthAuth struct {
	Conn   string
	Store  TokenStore
	Sealer *Sealer
	HTTP   *http.Client
	// Secret is a configured client's secret (auth.client_secret_env), empty for a registered one.
	Secret string

	mu      sync.Mutex
	version int
	token   *Token
}

// ErrNotSignedIn is a connector no person has signed in.
var ErrNotSignedIn = errors.New("not signed in: a person connects it first (POST /api/v1/connectors/{name}/authorize)")

func (o *OAuthAuth) load(ctx context.Context) (*Token, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.token == nil {
		st, err := o.Store.LatestToken(ctx, o.Conn)
		if err != nil {
			return nil, err
		}
		if st == nil {
			return nil, ErrNotSignedIn
		}
		t := &Token{}
		if err := o.Sealer.Open(SealKind, st.Sealed, t); err != nil {
			return nil, err
		}
		o.token, o.version = t, st.Version
	}
	if !o.token.ExpiresAt.IsZero() && time.Until(o.token.ExpiresAt) < time.Minute {
		if err := o.refreshLocked(ctx); err != nil {
			return nil, err
		}
	}
	return o.token, nil
}

func (o *OAuthAuth) Authorize(ctx context.Context, req *http.Request) error {
	t, err := o.load(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.AccessToken)
	return nil
}

func (o *OAuthAuth) Refresh(ctx context.Context) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.token == nil {
		return false, nil
	}
	if err := o.refreshLocked(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (o *OAuthAuth) refreshLocked(ctx context.Context) error {
	seen := o.version
	st, err := o.Store.RefreshToken(ctx, o.Conn, seen, func(cur StoredToken) ([]byte, *time.Time, error) {
		t := &Token{}
		if err := o.Sealer.Open(SealKind, cur.Sealed, t); err != nil {
			return nil, nil, err
		}
		n, err := t.Refreshed(ctx, o.HTTP, o.Secret)
		if err != nil {
			return nil, nil, err
		}
		b, err := o.Sealer.Seal(SealKind, n, 0)
		var exp *time.Time
		if !n.ExpiresAt.IsZero() {
			exp = &n.ExpiresAt
		}
		return b, exp, err
	})
	if err != nil {
		return fmt.Errorf("connector %s: refresh: %w", o.Conn, err)
	}
	t := &Token{}
	if err := o.Sealer.Open(SealKind, st.Sealed, t); err != nil {
		return err
	}
	o.token, o.version = t, st.Version
	return nil
}
