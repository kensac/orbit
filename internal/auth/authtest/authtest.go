// Package authtest is a stand-in authentik provider for tests: an issuer
// with a discovery document and a JWKS, and keys generated per test to sign
// tokens with.
package authtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Issuer serves /.well-known/openid-configuration and /jwks/ under its URL.
type Issuer struct {
	// URL is the issuer (`iss`), with authentik's trailing slash.
	URL string
	// JWKSFetches counts JWKS requests.
	JWKSFetches atomic.Int32

	t      *testing.T
	mu     sync.Mutex
	keys   []jose.JSONWebKey // private; the JWKS serves their public halves
	signer int               // index into keys used by Mint
	n      int
}

// NewIssuer starts an issuer with one RSA key.
func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	is := &Issuer{t: t}
	is.Rotate()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	is.URL = srv.URL + "/application/o/orbit/"
	mux.HandleFunc("GET /application/o/orbit/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is.URL, "jwks_uri": is.URL + "jwks/"})
	})
	mux.HandleFunc("GET /application/o/orbit/jwks/", func(w http.ResponseWriter, r *http.Request) {
		is.JWKSFetches.Add(1)
		is.mu.Lock()
		defer is.mu.Unlock()
		var set jose.JSONWebKeySet
		for _, k := range is.keys {
			set.Keys = append(set.Keys, k.Public())
		}
		_ = json.NewEncoder(w).Encode(set)
	})
	return is
}

// Rotate adds a new RSA key, served in the JWKS from now on, and signs with
// it. The old keys stay in the JWKS.
func (is *Issuer) Rotate() {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		is.t.Fatal(err)
	}
	is.mu.Lock()
	defer is.mu.Unlock()
	is.n++
	is.keys = append(is.keys, jose.JSONWebKey{Key: k, KeyID: "key-" + strconv.Itoa(is.n), Algorithm: string(jose.RS256), Use: "sig"})
	is.signer = len(is.keys) - 1
}

// Claims are a token's contents.
type Claims struct {
	jwt.Claims
	Groups            []string `json:"groups,omitempty"`
	PreferredUsername string   `json:"preferred_username,omitempty"`
}

// Valid returns claims orbit accepts: this issuer, aud, group, a person,
// an hour of validity.
func (is *Issuer) Valid(aud, group, person string) Claims {
	now := time.Now()
	return Claims{
		Claims: jwt.Claims{Issuer: is.URL, Subject: "sub-" + person, Audience: jwt.Audience{aud},
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour))},
		Groups: []string{group}, PreferredUsername: person,
	}
}

// Mint signs c with the current key.
func (is *Issuer) Mint(c Claims) string {
	is.mu.Lock()
	k := is.keys[is.signer]
	is.mu.Unlock()
	return sign(is.t, jose.SigningKey{Algorithm: jose.RS256, Key: k}, c)
}

// MintForeign signs c with a key the issuer does not publish, under kid.
func MintForeign(t *testing.T, kid string, c Claims) string {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return sign(t, jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: k, KeyID: kid}}, c)
}

// MintHS signs c with a shared secret, which orbit must refuse.
func MintHS(t *testing.T, secret []byte, c Claims) string {
	return sign(t, jose.SigningKey{Algorithm: jose.HS256, Key: secret}, c)
}

func sign(t *testing.T, key jose.SigningKey, c Claims) string {
	s, err := jose.NewSigner(key, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(s).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
