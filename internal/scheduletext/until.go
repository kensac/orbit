package scheduletext

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	inRE    = regexp.MustCompile(`^in ([0-9]{1,3}) (hour|day|week)s?$`)
	isoRE   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}(?:[t ][0-9]{2}:[0-9]{2})?$`)
	monthRE = regexp.MustCompile(`^([a-z]+) ([0-9]{1,2})(?:st|nd|rd|th)?$`)
	months  = map[string]time.Month{"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3, "apr": 4, "april": 4,
		"may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7, "aug": 8, "august": 8, "sep": 9, "sept": 9, "september": 9,
		"oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12}
)

// ParseUntil reads when a pause ends, in loc, strictly after now: "tomorrow", a weekday ("monday", "next monday": the next such date after today), a date ("2026-11-09", "nov 9", "november 9th": the next one after today), any of those followed by "at" and a time as Parse reads one, a local "2026-11-09T08:00", or "in N hours|days|weeks". A date alone is its local midnight, the start of that day.
func ParseUntil(text string, now time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil || loc == time.Local {
		return time.Time{}, errors.New("an explicit timezone is required")
	}
	text = strings.Join(strings.Fields(strings.ToLower(text)), " ")
	text = strings.TrimPrefix(text, "until ")
	now = now.In(loc)
	if m := inRE.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n < 1 {
			return time.Time{}, errors.New("in N: N must be at least 1")
		}
		switch m[2] {
		case "hour":
			return now.Add(time.Duration(n) * time.Hour).Truncate(time.Minute), nil
		case "day":
			return now.AddDate(0, 0, n).Truncate(time.Minute), nil
		}
		return now.AddDate(0, 0, 7*n).Truncate(time.Minute), nil
	}
	if isoRE.MatchString(text) {
		layout := "2006-01-02"
		if len(text) > 10 {
			text = text[:10] + "T" + text[11:]
			layout = "2006-01-02T15:04"
		}
		t, err := time.ParseInLocation(layout, text, loc)
		if err != nil || t.Format(layout) != text {
			return time.Time{}, fmt.Errorf("%q is not a local date or time that exists", text)
		}
		return after(t, now)
	}
	day, clock, hasClock := strings.Cut(text, " at ")
	hour, minute := 0, 0
	if hasClock {
		var err error
		if hour, minute, err = parseClock(clock); err != nil {
			return time.Time{}, err
		}
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	var date time.Time
	switch d := strings.TrimPrefix(day, "next "); {
	case day == "today" && hasClock:
		date = today
	case day == "tomorrow":
		date = today.AddDate(0, 0, 1)
	case days[d] != "":
		date = today.AddDate(0, 0, 1)
		for date.Weekday().String() != strings.ToUpper(d[:1])+d[1:] {
			date = date.AddDate(0, 0, 1)
		}
	case isoRE.MatchString(day) && len(day) == 10:
		var err error
		if date, err = time.ParseInLocation("2006-01-02", day, loc); err != nil || date.Format("2006-01-02") != day {
			return time.Time{}, fmt.Errorf("%q is not a date", day)
		}
	default:
		m := monthRE.FindStringSubmatch(day)
		if m == nil || months[m[1]] == 0 {
			return time.Time{}, fmt.Errorf("unsupported time %q: want tomorrow, a weekday, a date (2026-11-09, nov 9), optionally at a time, or in N hours, days or weeks", text)
		}
		n, _ := strconv.Atoi(m[2])
		date = time.Date(now.Year(), months[m[1]], n, 0, 0, 0, 0, loc)
		if date.Month() != months[m[1]] {
			return time.Time{}, fmt.Errorf("%s has no day %d", months[m[1]], n)
		}
		if !date.After(today) {
			date = date.AddDate(1, 0, 0)
		}
	}
	t := time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, loc)
	if t.Hour() != hour || t.Minute() != minute {
		return time.Time{}, fmt.Errorf("%s does not exist in %s (a daylight saving change)", t.Format("2006-01-02 15:04"), loc)
	}
	return after(t, now)
}

func after(t, now time.Time) (time.Time, error) {
	if !t.After(now) {
		return time.Time{}, fmt.Errorf("%s is not in the future", t.Format("2006-01-02 15:04 MST"))
	}
	return t, nil
}
