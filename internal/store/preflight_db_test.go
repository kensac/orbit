package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// The version applied before the search, tasks and skills migrations, so the
// dry run below creates and backfills over existing rows.
const preflightFrom = 15

func TestPlanMigrations(t *testing.T) {
	ctx := context.Background()
	s := testdb.Empty(t)
	all, err := store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.PlanMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Applied != 0 || len(p.Pending) != len(all) {
		t.Fatalf("new database: applied %d, pending %d, want 0 and %d", p.Applied, len(p.Pending), len(all))
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// A newer image migrated this database: the plan says so and has
	// nothing to apply.
	if _, err := s.Pool.Exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (99991231235959, true)`); err != nil {
		t.Fatal(err)
	}
	p, err = s.PlanMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Applied != len(all) || len(p.Pending) != 0 || !slices.Equal(p.Ahead, []int64{99991231235959}) {
		t.Fatalf("plan = applied %d pending %d ahead %v", p.Applied, len(p.Pending), p.Ahead)
	}
}

func TestDryRunMigrationsRollsBack(t *testing.T) {
	ctx := context.Background()
	s := testdb.Empty(t)
	if err := s.MigrateTo(ctx, preflightFrom); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO agent_specs (sha, agent, spec) VALUES ('s1', 'a', '{}');
		INSERT INTO agents (name, spec_sha, enabled) VALUES ('a', 's1', true);`); err != nil {
		t.Fatal(err)
	}
	before, err := s.PlanMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Pending) == 0 {
		t.Fatal("nothing pending above the starting version")
	}
	took, err := s.DryRunMigrations(ctx, before.Pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(took) != len(before.Pending) {
		t.Fatalf("timed %d of %d", len(took), len(before.Pending))
	}
	after, err := s.PlanMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Applied != before.Applied || len(after.Pending) != len(before.Pending) {
		t.Fatalf("the dry run applied something: before %+v, after %+v", before, after)
	}
	var tables int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE tablename IN ('tasks', 'skills', 'workspaces')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("%d tables of the pending migrations survived the dry run", tables)
	}
	// The real migration still applies after it.
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDryRunMigrationsFailsOnRows(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	if _, err := s.Pool.Exec(ctx, `INSERT INTO agent_specs (sha, agent, spec) VALUES ('s1', 'a', '{}');
		INSERT INTO agents (name, spec_sha, enabled) VALUES ('a', 's1', true);`); err != nil {
		t.Fatal(err)
	}
	bad := []store.Migration{
		{Version: 1, Name: "fine", Up: "ALTER TABLE agents ADD COLUMN note text;\nCREATE INDEX agents_note ON agents (note);\n"},
		{Version: 2, Name: "breaks", Up: "ALTER TABLE agents ADD COLUMN n int NOT NULL;"},
	}
	_, err := s.DryRunMigrations(ctx, bad)
	if err == nil || !strings.Contains(err.Error(), "2_breaks") || !strings.Contains(err.Error(), "contains null values") {
		t.Fatalf("err = %v, want 2_breaks failing on the existing row", err)
	}
	var cols int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'agents' AND column_name IN ('note', 'n')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 0 {
		t.Fatal("a failed dry run left columns behind")
	}
	if _, err := s.DryRunMigrations(ctx, []store.Migration{{Version: 3, Name: "concurrent", NoTx: true}}); err == nil {
		t.Fatal("a NO TRANSACTION migration was dry-run")
	}
}
