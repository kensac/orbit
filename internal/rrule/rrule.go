// Package rrule is the subset of RFC 5545 recurrence rules orbit schedules
// with: what a cron expression cannot say (every other Tuesday, the last
// Friday of the month, the second weekday, a series that ends). A rule is
// anchored at a start time in the agent's timezone; occurrences are local
// wall-clock times there, so 09:00 stays 09:00 across daylight saving.
//
// Supported: FREQ (MINUTELY to YEARLY), INTERVAL, COUNT, UNTIL, BYMONTH,
// BYMONTHDAY, BYDAY (with ordinals in MONTHLY and YEARLY), BYHOUR,
// BYMINUTE, BYSECOND, BYSETPOS and WKST. BYYEARDAY, BYWEEKNO and SECONDLY
// are refused rather than half supported.
package rrule

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Freq is a rule's frequency: the period each interval steps by.
type Freq int

// The frequencies orbit accepts.
const (
	Minutely Freq = iota
	Hourly
	Daily
	Weekly
	Monthly
	Yearly
)

var freqs = map[string]Freq{"MINUTELY": Minutely, "HOURLY": Hourly, "DAILY": Daily, "WEEKLY": Weekly, "MONTHLY": Monthly, "YEARLY": Yearly}

var weekdays = map[string]time.Weekday{"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday}

// Day is one BYDAY entry: a weekday, and with N set the Nth (negative:
// from the end) such weekday of the month or year.
type Day struct {
	Weekday time.Weekday
	N       int
}

// Rule is a parsed recurrence rule with its start.
type Rule struct {
	Freq     Freq
	Interval int
	// Count ends the series after that many occurrences; 0 is unbounded.
	Count int
	// Until ends the series after that instant; zero is unbounded.
	Until      time.Time
	ByMonth    []int
	ByMonthDay []int
	ByDay      []Day
	ByHour     []int
	ByMinute   []int
	BySecond   []int
	BySetPos   []int
	Wkst       time.Weekday
	// Start is the series' anchor, in its location: the first period, and
	// the time of day and date parts a rule does not name.
	Start time.Time
}

// MaxCount bounds COUNT: a series is enumerated from its start to count
// occurrences, so its length bounds the work of every Next.
const MaxCount = 10000

// horizon is how far ahead Next looks before deciding a rule has no more
// occurrences. Eight years holds a February 29 rule across a century
// year that is not a leap year (2096 to 2104).
const horizon = 9 * 366 * 24 * time.Hour

// Parse reads a rule ("FREQ=WEEKLY;INTERVAL=2;BYDAY=TU", an optional
// "RRULE:" prefix) anchored at start. A floating UNTIL (no Z) is read in
// start's location.
func Parse(s string, start time.Time) (*Rule, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "RRULE:"))
	if s == "" {
		return nil, errors.New("empty rule")
	}
	if start.IsZero() {
		return nil, errors.New("a rule needs a start")
	}
	r := &Rule{Interval: 1, Wkst: time.Monday, Start: start.Truncate(time.Second)}
	seen := map[string]bool{}
	hasFreq := false
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		k = strings.ToUpper(strings.TrimSpace(k))
		v = strings.ToUpper(strings.TrimSpace(v))
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("%q is not NAME=VALUE", part)
		}
		if seen[k] {
			return nil, fmt.Errorf("%s given twice", k)
		}
		seen[k] = true
		var err error
		switch k {
		case "FREQ":
			f, ok := freqs[v]
			if !ok {
				return nil, fmt.Errorf("FREQ=%s is not supported (want one of MINUTELY, HOURLY, DAILY, WEEKLY, MONTHLY, YEARLY)", v)
			}
			r.Freq, hasFreq = f, true
		case "INTERVAL":
			r.Interval, err = number(k, v, 1, 1000)
		case "COUNT":
			r.Count, err = number(k, v, 1, MaxCount)
		case "UNTIL":
			r.Until, err = until(v, start.Location())
		case "BYMONTH":
			r.ByMonth, err = list(k, v, 1, 12, false)
		case "BYMONTHDAY":
			r.ByMonthDay, err = list(k, v, 1, 31, true)
		case "BYHOUR":
			r.ByHour, err = list(k, v, 0, 23, false)
		case "BYMINUTE":
			r.ByMinute, err = list(k, v, 0, 59, false)
		case "BYSECOND":
			r.BySecond, err = list(k, v, 0, 59, false)
		case "BYSETPOS":
			r.BySetPos, err = list(k, v, 1, 366, true)
		case "BYDAY":
			r.ByDay, err = days(v)
		case "WKST":
			wd, ok := weekdays[v]
			if !ok {
				return nil, fmt.Errorf("WKST=%s is not a weekday", v)
			}
			r.Wkst = wd
		case "BYYEARDAY", "BYWEEKNO":
			return nil, fmt.Errorf("%s is not supported", k)
		default:
			return nil, fmt.Errorf("unknown rule part %s", k)
		}
		if err != nil {
			return nil, err
		}
	}
	switch {
	case !hasFreq:
		return nil, errors.New("FREQ is required")
	case r.Count > 0 && !r.Until.IsZero():
		return nil, errors.New("COUNT and UNTIL cannot both be given")
	case !r.Until.IsZero() && r.Until.Before(r.Start):
		return nil, errors.New("UNTIL is before the start")
	}
	for _, d := range r.ByDay {
		if d.N != 0 && r.Freq != Monthly && r.Freq != Yearly {
			return nil, errors.New("a numbered BYDAY (1MO, -1FR) is only for MONTHLY and YEARLY")
		}
		if d.N != 0 && r.Freq == Monthly && (d.N > 5 || d.N < -5) {
			return nil, fmt.Errorf("BYDAY %d: a month has at most five of a weekday", d.N)
		}
	}
	if len(r.ByMonthDay) > 0 && r.Freq == Weekly {
		return nil, errors.New("BYMONTHDAY is not for WEEKLY")
	}
	return r, nil
}

