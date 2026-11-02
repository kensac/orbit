// Package scheduletext compiles a strict set of recurring English phrases into existing triggers.
package scheduletext

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

var clockRE = regexp.MustCompile(`^(?:([0-9]{1,2})(?::([0-9]{2}))?|([0-9]{1,2})(?::([0-9]{2}))?\s?(am|pm))$`)
var monthlyRE = regexp.MustCompile(`^(?:the ([0-9]{1,2})(st|nd|rd|th)(?: of every month)?|every month on the ([0-9]{1,2})(st|nd|rd|th))$`)
var days = map[string]string{"monday": "MO", "tuesday": "TU", "wednesday": "WE", "thursday": "TH", "friday": "FR", "saturday": "SA", "sunday": "SU"}

// Parse accepts daily, every weekday, every named weekday, every other named weekday, and an ordinal date of every month, each followed by "at" and an explicit time. loc is required; anchor selects the first eligible local date; a fortnightly rule starts at its first valid occurrence strictly after anchor. The trigger must run in loc as the agent's timezone.
func Parse(text string, anchor time.Time, loc *time.Location) (spec.Trigger, error) {
	var trigger spec.Trigger
	if loc == nil || loc == time.Local {
		return trigger, errors.New("an explicit timezone is required")
	}
	if anchor.IsZero() {
		return trigger, errors.New("a nonzero anchor is required")
	}
	text = strings.Join(strings.Fields(strings.ToLower(text)), " ")
	recurrence, clock, ok := strings.Cut(text, " at ")
	if !ok {
		return trigger, errors.New("want a supported recurrence followed by at HH:MM or at 9am")
	}
	hour, minute, err := parseClock(clock)
	if err != nil {
		return trigger, err
	}
	rule := ""
	switch recurrence {
	case "daily", "every day":
		rule = "FREQ=DAILY"
	case "every weekday":
		rule = "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR"
	default:
		if day, ok := strings.CutPrefix(recurrence, "every other "); ok && days[day] != "" {
			rule = "FREQ=WEEKLY;INTERVAL=2;BYDAY=" + days[day]
		} else if day, ok := strings.CutPrefix(recurrence, "every "); ok && days[day] != "" {
			rule = "FREQ=WEEKLY;BYDAY=" + days[day]
		} else if m := monthlyRE.FindStringSubmatch(recurrence); m != nil {
			digits, suffix := m[1], m[2]
			if digits == "" {
				digits, suffix = m[3], m[4]
			}
			date, _ := strconv.Atoi(digits)
			if date < 1 || date > 31 || suffix != ordinal(date) || digits != strconv.Itoa(date) {
				return trigger, errors.New("monthly date must be a valid ordinal from 1st through 31st")
			}
			rule = fmt.Sprintf("FREQ=MONTHLY;BYMONTHDAY=%d", date)
		}
	}
	if rule == "" {
		return trigger, fmt.Errorf("unsupported recurrence %q", recurrence)
	}
	// Midnight anchors the local date without letting the requested clock time normalize through a DST gap.
	start := anchor.In(loc).Format(time.DateOnly) + "T00:00:00"
	local, err := time.ParseInLocation("2006-01-02T15:04:05", start, loc)
	if err != nil || local.Format("2006-01-02T15:04:05") != start {
		return trigger, errors.New("anchor date has no local midnight; choose another anchor date")
	}
	trigger = spec.Trigger{RRule: fmt.Sprintf("%s;BYHOUR=%d;BYMINUTE=%d;BYSECOND=0", rule, hour, minute), Start: start}
	if strings.Contains(rule, ";INTERVAL=2;") {
		weekly := trigger
		weekly.RRule = strings.Replace(trigger.RRule, ";INTERVAL=2;", ";", 1)
		schedule, err := spec.TriggerSchedule(weekly, loc)
		if err != nil {
			return spec.Trigger{}, fmt.Errorf("weekly anchor: %w", err)
		}
		first := schedule.Next(anchor)
		if first.IsZero() {
			return spec.Trigger{}, errors.New("no upcoming occurrence for fortnightly anchor")
		}
		trigger.Start = first.In(loc).Format(time.DateOnly) + "T00:00:00"
		midnight, err := time.ParseInLocation("2006-01-02T15:04:05", trigger.Start, loc)
		if err != nil || midnight.Format("2006-01-02T15:04:05") != trigger.Start {
			return spec.Trigger{}, errors.New("first occurrence date has no local midnight; choose another anchor")
		}
	}
	if _, err := spec.TriggerSchedule(trigger, loc); err != nil {
		return spec.Trigger{}, fmt.Errorf("compiled trigger: %w", err)
	}
	return trigger, nil
}

// parseClock reads a time of day: without am or pm it is the 24-hour clock ("8" and "8:00" are 08:00, "20" is 20:00), which the caller echoes back for confirmation.
func parseClock(s string) (int, int, error) {
	m := clockRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, errors.New("time must be on the 24-hour clock (8, 08:00, 20:30) or explicitly am/pm, such as 9am or 9:30pm")
	}
	hour, minute := 0, 0
	if m[1] != "" {
		hour, _ = strconv.Atoi(m[1])
		minute, _ = strconv.Atoi(m[2])
		if hour > 23 || minute > 59 {
			return 0, 0, errors.New("time is outside 00:00 through 23:59")
		}
	} else {
		hour, _ = strconv.Atoi(m[3])
		if m[4] != "" {
			minute, _ = strconv.Atoi(m[4])
		}
		if hour < 1 || hour > 12 || minute > 59 {
			return 0, 0, errors.New("am/pm time needs hour 1 through 12 and minute 00 through 59")
		}
		hour %= 12
		if m[5] == "pm" {
			hour += 12
		}
	}
	return hour, minute, nil
}
func ordinal(day int) string {
	if day%100 >= 11 && day%100 <= 13 {
		return "th"
	}
	switch day % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	}
	return "th"
}
