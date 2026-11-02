package store

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenReadOnly connects to url with every transaction read only
// (default_transaction_read_only), so Postgres refuses any write a caller
// attempts: for reading a restored copy without changing it.
func OpenReadOnly(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// TableCounts is the exact row count of every table in the public schema.
func (s *Store) TableCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(names))
	for _, n := range names {
		var c int64
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM public.`+pgx.Identifier{n}.Sanitize()).Scan(&c); err != nil {
			return nil, err
		}
		out[n] = c
	}
	return out, nil
}

// AppliedVersions are the migration versions the database applied.
func (s *Store) AppliedVersions(ctx context.Context) ([]int64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT version_id FROM goose_db_version WHERE is_applied AND version_id > 0 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// EmbeddedVersions are the migration versions this binary carries, in
// order.
func EmbeddedVersions() ([]int64, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, e := range entries {
		prefix, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		out = append(out, v)
	}
	slices.Sort(out)
	return out, nil
}
