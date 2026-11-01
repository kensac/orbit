// Package testdb gives each test its own freshly migrated database on the
// server named by ORBIT_TEST_DATABASE_URL. Without it the test skips, except
// under CI, where a skipped database test would make a green run mean
// nothing.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/kensac/orbit/internal/store"
)

// New returns a store on a new, fully migrated database, dropped when the
// test ends.
func New(t *testing.T) *store.Store {
	t.Helper()
	s := Empty(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// Empty is New without migrations, for tests of an upgrade.
func Empty(t *testing.T) *store.Store {
	t.Helper()
	base := os.Getenv("ORBIT_TEST_DATABASE_URL")
	if base == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("ORBIT_TEST_DATABASE_URL is not set under CI")
		}
		t.Skip("ORBIT_TEST_DATABASE_URL is not set")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if name := strings.TrimPrefix(u.Path, "/"); name != "" && !strings.Contains(name, "test") && name != "postgres" {
		// The server is only used to create throwaway databases, but refuse
		// a URL that looks like it names a real one.
		t.Fatalf("ORBIT_TEST_DATABASE_URL names database %q; point it at a test server", name)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "orbit_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	s, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	return s
}
