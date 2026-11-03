package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestCatalogConnectorsAreUniqueAndOrdered(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	for _, c := range []store.CatalogConnector{
		{Name: "zeta", URL: "https://zeta.example/mcp", CreatedBy: "person", Scopes: []string{"read"}},
		{Name: "alpha", URL: "https://alpha.example/mcp", CreatedBy: "person"},
	} {
		if _, err := s.AddCatalogConnector(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddCatalogConnector(ctx, store.CatalogConnector{Name: "alpha", URL: "https://other.example/mcp", CreatedBy: "person"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name error = %v", err)
	}
	if _, err := s.AddCatalogConnector(ctx, store.CatalogConnector{Name: "other", URL: "https://alpha.example/mcp", CreatedBy: "person"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate URL error = %v", err)
	}
	got, err := s.CatalogConnectors(ctx)
	if err != nil || len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" || got[0].Scopes == nil {
		t.Fatalf("catalog = %+v, %v", got, err)
	}
	n, err := s.ConnectorCatalogGeneration(ctx)
	if err != nil || n != 2 {
		t.Fatalf("generation = %d, %v", n, err)
	}
}

func TestRevokingConnectorGrantRetainsHistoryAndAllowsReauthorization(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	if _, err := s.AddCatalogConnector(ctx, store.CatalogConnector{Name: "acme", URL: "https://acme.example/mcp", CreatedBy: "person"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.PutConnectorToken(ctx, "acme", []byte("sealed-one"), nil, "person")
	if err != nil || first != 1 {
		t.Fatalf("first token version = %d, %v", first, err)
	}
	revoked, err := s.RevokeConnectorToken(ctx, "acme", "person")
	if err != nil || revoked != first {
		t.Fatalf("revoked version = %d, %v", revoked, err)
	}
	if got, err := s.LatestConnectorToken(ctx, "acme"); err != nil || got != nil {
		t.Fatalf("active token after revoke = %+v, %v", got, err)
	}
	if got, err := s.ConnectorTokens(ctx); err != nil || len(got) != 0 {
		t.Fatalf("listed tokens after revoke = %+v, %v", got, err)
	}
	if _, err := s.RevokeConnectorToken(ctx, "acme", "person"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("repeated revoke error = %v", err)
	}
	second, err := s.PutConnectorToken(ctx, "acme", []byte("sealed-two"), nil, "person")
	if err != nil || second != 2 {
		t.Fatalf("second token version = %d, %v", second, err)
	}
	got, err := s.LatestConnectorToken(ctx, "acme")
	if err != nil || got == nil || got.Version != 2 || string(got.Sealed) != "sealed-two" {
		t.Fatalf("reauthorized token = %+v, %v", got, err)
	}
}
