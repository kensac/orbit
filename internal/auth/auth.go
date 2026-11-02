// Package auth verifies the authentik access tokens /api/v1 accepts: a JWT
// signed by one of the configured issuers (authentik providers), for one of
// the configured clients, held by a member of the configured group.
//
// It is built on go-jose rather than the standard library alone, because
// Go has no JOSE implementation, and hand-rolled JWS parsing is where
// algorithm-confusion bugs live. go-jose refuses any algorithm not in the
// allowlist at parse time, checks the key type against the algorithm, and
// validates exp, nbf and iat with a leeway. The JWKS cache and its refresh
// policy are here, so what orbit does when authentik rotates a key, or is
// down, is a rule in this file and not a library default.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Leeway is the clock skew tolerated on exp, nbf and iat. authentik and
// orbit run on the same host today; 30 s still absorbs a skewed client
// forwarding a token the moment it expires without accepting stale ones.
const Leeway = 30 * time.Second

// MinRefresh is the shortest interval between two fetches of one issuer's
// JWKS. A token naming an unknown kid triggers a refresh (authentik rotated
// its key), and without this bound a stream of forged kids would make orbit
// hammer authentik.
const MinRefresh = 30 * time.Second

// MaxKeyAge is how long fetched keys are trusted without refetching, so a
// key authentik removed stops verifying within a day. A failed refetch
// keeps the old keys: tokens authentik already signed stay valid while it
// is down.
const MaxKeyAge = 24 * time.Hour

// fetchTimeout bounds one discovery or JWKS request. A request waiting on
// a key fetch waits at most this long.
const fetchTimeout = 10 * time.Second

// algorithms are the signatures accepted. Every asymmetric JWS algorithm
// authentik can sign with; HS* is absent on purpose, since a symmetric key
// is the client secret and verifying with it would let any client mint
// tokens.
var algorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512, jose.EdDSA,
}

// Config is what a Verifier trusts.
type Config struct {
	// Issuers are the accepted `iss` values, each an authentik provider's
	// issuer (https://auth.example.com/application/o/<slug>/). Each has its
	// own signing key, found through its discovery document.
	Issuers []string
	// ClientIDs are the accepted `aud` values.
	ClientIDs []string
	// Group must be in the token's `groups` claim.
	Group string
	// Client fetches discovery and JWKS documents. Nil uses a client with
	// fetchTimeout.
	Client *http.Client
	// Now is time.Now, replaceable in tests.
	Now func() time.Time
}

// Identity is who a verified token belongs to.
type Identity struct {
	// Person is preferred_username: the author of their messages and the
	// decider of their decisions.
	Person  string
	Subject string
	Issuer  string
	// Groups is the token's groups claim, for checks narrower than the
	// one Verify makes (the workspace group).
	Groups []string
}

// Error is a rejected token. Check names the check that failed (token,
// iss, kid, signature, exp, nbf, iat, aud, groups, preferred_username), so a
// 401 says which one.
type Error struct {
	Check  string
	Reason string
}

func (e *Error) Error() string { return e.Check + ": " + e.Reason }

// ErrUnavailable wraps a failure to fetch an issuer's keys when none are
// cached: the token may be fine, orbit cannot tell yet.
var ErrUnavailable = errors.New("signing keys unavailable")

// Verifier checks tokens. It is safe for concurrent use.
type Verifier struct {
	cfg  Config
	sets map[string]*keySet
}

// New returns a Verifier. It fetches nothing until the first token, so
// orbit starts while authentik is down.
func New(cfg Config) (*Verifier, error) {
	if len(cfg.Issuers) == 0 || len(cfg.ClientIDs) == 0 || cfg.Group == "" {
		return nil, errors.New("auth: issuers, client ids and group are all required")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: fetchTimeout}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	v := &Verifier{cfg: cfg, sets: map[string]*keySet{}}
	for _, iss := range cfg.Issuers {
		v.sets[iss] = &keySet{issuer: iss}
	}
	return v, nil
}

type claims struct {
	jwt.Claims
	Groups            []string `json:"groups"`
	PreferredUsername string   `json:"preferred_username"`
}

