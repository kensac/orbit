package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/testdb"
)

func TestPreflight(t *testing.T) {
	ctx := context.Background()
	s := testdb.Empty(t)
	if err := s.MigrateTo(ctx, 15); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := preflight(ctx, s, &out); err != nil {
		t.Fatalf("preflight from version 15: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "15 applied") || !strings.Contains(out.String(), "rolled back") {
		t.Fatalf("output:\n%s", out.String())
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := preflight(ctx, s, &out); err != nil || !strings.Contains(out.String(), "schema current") {
		t.Fatalf("after migrate: %v\n%s", err, out.String())
	}
}
