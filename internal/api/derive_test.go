package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/url"
	"testing"
	"time"
)

// goldenRoot is the operator token the values below were recorded under, before the key derivations moved to internal/secret.
const goldenRoot = "0123456789abcdef0123456789abcdef-golden"

// TestUploadURLSignatureIsStable pins a signature recorded before the change: an upload URL handed out before a rollout must still verify after it.
func TestUploadURLSignatureIsStable(t *testing.T) {
	s := &Server{Token: goldenRoot}
	v := url.Values{"c": {"conv"}, "n": {"nonce"}, "t": {"text/plain"}, "b": {"10"}, "e": {"1800000000"}, "a": {"agent"}, "k": {"kind"}}
	if got, want := s.uploadSig(v), "3e680d13744ae935dbab5e0bc0311f2e0c977f562b5a0aadb8ee56994b805812"; got != want {
		t.Fatalf("uploadSig = %s, want %s", got, want)
	}
}

// TestOAuthStateSealedUnderTheOldKeyOpens seals with the key as orbit derived it before internal/secret, and opens with SetKey: a sign-in in flight across a rollout must complete.
func TestOAuthStateSealedUnderTheOldKeyOpens(t *testing.T) {
	m := hmac.New(sha256.New, []byte(goldenRoot))
	m.Write([]byte("orbit mcp oauth"))
	before := &OAuthProxy{key: m.Sum(nil)}
	type state struct {
		Kind string `json:"k"`
		X    string `json:"x"`
	}
	sealed, err := before.seal(state{Kind: "state", X: "in flight"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	after := &OAuthProxy{}
	after.SetKey([]byte(goldenRoot))
	var got state
	if err := after.open(sealed, "state", &got); err != nil || got.X != "in flight" {
		t.Fatalf("open = %+v, %v", got, err)
	}
}