// Verify checks raw and returns its identity. A rejected token is an
// *Error; a key fetch failure with nothing cached wraps ErrUnavailable.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Identity, error) {
	tok, err := jwt.ParseSigned(raw, algorithms)
	if err != nil {
		return nil, &Error{"token", "not a signed JWT with an accepted algorithm: " + err.Error()}
	}
	if len(tok.Headers) != 1 {
		return nil, &Error{"token", "want exactly one signature"}
	}
	kid := tok.Headers[0].KeyID

	// The issuer picks the key set, so it is read before the signature is
	// checked; it is trusted only once the signature verifies.
	var unverified jwt.Claims
	if err := tok.UnsafeClaimsWithoutVerification(&unverified); err != nil {
		return nil, &Error{"token", "claims: " + err.Error()}
	}
	set, ok := v.sets[unverified.Issuer]
	if !ok {
		return nil, &Error{"iss", fmt.Sprintf("issuer %q is not accepted", unverified.Issuer)}
	}
	keys, err := set.keys(ctx, v.cfg.Client, v.cfg.Now(), kid)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, &Error{"kid", fmt.Sprintf("no key %q in the issuer's JWKS", kid)}
	}
	var c claims
	verified := false
	for _, k := range keys {
		if tok.Claims(k.Key, &c) == nil {
			verified = true
			break
		}
	}
	if !verified {
		return nil, &Error{"signature", "does not verify against the issuer's key"}
	}
	if c.Expiry == nil {
		return nil, &Error{"exp", "token has no exp"}
	}
	now := v.cfg.Now()
	err = c.ValidateWithLeeway(jwt.Expected{Issuer: set.issuer, AnyAudience: v.cfg.ClientIDs, Time: now}, Leeway)
	switch {
	case errors.Is(err, jwt.ErrExpired):
		return nil, &Error{"exp", "expired at " + c.Expiry.Time().UTC().Format(time.RFC3339)}
	case errors.Is(err, jwt.ErrNotValidYet):
		return nil, &Error{"nbf", "not valid until " + c.NotBefore.Time().UTC().Format(time.RFC3339)}
	case errors.Is(err, jwt.ErrIssuedInTheFuture):
		return nil, &Error{"iat", "issued in the future"}
	case errors.Is(err, jwt.ErrInvalidAudience):
		return nil, &Error{"aud", fmt.Sprintf("audience %v is not an accepted client", []string(c.Audience))}
	case err != nil:
		return nil, &Error{"token", err.Error()}
	}
	if !slices.Contains(c.Groups, v.cfg.Group) {
		return nil, &Error{"groups", fmt.Sprintf("not a member of %q", v.cfg.Group)}
	}
	if strings.TrimSpace(c.PreferredUsername) == "" {
		return nil, &Error{"preferred_username", "token has no preferred_username"}
	}
	return &Identity{Person: c.PreferredUsername, Subject: c.Subject, Issuer: set.issuer, Groups: c.Groups}, nil
}

// keySet is one issuer's cached JWKS.
type keySet struct {
	issuer string

	mu        sync.Mutex
	jwksURI   string
	set       jose.JSONWebKeySet
	fetchedAt time.Time
	triedAt   time.Time
}

// keys returns the keys matching kid (every key when the token names none).
// It refetches when nothing is cached, when the cache is older than
// MaxKeyAge, or when kid is unknown, never more often than MinRefresh. The
// lock is held across the fetch so concurrent requests share one fetch.
func (s *keySet) keys(ctx context.Context, client *http.Client, now time.Time, kid string) ([]jose.JSONWebKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	match := s.lookup(kid)
	stale := s.fetchedAt.IsZero() || now.Sub(s.fetchedAt) > MaxKeyAge || len(match) == 0
	if stale && (s.triedAt.IsZero() || now.Sub(s.triedAt) >= MinRefresh) {
		s.triedAt = now
		set, err := s.fetch(ctx, client)
		switch {
		case err == nil:
			s.set, s.fetchedAt = set, now
			match = s.lookup(kid)
		case s.fetchedAt.IsZero():
			return nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, s.issuer, err)
		}
	} else if s.fetchedAt.IsZero() {
		return nil, fmt.Errorf("%w: %s: last fetch failed, retrying after %s", ErrUnavailable, s.issuer, MinRefresh)
	}
	return match, nil
}

func (s *keySet) lookup(kid string) []jose.JSONWebKey {
	var out []jose.JSONWebKey
	for _, k := range s.set.Keys {
		if (kid == "" || k.KeyID == kid) && k.Use != "enc" && k.IsPublic() {
			out = append(out, k)
		}
	}
	return out
}

// fetch reads the issuer's discovery document (once) and then its JWKS.
func (s *keySet) fetch(ctx context.Context, client *http.Client) (jose.JSONWebKeySet, error) {
	var set jose.JSONWebKeySet
	if s.jwksURI == "" {
		var doc struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := getJSON(ctx, client, strings.TrimSuffix(s.issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
			return set, fmt.Errorf("discovery: %w", err)
		}
		// The discovery document must describe the issuer it was fetched
		// for, or its keys would verify someone else's tokens.
		if doc.Issuer != s.issuer || doc.JWKSURI == "" {
			return set, fmt.Errorf("discovery: issuer %q, jwks_uri %q; want issuer %q and a jwks_uri", doc.Issuer, doc.JWKSURI, s.issuer)
		}
		s.jwksURI = doc.JWKSURI
	}
	if err := getJSON(ctx, client, s.jwksURI, &set); err != nil {
		return set, fmt.Errorf("jwks: %w", err)
	}
	return set, nil
}

func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	// A JWKS is a few KB; 1 MB bounds a misbehaving endpoint.
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// SplitList parses a comma-separated environment value, dropping blanks.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
