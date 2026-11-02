package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/kensac/orbit/internal/scheduletext"
	"github.com/kensac/orbit/internal/spec"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "orbitschedule:", err)
		os.Exit(1)
	}
}
func run(args []string, out io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("orbitschedule", flag.ContinueOnError)
	timezone := fs.String("timezone", "", "required IANA timezone (for example America/New_York)")
	anchorText := fs.String("anchor", "", "RFC3339 instant to preview after (default now)")
	count := fs.Int("count", 5, "number of upcoming fires, 1 through 20")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *timezone == "" || fs.NArg() == 0 {
		return errors.New("usage: orbitschedule --timezone America/New_York [--anchor RFC3339] [--count 5] 'every weekday at 9am'")
	}
	// A preview is a short review aid, not an unbounded recurrence export.
	const maxCount = 20
	if *count < 1 || *count > maxCount {
		return fmt.Errorf("count must be between 1 and %d", maxCount)
	}
	if *timezone == "Local" {
		return errors.New("timezone must name an explicit location, not Local")
	}
	loc, err := time.LoadLocation(*timezone)
	if err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	anchor := now
	if *anchorText != "" {
		anchor, err = time.Parse(time.RFC3339, *anchorText)
		if err != nil {
			return fmt.Errorf("anchor: %w", err)
		}
	}
	trigger, err := scheduletext.Parse(strings.Join(fs.Args(), " "), anchor, loc)
	if err != nil {
		return err
	}
	schedule, err := spec.TriggerSchedule(trigger, loc)
	if err != nil {
		return err
	}
	fires := make([]time.Time, 0, *count)
	next := anchor.In(loc)
	for range *count {
		next = schedule.Next(next)
		if next.IsZero() {
			break
		}
		fires = append(fires, next.In(loc))
	}
	result := struct {
		Timezone string       `json:"timezone"`
		Trigger  spec.Trigger `json:"trigger"`
		Next     []time.Time  `json:"next_fires"`
	}{*timezone, trigger, fires}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}
