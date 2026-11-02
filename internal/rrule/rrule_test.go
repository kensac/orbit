package rrule

import (
	"strings"
	"testing"
	"time"
)

var ny = func() *time.Location {
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return l
}()

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, ny)
	if err != nil {
		panic(err)
	}
	return t
}

// series is the first n occurrences after (just before) the start.
func series(t *testing.T, rule, start string, n int) []string {
	t.Helper()
	r, err := Parse(rule, at(start))
	if err != nil {
		t.Fatalf("%s: %v", rule, err)
	}
	var out []string
	cur := r.Start.Add(-time.Second)
	for i := 0; i < n; i++ {
		cur = r.Next(cur)
		if cur.IsZero() {
			break
		}
		out = append(out, cur.Format("2006-01-02 15:04 MST"))
	}
	return out
}

func TestSeries(t *testing.T) {
	for _, c := range []struct {
		rule, start string
		want        []string
	}{
		{"FREQ=DAILY;COUNT=3", "2026-09-02 09:00", []string{"2026-09-02 09:00 EDT", "2026-09-03 09:00 EDT", "2026-09-04 09:00 EDT"}},
		// Every other week on Tuesday and Thursday.
		{"FREQ=WEEKLY;INTERVAL=2;BYDAY=TU,TH", "2026-11-03 09:00",
			[]string{"2026-11-03 09:00 EST", "2026-11-05 09:00 EST", "2026-11-17 09:00 EST", "2026-11-19 09:00 EST"}},
		// The last Friday of each month.
		{"RRULE:FREQ=MONTHLY;BYDAY=-1FR", "2026-11-01 17:30", []string{"2026-11-27 17:30 EST", "2026-12-25 17:30 EST", "2027-01-29 17:30 EST"}},
		// The second weekday of each month.
		{"FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=2", "2026-11-01 08:00", []string{"2026-11-03 08:00 EST", "2026-12-02 08:00 EST", "2027-01-04 08:00 EST"}},
		// The 31st, in the months that have one.
		{"FREQ=MONTHLY;BYMONTHDAY=31", "2026-10-31 12:00", []string{"2026-10-31 12:00 EDT", "2026-12-31 12:00 EST", "2027-01-31 12:00 EST", "2027-03-31 12:00 EDT"}},
		// The last day of the month.
		{"FREQ=MONTHLY;BYMONTHDAY=-1", "2027-01-31 06:00", []string{"2027-01-31 06:00 EST", "2027-02-28 06:00 EST", "2027-03-31 06:00 EDT"}},
		// February 29: leap years only.
		{"FREQ=YEARLY", "2028-02-29 10:00", []string{"2028-02-29 10:00 EST", "2032-02-29 10:00 EST"}},
		// Thanksgiving: the fourth Thursday of November.
		{"FREQ=YEARLY;BYMONTH=11;BYDAY=4TH", "2026-01-01 11:00", []string{"2026-11-26 11:00 EST", "2027-11-25 11:00 EST"}},
		// The first Monday of the year.
		{"FREQ=YEARLY;BYDAY=1MO", "2026-01-01 09:00", []string{"2026-01-05 09:00 EST", "2027-01-04 09:00 EST"}},
		// Two times of day, weekdays only.
		{"FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR;BYHOUR=9,17;BYMINUTE=0", "2026-11-06 00:00",
			[]string{"2026-11-06 09:00 EST", "2026-11-06 17:00 EST", "2026-11-09 09:00 EST"}},
		// Wall-clock time holds across the end of daylight saving.
		{"FREQ=DAILY", "2026-10-31 09:00", []string{"2026-10-31 09:00 EDT", "2026-11-01 09:00 EST", "2026-11-02 09:00 EST"}},
		// Hours are elapsed time: 01:00 happens twice on 2026-11-01, and
		// every three hours counts real hours.
		{"FREQ=HOURLY;INTERVAL=3", "2026-10-31 22:00", []string{"2026-10-31 22:00 EDT", "2026-11-01 01:00 EDT", "2026-11-01 03:00 EST", "2026-11-01 06:00 EST"}},
		// Every fifteen minutes in two hours on Mondays.
		{"FREQ=MINUTELY;INTERVAL=15;BYHOUR=9;BYDAY=MO", "2026-11-04 00:00",
			[]string{"2026-11-09 09:00 EST", "2026-11-09 09:15 EST", "2026-11-09 09:30 EST", "2026-11-09 09:45 EST", "2026-11-16 09:00 EST"}},
		// UNTIL as a date includes that whole day.
		{"FREQ=DAILY;UNTIL=20261105", "2026-11-04 09:00", []string{"2026-11-04 09:00 EST", "2026-11-05 09:00 EST"}},
		{"FREQ=DAILY;UNTIL=20261105T140000Z", "2026-11-04 09:00", []string{"2026-11-04 09:00 EST", "2026-11-05 09:00 EST"}},
		{"FREQ=DAILY;UNTIL=20261105T135959Z", "2026-11-04 09:00", []string{"2026-11-04 09:00 EST"}},
		// WKST decides which Sunday shares a week with a Tuesday.
		{"FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=MO", "2026-08-04 09:00",
			[]string{"2026-08-04 09:00 EDT", "2026-08-09 09:00 EDT", "2026-08-18 09:00 EDT", "2026-08-23 09:00 EDT"}},
		{"FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=SU", "2026-08-04 09:00",
			[]string{"2026-08-04 09:00 EDT", "2026-08-16 09:00 EDT", "2026-08-18 09:00 EDT", "2026-08-30 09:00 EDT"}},
	} {
		got := series(t, c.rule, c.start, len(c.want)+1)
		if len(got) > len(c.want) && !strings.Contains(c.rule, "COUNT") && !strings.Contains(c.rule, "UNTIL") {
			got = got[:len(c.want)]
		}
		if strings.Join(got, ", ") != strings.Join(c.want, ", ") {
			t.Errorf("%s from %s:\n got  %v\n want %v", c.rule, c.start, got, c.want)
		}
	}
}

