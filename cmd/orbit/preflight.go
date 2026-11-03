package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kensac/orbit/internal/store"
)

// preflightCmd is `orbit preflight [--config DIR]`, run from the new image
// against the live database before a rollout starts it: every pending
// migration is expand-only (the running image keeps working on the new
// schema while both run), and they all apply to this database's schema and
// rows, in one transaction that is rolled back. With --config it also loads
// that configuration, as the new instance will. It changes nothing; a
// failure means the rollout must not start the new image.
func preflightCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	config := fs.String("config", "", "also load this configuration directory, as the new instance would")
	worker := fs.Bool("worker", false, "check worker readiness without claiming runs or writing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *worker && *config != "" {
		return fmt.Errorf("preflight: --worker and --config cannot be combined")
	}
	if *worker {
		return withStore(ctx, func(s *store.Store) error { return workerPreflight(ctx, s, os.Stdout, os.LookupEnv) })
	}
	if *config != "" {
		if err := validate([]string{*config}); err != nil {
			return fmt.Errorf("preflight: configuration: %w", err)
		}
	}
	return withStore(ctx, func(s *store.Store) error { return preflight(ctx, s, os.Stdout) })
}

func preflight(ctx context.Context, s *store.Store, w io.Writer) error {
	say := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	plan, err := s.PlanMigrations(ctx)
	if err != nil {
		return fmt.Errorf("preflight: reading applied migrations: %w", err)
	}
	say("preflight: %s: %d applied, %d pending\n", revision, plan.Applied, len(plan.Pending))
	if len(plan.Ahead) > 0 {
		// Expand-only migrations keep an older binary working on a newer
		// schema, which is what a rollback to this image relies on.
		say("preflight: the database has %d migration(s) this image does not (a newer image applied them): %v\n", len(plan.Ahead), plan.Ahead)
	}
	var breaking []string
	for _, m := range plan.Pending {
		f := store.ExpandFindings(m.Up)
		switch {
		case len(f) == 0:
			say("  %d_%s\n", m.Version, m.Name)
		case m.Reviewed != "":
			say("  %d_%s: %s, reviewed: %s\n", m.Version, m.Name, strings.Join(f, ", "), m.Reviewed)
		case m.Contract:
			say("  %d_%s: contract (%s), marked orbit:contract\n", m.Version, m.Name, strings.Join(f, ", "))
		default:
			say("  %d_%s: NOT expand-only: %s\n", m.Version, m.Name, strings.Join(f, ", "))
			breaking = append(breaking, fmt.Sprintf("%d_%s", m.Version, m.Name))
		}
	}
	if len(breaking) > 0 {
		return fmt.Errorf("preflight: %s would break the running image while both run (docs/agents/store.md, expand then contract)", strings.Join(breaking, ", "))
	}
	if len(plan.Pending) == 0 {
		say("preflight: ok: schema current\n")
		return nil
	}
	took, err := s.DryRunMigrations(ctx, plan.Pending)
	if err != nil {
		return fmt.Errorf("preflight: dry run (rolled back): %w", err)
	}
	var total float64
	for i, d := range took {
		total += d.Seconds()
		if d.Seconds() >= 1 {
			say("  %d_%s took %.1fs (it holds its locks that long in the real migration)\n", plan.Pending[i].Version, plan.Pending[i].Name, d.Seconds())
		}
	}
	say("preflight: ok: %d migration(s) apply to this database (dry run %.1fs, rolled back)\n", len(took), total)
	return nil
}
