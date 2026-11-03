package connectors

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

type changingTokenStore struct{ token *StoredToken }

func (s *changingTokenStore) LatestToken(context.Context, string) (*StoredToken, error) {
	return s.token, nil
}

func (*changingTokenStore) RefreshToken(context.Context, string, int, func(StoredToken) ([]byte, *time.Time, error)) (*StoredToken, error) {
	return nil, errors.New("unexpected refresh")
}

func TestOAuthAuthStopsUsingCachedGrantAfterRevocation(t *testing.T) {
	seal, err := NewSealer("01234567890123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	tok := &Token{AccessToken: "secret-access-token", TokenEndpoint: "https://auth.example/token", ClientID: "orbit"}
	sealed, err := seal.Seal(SealKind, tok, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows := &changingTokenStore{token: &StoredToken{Version: 1, Sealed: sealed}}
	auth := &OAuthAuth{Conn: "acme", Store: rows, Sealer: seal}
	first := &http.Request{Header: make(http.Header)}
	if err := auth.Authorize(context.Background(), first); err != nil || first.Header.Get("Authorization") != "Bearer secret-access-token" {
		t.Fatalf("first authorization = %q, %v", first.Header.Get("Authorization"), err)
	}
	rows.token = nil
	second := &http.Request{Header: make(http.Header)}
	if err := auth.Authorize(context.Background(), second); !errors.Is(err, ErrNotSignedIn) || second.Header.Get("Authorization") != "" {
		t.Fatalf("authorization after revocation = %q, %v", second.Header.Get("Authorization"), err)
	}
}
