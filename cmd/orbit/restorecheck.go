package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/kensac/orbit/internal/store"
)

// restoreCheckCmd is `orbit restore-check [--expect FILE]`, the restore
// drill's last step (deploy/restore-drill.sh): it reads a restored copy of
// orbit-db through orbit's own store code on a connection whose every
// transaction is read only, so it can change nothing. It fails when the
// schema is behind this binary, when a table in FILE (lines of
// "table<TAB>rows", the dump's own counts) is missing or has fewer rows
// (migrations only add), or when any read fails.
func restoreCheckCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore-check", flag.ContinueOnError)
	expectPath := fs.String("expect", "", "row counts the copy must hold at least, one \"table<TAB>rows\" per line")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var expect map[string]int64
	if *expectPath != "" {
		f, err := os.Open(*expectPath)
		if err != nil {
			return err
		}
		expect, err = parseCounts(f)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", *expectPath, err)
		}
	}
	url := os.Getenv("ORBIT_DATABASE_URL")
	if url == "" {
		return fmt.Errorf("ORBIT_DATABASE_URL is not set")
	}
	s, err := store.OpenReadOnly(ctx, url)
	if err != nil {
		return err
	}
	defer s.Close()
	return restoreCheck(ctx, s, expect, os.Stdout)
}

func parseCounts(r io.Reader) (map[string]int64, error) {
	out := map[string]int64{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		table, count, ok := strings.Cut(line, "\t")
		c, err := strconv.ParseInt(strings.TrimSpace(count), 10, 64)
		if !ok || err != nil || table == "" {
			return nil, fmt.Errorf("line %d: want table<TAB>rows, got %q", n, line)
		}
		out[table] = c
	}
	return out, sc.Err()
}

func restoreCheck(ctx context.Context, s *store.Store, expect map[string]int64, w io.Writer) error {
	say := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	var ro string
	if err := s.Pool.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&ro); err != nil {
		return err
	}
	if ro != "on" {
		return fmt.Errorf("restore-check: the connection is not read only (transaction_read_only %s)", ro)
	}

	applied, err := s.AppliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("restore-check: applied migrations: %w", err)
	}
	embedded, err := store.EmbeddedVersions()
	if err != nil {
		return err
	}
	var behind, ahead []int64
	for _, v := range embedded {
		if !slices.Contains(applied, v) {
			behind = append(behind, v)
		}
	}
	for _, v := range applied {
		if !slices.Contains(embedded, v) {
			ahead = append(ahead, v)
		}
	}
	say("restore-check: %s: schema has %d of this binary's %d migrations\n", revision, len(embedded)-len(behind), len(embedded))
	if len(ahead) > 0 {
		say("restore-check: %d migration(s) newer than this binary: %v\n", len(ahead), ahead)
	}
	if len(behind) > 0 {
		return fmt.Errorf("restore-check: the schema is %d migration(s) behind this binary (%v): run orbit migrate on the copy first", len(behind), behind)
	}

	counts, err := s.TableCounts(ctx)
	if err != nil {
		return fmt.Errorf("restore-check: counting: %w", err)
	}
	var short []string
	tables := make([]string, 0, len(expect))
	for t := range expect {
		tables = append(tables, t)
	}
	slices.Sort(tables)
	for _, t := range tables {
		got, ok := counts[t]
		switch {
		case !ok:
			short = append(short, t+" is missing")
		case got < expect[t]:
			short = append(short, fmt.Sprintf("%s has %d rows, the dump %d", t, got, expect[t]))
		}
	}
	if len(short) > 0 {
		return fmt.Errorf("restore-check: %s", strings.Join(short, "; "))
	}
	if expect != nil {
		say("restore-check: %d table(s) hold at least the dump's rows\n", len(expect))
	}

	// Each read the API, the thread and the run page make, through the
	// store's own queries, on the restored rows.
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("restore-check: agents: %w", err)
	}
	if int64(len(agents)) != counts["agents"] {
		return fmt.Errorf("restore-check: ListAgents returned %d of %d agents", len(agents), counts["agents"])
	}
	var newest *store.Run
	var runs, convs int
	for _, a := range agents {
		rs, err := s.ListRuns(ctx, store.RunFilter{Agent: a.Name, Limit: 5})
		if err != nil {
			return fmt.Errorf("restore-check: runs of %s: %w", a.Name, err)
		}
		runs += len(rs)
		for i := range rs {
			if newest == nil || rs[i].CreatedAt.After(newest.CreatedAt) {
				newest = &rs[i]
			}
		}
		page, err := s.ListConversations(ctx, a.Name, false, nil, 5)
		if err != nil {
			return fmt.Errorf("restore-check: conversations of %s: %w", a.Name, err)
		}
		convs += len(page.Conversations)
		if _, err := s.ListMessages(ctx, a.Name, 5); err != nil {
			return fmt.Errorf("restore-check: messages of %s: %w", a.Name, err)
		}
	}
	if newest != nil {
		if _, err := s.GetRun(ctx, newest.ID); err != nil {
			return fmt.Errorf("restore-check: run %s: %w", newest.ID, err)
		}
		if _, err := s.ListAttempts(ctx, newest.ID); err != nil {
			return fmt.Errorf("restore-check: attempts of %s: %w", newest.ID, err)
		}
		if _, err := s.ListEvents(ctx, newest.ID, 0, 0, 100); err != nil {
			return fmt.Errorf("restore-check: events of %s: %w", newest.ID, err)
		}
		if _, err := s.ListArtifacts(ctx, newest.ID); err != nil {
			return fmt.Errorf("restore-check: artifacts of %s: %w", newest.ID, err)
		}
		if _, err := s.ListDeliveries(ctx, newest.ID); err != nil {
			return fmt.Errorf("restore-check: deliveries of %s: %w", newest.ID, err)
		}
	}
	entries, err := s.ListEntries(ctx, store.EntryFilter{Status: "all"})
	if err != nil {
		return fmt.Errorf("restore-check: memory entries: %w", err)
	}
	skills, err := s.ListSkills(ctx, "")
	if err != nil {
		return fmt.Errorf("restore-check: skills: %w", err)
	}
	if _, err := s.ListTickets(ctx, store.TicketFilter{Limit: 20}); err != nil {
		return fmt.Errorf("restore-check: tickets: %w", err)
	}
	if _, err := s.ListActions(ctx, "", "", 20); err != nil {
		return fmt.Errorf("restore-check: actions: %w", err)
	}
	if _, err := s.ListAttention(ctx, 20); err != nil {
		return fmt.Errorf("restore-check: attention: %w", err)
	}

	say("restore-check: read %d agent(s), %d recent run(s), %d conversation(s), %d memory entr(ies), %d skill(s)\n", len(agents), runs, convs, len(entries), len(skills))
	for _, t := range []string{"agents", "runs", "run_events", "messages", "conversations", "memory_entries", "actions", "tickets"} {
		if c, ok := counts[t]; ok {
			say("  %-15s %d\n", t, c)
		}
	}
	if newest != nil {
		say("restore-check: newest run %s (%s), created %s\n", newest.ID, newest.Agent, newest.CreatedAt.UTC().Format("2006-01-02 15:04Z"))
	}
	say("restore-check: ok\n")
	return nil
}
