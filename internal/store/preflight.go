package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Migration is one embedded migration file.
type Migration struct {
	Version int64
	Name    string
	// Up is the file's Up section, as goose would run it.
	Up string
	// NoTx is goose's NO TRANSACTION annotation: it cannot be dry-run.
	NoTx bool
	// Contract is a migration marked `-- orbit:contract`: the second half
	// of an expand and contract, which drops what the previous release
	// already stopped using (docs/agents/store.md).
	Contract bool
	// Reviewed says why a migration ExpandFindings flags was safe for the
	// image running beside it (expandReviewed).
	Reviewed string
}

// expandReviewed are migrations ExpandFindings flags that were merged
// before the check existed, with why each kept the previous image working.
// Nothing is added here: a new one is expand-only or marked orbit:contract.
var expandReviewed = map[int64]string{
	4: "conversation_id is SET NOT NULL, and the orbit_conversation_fill trigger fills it for any writer that omits it",
}

// MigrationPlan is what Migrate would do to a database.
type MigrationPlan struct {
	// Applied counts the migrations this binary has that the database
	// already applied.
	Applied int
	// Ahead are versions the database applied that this binary does not
	// have: a newer image migrated it. Migrate ignores them.
	Ahead []int64
	// Pending are the migrations Migrate would apply, in its order.
	Pending []Migration
}

// Migrations are the embedded migration files, by version.
func Migrations() ([]Migration, error) {
	return loadMigrations(mustSub(migrations, "migrations"))
}

func loadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, rest, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		if !ok || err != nil {
			return nil, fmt.Errorf("%s: want <version>_<name>.sql", e.Name())
		}
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		m := parseMigration(string(b))
		m.Version, m.Name = v, strings.TrimSuffix(rest, ".sql")
		m.Reviewed = expandReviewed[v]
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Migration) int { return cmpInt64(a.Version, b.Version) })
	return out, nil
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// parseMigration takes the Up section (from `-- +goose Up` to `-- +goose
// Down`) and the file's annotations. The StatementBegin and End markers
// are comments to Postgres, so the section runs as one multi-statement
// string.
func parseMigration(src string) Migration {
	var m Migration
	var up strings.Builder
	in := false
	sc := bufio.NewScanner(strings.NewReader(src))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "-- +goose Up"):
			in = true
			continue
		case strings.HasPrefix(t, "-- +goose Down"):
			in = false
			continue
		case strings.HasPrefix(t, "-- +goose NO TRANSACTION"):
			m.NoTx = true
		case contractMarker.MatchString(t):
			m.Contract = true
		}
		if in {
			up.WriteString(line)
			up.WriteByte('\n')
		}
	}
	m.Up = up.String()
	return m
}

var contractMarker = regexp.MustCompile(`^--\s*orbit:contract\b`)