func TestNextIsStrictlyAfterAndEnds(t *testing.T) {
	r, _ := Parse("FREQ=DAILY;COUNT=2", at("2026-11-02 09:00"))
	if got := r.Next(at("2026-11-02 09:00")); !got.Equal(at("2026-11-03 09:00")) {
		t.Fatalf("after the first = %v", got)
	}
	if got := r.Next(at("2026-11-03 09:00")); !got.IsZero() {
		t.Fatalf("after the last = %v, want zero", got)
	}
	// Interval alignment holds when Next starts far from the start.
	r, _ = Parse("FREQ=WEEKLY;INTERVAL=3;BYDAY=WE", at("2026-11-04 07:00"))
	if got := r.Next(at("2027-06-01 00:00")); !got.Equal(at("2027-06-02 07:00")) {
		t.Fatalf("aligned = %v", got)
	}
	// A rule with no occurrence within the horizon is zero, not a hang.
	r, _ = Parse("FREQ=MONTHLY;BYMONTH=2;BYMONTHDAY=30", at("2026-01-01 00:00"))
	if got := r.Next(at("2026-01-01 00:00")); !got.IsZero() {
		t.Fatalf("February 30 = %v", got)
	}
	// A sparse minutely rule is found without walking every minute.
	r, _ = Parse("FREQ=MINUTELY;BYMONTH=12;BYHOUR=3;BYMINUTE=7", at("2026-01-01 00:00"))
	start := time.Now()
	if got := r.Next(at("2026-01-01 00:00")); !got.Equal(at("2026-12-01 03:07")) {
		t.Fatalf("sparse minutely = %v", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("sparse minutely took %s", d)
	}
}

func TestParseRefuses(t *testing.T) {
	start := at("2026-11-02 09:00")
	for rule, want := range map[string]string{
		"":                                  "empty",
		"INTERVAL=2":                        "FREQ is required",
		"FREQ=SECONDLY":                     "not supported",
		"FREQ=DAILY;COUNT=2;UNTIL=20270101": "COUNT and UNTIL",
		"FREQ=DAILY;UNTIL=20250101":         "before the start",
		"FREQ=YEARLY;BYYEARDAY=100":         "BYYEARDAY is not supported",
		"FREQ=YEARLY;BYWEEKNO=20":           "BYWEEKNO is not supported",
		"FREQ=WEEKLY;BYDAY=1MO":             "only for MONTHLY and YEARLY",
		"FREQ=MONTHLY;BYDAY=6MO":            "at most five",
		"FREQ=WEEKLY;BYMONTHDAY=1":          "not for WEEKLY",
		"FREQ=DAILY;BYHOUR=24":              "out of range",
		"FREQ=DAILY;BYMONTHDAY=0":           "out of range",
		"FREQ=DAILY;INTERVAL=0":             "whole number",
		"FREQ=DAILY;FREQ=WEEKLY":            "twice",
		"FREQ=DAILY;BYDAY=XX":               "not a weekday",
		"FREQ=DAILY;TZID=UTC":               "unknown rule part",
		"FREQ=DAILY;COUNT=10001":            "whole number",
		"FREQ=DAILY;nonsense":               "NAME=VALUE",
	} {
		_, err := Parse(rule, start)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want %q", rule, err, want)
		}
	}
	if _, err := Parse("FREQ=DAILY", time.Time{}); err == nil {
		t.Error("a rule with no start parsed")
	}
}
