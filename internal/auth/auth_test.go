package auth_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/kensac/orbit/internal/auth"
	"github.com/kensac/orbit/internal/auth/authtest"
)

const (
	client = "orbit-client"
	group  = "infra-admins"
)

// clock is a settable Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func verifier(t *testing.T, is *authtest.Issuer, clk *clock) *auth.Verifier {
	t.Helper()
	v, err := auth.New(auth.Config{Issuers: []string{is.URL}, ClientIDs: []string{"other", client}, Group: group, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func check(t *testing.T, err error, want string) {
	t.Helper()
	var ae *auth.Error
	if !errors.As(err, &ae) || ae.Check != want {
		t.Fatalf("err = %v, want a %q rejection", err, want)
	}
}

func TestVerify(t *testing.T) {
	is := authtest.NewIssuer(t)
	clk := &clock{t: time.Now()}
	v := verifier(t, is, clk)
	ctx := context.Background()

	id, err := v.Verify(ctx, is.Mint(is.Valid(client, group, "kanishk")))
	if err != nil || id.Person != "kanishk" || id.Issuer != is.URL {
		t.Fatalf("valid token: %+v %v", id, err)
	}

	cases := map[string]func(c *authtest.Claims){
		"exp":                func(c *authtest.Claims) { c.Expiry = jwt.NewNumericDate(time.Now().Add(-time.Minute)) },
		"nbf":                func(c *authtest.Claims) { c.NotBefore = jwt.NewNumericDate(time.Now().Add(time.Hour)) },
		"aud":                func(c *authtest.Claims) { c.Audience = jwt.Audience{"someone-else"} },
		"iss":                func(c *authtest.Claims) { c.Issuer = "https://auth.example.com/application/o/other/" },
		"groups":             func(c *authtest.Claims) { c.Groups = []string{"users"} },
		"preferred_username": func(c *authtest.Claims) { c.PreferredUsername = "" },
	}
	for want, mutate := range cases {
		c := is.Valid(client, group, "kanishk")
		mutate(&c)
		_, err := v.Verify(ctx, is.Mint(c))
		check(t, err, want)
	}

	// Within the leeway an expired token still passes; past it, it fails.
	c := is.Valid(client, group, "kanishk")
	c.Expiry = jwt.NewNumericDate(clk.Now().Add(-auth.Leeway / 2))
	if _, err := v.Verify(ctx, is.Mint(c)); err != nil {
		t.Fatalf("expired inside the leeway: %v", err)
	}

	noExp := is.Valid(client, group, "kanishk")
	noExp.Expiry = nil
	_, err = v.Verify(ctx, is.Mint(noExp))
	check(t, err, "exp")
}

func TestBadSignature(t *testing.T) {
	is := authtest.NewIssuer(t)
	v := verifier(t, is, &clock{t: time.Now()})
	ctx := context.Background()

	// A valid token's payload swapped for one naming another person.
	good := strings.Split(is.Mint(is.Valid(client, group, "kanishk")), ".")
	forged := is.Valid(client, group, "mallory")
	other := strings.Split(authtest.MintForeign(t, "key-1", forged), ".")
	tampered := good[0] + "." + other[1] + "." + good[2]
	_, err := v.Verify(ctx, tampered)
	check(t, err, "signature")

	// A key the issuer never published, under its kid.
	_, err = v.Verify(ctx, authtest.MintForeign(t, "key-1", forged))
	check(t, err, "signature")

	// A shared-secret token is refused before any key is looked at.
	_, err = v.Verify(ctx, authtest.MintHS(t, []byte(strings.Repeat("s", 32)), forged))
	check(t, err, "token")

	// alg none, hand built.
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + other[1] + "."
	_, err = v.Verify(ctx, none)
	check(t, err, "token")

	_, err = v.Verify(ctx, "not-a-jwt")
	check(t, err, "token")
}

func TestUnknownKidRefreshesTheJWKS(t *testing.T) {
	is := authtest.NewIssuer(t)
	clk := &clock{t: time.Now()}
	v := verifier(t, is, clk)
	ctx := context.Background()

	if _, err := v.Verify(ctx, is.Mint(is.Valid(client, group, "kanishk"))); err != nil {
		t.Fatal(err)
	}
	if n := is.JWKSFetches.Load(); n != 1 {
		t.Fatalf("fetches after first token = %d", n)
	}
	// Cached keys serve the next token without a fetch.
	if _, err := v.Verify(ctx, is.Mint(is.Valid(client, group, "kanishk"))); err != nil || is.JWKSFetches.Load() != 1 {
		t.Fatalf("second token: %v, fetches %d", err, is.JWKSFetches.Load())
	}

	// authentik rotates its key. Inside MinRefresh of the last fetch the
	// unknown kid is refused without fetching; after it, orbit refetches
	// and accepts.
	is.Rotate()
	rotated := is.Mint(is.Valid(client, group, "kanishk"))
	_, err := v.Verify(ctx, rotated)
	check(t, err, "kid")
	if is.JWKSFetches.Load() != 1 {
		t.Fatalf("refetched inside MinRefresh")
	}
	clk.Add(auth.MinRefresh)
	if _, err := v.Verify(ctx, rotated); err != nil {
		t.Fatalf("after refresh: %v", err)
	}
	if n := is.JWKSFetches.Load(); n != 2 {
		t.Fatalf("fetches = %d, want 2", n)
	}

	// A kid nobody has is refused, and refetches at most once per interval.
	clk.Add(auth.MinRefresh)
	for i := 0; i < 5; i++ {
		_, err := v.Verify(ctx, authtest.MintForeign(t, "nobody", is.Valid(client, group, "kanishk")))
		check(t, err, "kid")
	}
	if n := is.JWKSFetches.Load(); n != 3 {
		t.Fatalf("fetches = %d, want 3", n)
	}
}

func TestUnreachableIssuerIsUnavailableNotRejected(t *testing.T) {
	v, err := auth.New(auth.Config{Issuers: []string{"http://127.0.0.1:1/application/o/x/"}, ClientIDs: []string{client}, Group: group})
	if err != nil {
		t.Fatal(err)
	}
	is := authtest.NewIssuer(t)
	c := is.Valid(client, group, "kanishk")
	c.Issuer = "http://127.0.0.1:1/application/o/x/"
	_, err = v.Verify(context.Background(), is.Mint(c))
	if !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}