// PlanMigrations compares the embedded migrations with what the database
// applied (goose_db_version), as Migrate would; it reads only.
func (s *Store) PlanMigrations(ctx context.Context) (MigrationPlan, error) {
	all, err := Migrations()
	if err != nil {
		return MigrationPlan{}, err
	}
	applied := map[int64]bool{}
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT version_id FROM goose_db_version WHERE is_applied AND version_id > 0`)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "42P01":
		// No version table: a new database, every migration pending.
	case err != nil:
		return MigrationPlan{}, err
	default:
		vs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return MigrationPlan{}, err
		}
		for _, v := range vs {
			applied[v] = true
		}
	}
	var p MigrationPlan
	known := map[int64]bool{}
	for _, m := range all {
		known[m.Version] = true
		if applied[m.Version] {
			p.Applied++
		} else {
			p.Pending = append(p.Pending, m)
		}
	}
	for v := range applied {
		if !known[v] {
			p.Ahead = append(p.Ahead, v)
		}
	}
	slices.Sort(p.Ahead)
	return p, nil
}

// DryRunLockTimeout bounds each lock a dry run waits for: the live
// instance keeps serving, and a migration queued behind its transaction
// would block its readers too.
const DryRunLockTimeout = 5 * time.Second

// DryRunMigrations applies ms in order inside one transaction and rolls it
// back, so a migration that fails on this database's schema or rows fails
// here instead of in the new instance's start. It returns each one's time.
// Each statement waits at most DryRunLockTimeout for a lock.
func (s *Store) DryRunMigrations(ctx context.Context, ms []Migration) ([]time.Duration, error) {
	for _, m := range ms {
		if m.NoTx {
			return nil, fmt.Errorf("migration %d_%s runs outside a transaction (goose NO TRANSACTION), so it cannot be dry-run", m.Version, m.Name)
		}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, DryRunLockTimeout.Milliseconds())); err != nil {
		return nil, err
	}
	took := make([]time.Duration, 0, len(ms))
	for _, m := range ms {
		start := time.Now()
		// No arguments: pgx sends it over the simple protocol, which runs
		// a multi-statement string, as goose runs the file.
		if _, err := tx.Exec(ctx, m.Up); err != nil {
			return took, fmt.Errorf("migration %d_%s: %w", m.Version, m.Name, err)
		}
		took = append(took, time.Since(start))
	}
	return took, nil
}

// ExpandFindings lists what in a migration's Up section would break the
// previous image while both run against the new schema during a rollout
// (docs/agents/store.md, expand then contract): dropping or renaming what
// it reads, changing a column's type, a new NOT NULL it does not write, or
// a dropped default it relies on. It reads the SQL, not the database, so
// a new constraint the previous image would violate is caught only by
// CI's previous-binary test (deploy/compat.sh). Comments, string literals
// and dollar-quoted bodies are ignored.
func ExpandFindings(up string) []string {
	sql := strings.ToUpper(stripSQL(up))
	var out []string
	add := func(f string) {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	for _, m := range dropRE.FindAllStringSubmatch(sql, -1) {
		if !dropAllowed[m[1]] {
			add("DROP " + m[1])
		}
	}
	if renameRE.MatchString(sql) {
		add("RENAME")
	}
	if typeRE.MatchString(sql) {
		add("ALTER COLUMN ... TYPE")
	}
	if setNotNullRE.MatchString(sql) {
		add("SET NOT NULL")
	}
	for _, loc := range addRE.FindAllStringSubmatchIndex(sql, -1) {
		word := sql[loc[2]:loc[3]]
		if addNotColumn[word] {
			continue
		}
		clause := clauseFrom(sql, loc[1])
		if strings.Contains(clause, "NOT NULL") && !strings.Contains(clause, "DEFAULT") && !strings.Contains(clause, "GENERATED") {
			add("ADD COLUMN ... NOT NULL without a DEFAULT")
		}
	}
	return out
}

var (
	dropRE = regexp.MustCompile(`\bDROP\s+([A-Z_]+)`)
	// Dropping these never takes away something the previous image reads
	// or relies on when it writes.
	dropAllowed  = map[string]bool{"CONSTRAINT": true, "INDEX": true, "TRIGGER": true, "FUNCTION": true, "NOT": true, "POLICY": true}
	renameRE     = regexp.MustCompile(`\bRENAME\b`)
	typeRE       = regexp.MustCompile(`\bALTER\s+(COLUMN\s+)?[A-Z_0-9"]+\s+(SET\s+DATA\s+)?TYPE\b`)
	setNotNullRE = regexp.MustCompile(`\bSET\s+NOT\s+NULL\b`)
	// ALTER TABLE ... ADD [COLUMN] [IF NOT EXISTS] name: the word after ADD.
	addRE        = regexp.MustCompile(`\bADD\s+(?:COLUMN\s+)?(?:IF\s+NOT\s+EXISTS\s+)?([A-Z_0-9"]+)`)
	addNotColumn = map[string]bool{"CONSTRAINT": true, "PRIMARY": true, "UNIQUE": true, "FOREIGN": true, "CHECK": true, "EXCLUDE": true, "VALUE": true}
)

// clauseFrom is sql from i to the next comma or semicolon outside
// parentheses: one ADD COLUMN's definition.
func clauseFrom(sql string, i int) string {
	depth := 0
	for j := i; j < len(sql); j++ {
		switch sql[j] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return sql[i:j]
			}
			depth--
		case ',', ';':
			if depth == 0 {
				return sql[i:j]
			}
		}
	}
	return sql[i:]
}

// stripSQL blanks comments, string literals and dollar-quoted bodies, so
// the checks read only statements.
func stripSQL(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "--"):
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 4
			b.WriteByte(' ')
		case s[i] == '\'':
			i++
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteString("''")
		case s[i] == '$':
			if tag := dollarTag.FindString(s[i:]); tag != "" {
				end := strings.Index(s[i+len(tag):], tag)
				if end < 0 {
					return b.String()
				}
				i += len(tag) + end + len(tag)
				b.WriteString("''")
				continue
			}
			b.WriteByte(s[i])
			i++
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

var dollarTag = regexp.MustCompile(`^\$[A-Za-z_]*\$`)