func number(k, v string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s=%s: want a whole number from %d to %d", k, v, lo, hi)
	}
	return n, nil
}

// list reads a comma list of numbers in [lo, hi], or with signed also in
// [-hi, -lo] (counted from the end).
func list(k, v string, lo, hi int, signed bool) ([]int, error) {
	var out []int
	for _, f := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimPrefix(f, "+"))
		ok := err == nil && ((n >= lo && n <= hi) || (signed && n <= -lo && n >= -hi))
		if !ok {
			return nil, fmt.Errorf("%s: %q is out of range", k, f)
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

func days(v string) ([]Day, error) {
	var out []Day
	for _, f := range strings.Split(v, ",") {
		if len(f) < 2 {
			return nil, fmt.Errorf("BYDAY: %q is not a weekday", f)
		}
		wd, ok := weekdays[f[len(f)-2:]]
		if !ok {
			return nil, fmt.Errorf("BYDAY: %q is not a weekday", f)
		}
		d := Day{Weekday: wd}
		if p := f[:len(f)-2]; p != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(p, "+"))
			if err != nil || n == 0 || n > 53 || n < -53 {
				return nil, fmt.Errorf("BYDAY: %q has a bad ordinal", f)
			}
			d.N = n
		}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out, nil
}

func until(v string, loc *time.Location) (time.Time, error) {
	for _, layout := range []string{"20060102T150405Z", "20060102T150405", "20060102"} {
		var t time.Time
		var err error
		if strings.HasSuffix(layout, "Z") {
			t, err = time.Parse(layout, v)
		} else {
			t, err = time.ParseInLocation(layout, v, loc)
		}
		if err == nil {
			if layout == "20060102" {
				t = t.Add(24*time.Hour - time.Second) // the whole day
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("UNTIL=%s: want YYYYMMDD, YYYYMMDDTHHMMSS or YYYYMMDDTHHMMSSZ", v)
}

// Next returns the first occurrence strictly after t, or the zero time when
// the series has ended or has none within the horizon.
func (r *Rule) Next(t time.Time) time.Time {
	t = t.In(r.Start.Location())
	if r.Count > 0 {
		// Occurrences are counted from the start, so the series is walked
		// from there.
		n := 0
		var found time.Time
		r.walk(r.Start, func(o time.Time) bool {
			n++
			if o.After(t) {
				found = o
				return false
			}
			return n < r.Count
		})
		return found
	}
	from := r.Start
	if t.After(from) {
		from = t
	}
	var found time.Time
	r.walk(from, func(o time.Time) bool {
		if o.After(t) {
			found = o
			return false
		}
		return true
	})
	return found
}

// walk calls yield with each occurrence at or after from, in order, until
// yield returns false, the series passes UNTIL, or the horizon is reached.
func (r *Rule) walk(from time.Time, yield func(time.Time) bool) {
	end := from.Add(horizon)
	for k := r.firstPeriod(from); ; k += r.Interval {
		ps := r.periodStart(k)
		if ps.After(end) || (!r.Until.IsZero() && ps.After(r.Until)) {
			return
		}
		if skip := r.skipTo(ps); !skip.IsZero() {
			// An hour or a minute on a day (or in an hour) the rule
			// excludes: jump to the first aligned period after it.
			n := r.index(skip)
			if rem := n % r.Interval; rem != 0 {
				n += r.Interval - rem
			}
			k = max(n, k+r.Interval) - r.Interval
			continue
		}
		for _, o := range r.expand(ps) {
			if o.Before(r.Start) || o.Before(from) {
				continue
			}
			if !r.Until.IsZero() && o.After(r.Until) {
				return
			}
			if !yield(o) {
				return
			}
		}
	}
}

// skipTo is, for an hourly or minutely period the day or hour limits
// exclude, the start of the next day or hour that could match; zero
// otherwise.
func (r *Rule) skipTo(ps time.Time) time.Time {
	if r.Freq != Hourly && r.Freq != Minutely {
		return time.Time{}
	}
	if !r.dayMatches(ps) {
		return time.Date(ps.Year(), ps.Month(), ps.Day()+1, 0, 0, 0, 0, ps.Location())
	}
	if r.Freq == Minutely && len(r.ByHour) > 0 && !slices.Contains(r.ByHour, ps.Hour()) {
		return time.Date(ps.Year(), ps.Month(), ps.Day(), ps.Hour()+1, 0, 0, 0, ps.Location())
	}
	return time.Time{}
}

// date is a calendar date at midnight UTC, for counting days without
// daylight saving in the way.
func date(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func daysBetween(a, b time.Time) int { return int(date(b).Sub(date(a)).Hours() / 24) }

// weekStart is the first day of t's week, by WKST.
func (r *Rule) weekStart(t time.Time) time.Time {
	d := date(t)
	return d.AddDate(0, 0, -((int(d.Weekday()) - int(r.Wkst) + 7) % 7))
}

// index is the period number of t counted from the start's period.
func (r *Rule) index(t time.Time) int {
	s := r.Start
	switch r.Freq {
	case Yearly:
		return t.Year() - s.Year()
	case Monthly:
		return (t.Year()*12 + int(t.Month())) - (s.Year()*12 + int(s.Month()))
	case Weekly:
		return daysBetween(r.weekStart(s), r.weekStart(t)) / 7
	case Daily:
		return daysBetween(s, t)
	case Hourly:
		return int(t.Sub(r.periodStart(0)) / time.Hour)
	default:
		return int(t.Sub(r.periodStart(0)) / time.Minute)
	}
}

// firstPeriod is the first period aligned with INTERVAL that can hold an
// occurrence at or after from.
func (r *Rule) firstPeriod(from time.Time) int {
	k := r.index(from)
	if k < 0 {
		return 0
	}
	return k - k%r.Interval
}

// periodStart is the first instant of period k, in the start's location.
// Days and longer are wall-clock periods; hours and minutes are elapsed
// time, so a daylight saving change neither repeats nor skips one.
func (r *Rule) periodStart(k int) time.Time {
	s, loc := r.Start, r.Start.Location()
	switch r.Freq {
	case Yearly:
		return time.Date(s.Year()+k, 1, 1, 0, 0, 0, 0, loc)
	case Monthly:
		return time.Date(s.Year(), s.Month()+time.Month(k), 1, 0, 0, 0, 0, loc)
	case Weekly:
		w := r.weekStart(s).AddDate(0, 0, 7*k)
		return time.Date(w.Year(), w.Month(), w.Day(), 0, 0, 0, 0, loc)
	case Daily:
		return time.Date(s.Year(), s.Month(), s.Day()+k, 0, 0, 0, 0, loc)
	case Hourly:
		return time.Date(s.Year(), s.Month(), s.Day(), s.Hour(), 0, 0, 0, loc).Add(time.Duration(k) * time.Hour)
	default:
		return time.Date(s.Year(), s.Month(), s.Day(), s.Hour(), s.Minute(), 0, 0, loc).Add(time.Duration(k) * time.Minute)
	}
}

// expand lists the occurrences of the period starting at ps, sorted, after
// BYSETPOS.
func (r *Rule) expand(ps time.Time) []time.Time {
	var out []time.Time
	switch r.Freq {
	case Hourly, Minutely:
		if !r.dayMatches(ps) || (len(r.ByHour) > 0 && !slices.Contains(r.ByHour, ps.Hour())) {
			break
		}
		if r.Freq == Minutely && len(r.ByMinute) > 0 && !slices.Contains(r.ByMinute, ps.Minute()) {
			break
		}
		mins := []int{0}
		if r.Freq == Hourly {
			mins = orDefault(r.ByMinute, r.Start.Minute())
		}
		for _, m := range mins {
			for _, sec := range orDefault(r.BySecond, r.Start.Second()) {
				out = append(out, ps.Add(time.Duration(m)*time.Minute+time.Duration(sec)*time.Second))
			}
		}
	default:
		loc := ps.Location()
		for _, d := range r.dates(ps) {
			for _, h := range orDefault(r.ByHour, r.Start.Hour()) {
				for _, m := range orDefault(r.ByMinute, r.Start.Minute()) {
					for _, sec := range orDefault(r.BySecond, r.Start.Second()) {
						out = append(out, time.Date(d.Year(), d.Month(), d.Day(), h, m, sec, 0, loc))
					}
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	out = slices.CompactFunc(out, func(a, b time.Time) bool { return a.Equal(b) })
	return r.setPos(out)
}

func orDefault(v []int, def int) []int {
	if len(v) > 0 {
		return v
	}
	return []int{def}
}

func (r *Rule) setPos(set []time.Time) []time.Time {
	if len(r.BySetPos) == 0 {
		return set
	}
	var out []time.Time
	for _, p := range r.BySetPos {
		i := p - 1
		if p < 0 {
			i = len(set) + p
		}
		if i >= 0 && i < len(set) {
			out = append(out, set[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return slices.CompactFunc(out, func(a, b time.Time) bool { return a.Equal(b) })
}

// dayMatches applies BYMONTH, BYMONTHDAY and BYDAY as limits on a day.
func (r *Rule) dayMatches(t time.Time) bool {
	if len(r.ByMonth) > 0 && !slices.Contains(r.ByMonth, int(t.Month())) {
		return false
	}
	if len(r.ByMonthDay) > 0 && !monthDayMatches(r.ByMonthDay, t) {
		return false
	}
	if len(r.ByDay) > 0 && !slices.ContainsFunc(r.ByDay, func(d Day) bool { return d.Weekday == t.Weekday() }) {
		return false
	}
	return true
}

func daysIn(y int, m time.Month) int { return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day() }

func monthDayMatches(mds []int, t time.Time) bool {
	n := daysIn(t.Year(), t.Month())
	for _, md := range mds {
		if md == t.Day() || (md < 0 && n+md+1 == t.Day()) {
			return true
		}
	}
	return false
}

// dates lists the days of a day-or-longer period that hold occurrences, as
// UTC midnights.
func (r *Rule) dates(ps time.Time) []time.Time {
	y, s := ps.Year(), r.Start
	switch r.Freq {
	case Daily:
		d := date(ps)
		if r.dayMatches(d) {
			return []time.Time{d}
		}
		return nil
	case Weekly:
		var out []time.Time
		first := date(ps)
		for i := 0; i < 7; i++ {
			d := first.AddDate(0, 0, i)
			if len(r.ByDay) == 0 && d.Weekday() != s.Weekday() {
				continue
			}
			if len(r.ByMonth) > 0 && !slices.Contains(r.ByMonth, int(d.Month())) {
				continue
			}
			if len(r.ByDay) > 0 && !slices.ContainsFunc(r.ByDay, func(x Day) bool { return x.Weekday == d.Weekday() }) {
				continue
			}
			out = append(out, d)
		}
		return out
	case Monthly:
		if len(r.ByMonth) > 0 && !slices.Contains(r.ByMonth, int(ps.Month())) {
			return nil
		}
		return r.monthDates(y, ps.Month(), true)
	default: // Yearly
		if len(r.ByMonth) == 0 && len(r.ByMonthDay) == 0 && len(r.ByDay) > 0 {
			return r.yearWeekdays(y)
		}
		months := r.ByMonth
		if len(months) == 0 {
			if len(r.ByMonthDay) > 0 {
				months = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
			} else {
				months = []int{int(s.Month())}
			}
		}
		var out []time.Time
		for _, m := range months {
			out = append(out, r.monthDates(y, time.Month(m), len(r.ByMonth) > 0)...)
		}
		return out
	}
}

// monthDates lists the days of one month a rule names: BYMONTHDAY, limited
// by BYDAY; else BYDAY (ordinals within the month when ordinal is set);
// else the start's day of the month.
func (r *Rule) monthDates(y int, m time.Month, ordinal bool) []time.Time {
	n := daysIn(y, m)
	var out []time.Time
	add := func(day int) {
		if day >= 1 && day <= n {
			out = append(out, time.Date(y, m, day, 0, 0, 0, 0, time.UTC))
		}
	}
	switch {
	case len(r.ByMonthDay) > 0:
		for _, md := range r.ByMonthDay {
			day := md
			if md < 0 {
				day = n + md + 1
			}
			if day < 1 || day > n {
				continue
			}
			d := time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
			if len(r.ByDay) > 0 && !slices.ContainsFunc(r.ByDay, func(x Day) bool { return x.Weekday == d.Weekday() }) {
				continue
			}
			out = append(out, d)
		}
	case len(r.ByDay) > 0:
		for _, x := range r.ByDay {
			var hits []int
			for day := 1; day <= n; day++ {
				if time.Date(y, m, day, 0, 0, 0, 0, time.UTC).Weekday() == x.Weekday {
					hits = append(hits, day)
				}
			}
			if x.N == 0 || !ordinal {
				for _, h := range hits {
					add(h)
				}
				continue
			}
			if i := ordinalIndex(x.N, len(hits)); i >= 0 {
				add(hits[i])
			}
		}
	default:
		add(r.Start.Day())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// yearWeekdays is BYDAY across a whole year (YEARLY with no BYMONTH): with
// an ordinal, the Nth such weekday of the year.
func (r *Rule) yearWeekdays(y int) []time.Time {
	var out []time.Time
	first := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
	days := 365
	if daysIn(y, 2) == 29 {
		days = 366
	}
	for _, x := range r.ByDay {
		var hits []time.Time
		for i := 0; i < days; i++ {
			if d := first.AddDate(0, 0, i); d.Weekday() == x.Weekday {
				hits = append(hits, d)
			}
		}
		if x.N == 0 {
			out = append(out, hits...)
		} else if i := ordinalIndex(x.N, len(hits)); i >= 0 {
			out = append(out, hits[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

func ordinalIndex(n, count int) int {
	i := n - 1
	if n < 0 {
		i = count + n
	}
	if i < 0 || i >= count {
		return -1
	}
	return i
}
