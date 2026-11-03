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
