package store

import (
	"slices"
	"testing"
)

func TestExpandFindings(t *testing.T) {
	for _, c := range []struct {
		name, sql string
		want      []string
	}{
		{"new table", `CREATE TABLE t (id bigint PRIMARY KEY, s text NOT NULL);`, nil},
		{"nullable column", `ALTER TABLE runs ADD COLUMN note text;`, nil},
		{"not null with default", `ALTER TABLE runs ADD COLUMN n int NOT NULL DEFAULT 0;`, nil},
		{"generated", `ALTER TABLE runs ADD COLUMN g int NOT NULL GENERATED ALWAYS AS (1) STORED;`, nil},
		{"not null without default", `ALTER TABLE runs ADD COLUMN n int NOT NULL;`, []string{"ADD COLUMN ... NOT NULL without a DEFAULT"}},
		{"without COLUMN keyword", `ALTER TABLE runs ADD IF NOT EXISTS n int REFERENCES t (id) NOT NULL, ADD m int;`, []string{"ADD COLUMN ... NOT NULL without a DEFAULT"}},
		{"second clause only", `ALTER TABLE runs ADD COLUMN a int NOT NULL DEFAULT 1, ADD COLUMN b int NOT NULL;`, []string{"ADD COLUMN ... NOT NULL without a DEFAULT"}},
		{"constraint", `ALTER TABLE runs ADD CONSTRAINT c CHECK (x IS NOT NULL);`, nil},
		{"drop column", `ALTER TABLE runs DROP COLUMN IF EXISTS note;`, []string{"DROP COLUMN"}},
		{"drop column, implicit", `ALTER TABLE runs DROP note;`, []string{"DROP NOTE"}},
		{"drop table", `DROP TABLE IF EXISTS old;`, []string{"DROP TABLE"}},
		{"drop default", `ALTER TABLE runs ALTER COLUMN n DROP DEFAULT;`, []string{"DROP DEFAULT"}},
		{"allowed drops", `DROP INDEX i; DROP TRIGGER t ON runs; ALTER TABLE runs DROP CONSTRAINT c, ALTER COLUMN n DROP NOT NULL; DROP FUNCTION f();`, nil},
		{"rename", `ALTER TABLE runs RENAME COLUMN a TO b;`, []string{"RENAME"}},
		{"type", `ALTER TABLE runs ALTER COLUMN n TYPE bigint;`, []string{"ALTER COLUMN ... TYPE"}},
		{"set data type", `ALTER TABLE runs ALTER n SET DATA TYPE bigint;`, []string{"ALTER COLUMN ... TYPE"}},
		{"set not null", `ALTER TABLE runs ALTER COLUMN n SET NOT NULL;`, []string{"SET NOT NULL"}},
		{"enum value", `ALTER TYPE status ADD VALUE 'x';`, nil},
		{"comments and strings", "-- DROP TABLE runs\n/* RENAME */ INSERT INTO t VALUES ('DROP TABLE x', 'it''s RENAME');", nil},
		{"function body", "CREATE FUNCTION f() RETURNS trigger AS $$ BEGIN DROP TABLE x; END $$ LANGUAGE plpgsql;\nCREATE FUNCTION g() RETURNS int AS $body$ SELECT 1 $body$ LANGUAGE sql;", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ExpandFindings(c.sql); !slices.Equal(got, c.want) {
				t.Errorf("ExpandFindings = %q, want %q", got, c.want)
			}
		})
	}
}

// A migration that is not expand-only breaks the image still running during a rollout. Mark the contract half of an expand and contract with `-- orbit:contract` (docs/agents/store.md); orbit preflight then reports it instead of refusing it.
func TestMigrationsAreExpandOnly(t *testing.T) {
	ms, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		f := ExpandFindings(m.Up)
		if m.Reviewed != "" {
			if len(f) == 0 {
				t.Errorf("%d_%s is in expandReviewed but the check finds nothing: remove it", m.Version, m.Name)
			}
			continue
		}
		if len(f) > 0 && !m.Contract {
			t.Errorf("%d_%s is not expand-only (%v): the previous image would break on it during a rollout. Split it into expand now and contract in a later release, marked -- orbit:contract", m.Version, m.Name, f)
		}
		if m.NoTx {
			t.Errorf("%d_%s runs outside a transaction: orbit preflight cannot dry-run it", m.Version, m.Name)
		}
	}
}

func TestParseMigration(t *testing.T) {
	m := parseMigration("-- orbit:contract: runs.note unused since the previous release\n-- +goose Up\n-- +goose StatementBegin\nSELECT 1;\n-- +goose StatementEnd\n\n-- +goose Down\nSELECT 2;\n")
	if !m.Contract || m.NoTx {
		t.Errorf("contract %v, notx %v", m.Contract, m.NoTx)
	}
	if want := "-- +goose StatementBegin\nSELECT 1;\n-- +goose StatementEnd\n\n"; m.Up != want {
		t.Errorf("Up = %q, want %q", m.Up, want)
	}
	if m := parseMigration("-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX CONCURRENTLY i ON t (x);\n"); !m.NoTx {
		t.Error("NO TRANSACTION not seen")
	}
}
