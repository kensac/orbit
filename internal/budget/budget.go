// Package budget computes per-agent usage windows and limits.
package budget

import (
	"fmt"
	"time"
)

// AlertAt is the fraction of a configured limit at which /usage reports a
// warning. Reaching a limit blocks the next scheduled run.
const AlertAt = 0.8

// Window returns the current local calendar period and its exclusive end.
func Window(period string, now time.Time, loc *time.Location) (start, end time.Time, err error) {
	local := now.In(loc)
	y, m, d := local.Date()
	switch period {
	case "day":
		start = time.Date(y, m, d, 0, 0, 0, 0, loc)
		end = start.AddDate(0, 0, 1)
	case "week":
		// Periods start Monday, matching the ISO calendar week.
		back := (int(local.Weekday()) + 6) % 7
		start = time.Date(y, m, d-back, 0, 0, 0, 0, loc)
		end = start.AddDate(0, 0, 7)
	case "month":
		start = time.Date(y, m, 1, 0, 0, 0, 0, loc)
		end = start.AddDate(0, 1, 0)
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unknown budget period %q", period)
	}
	return start.UTC(), end.UTC(), nil
}

// Utilization is a fraction of limit used, or zero when the limit is absent.
func Utilization(used, limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	return used / limit
}

// Reached reports whether either configured limit has been reached.
func Reached(cost float64, runs int, maxCost float64, maxRuns int) bool {
	return maxCost > 0 && cost >= maxCost || maxRuns > 0 && runs >= maxRuns
}

// Alert reports whether a configured limit is at or above AlertAt.
func Alert(cost float64, runs int, maxCost float64, maxRuns int) bool {
	return maxCost > 0 && Utilization(cost, maxCost) >= AlertAt ||
		maxRuns > 0 && float64(runs)/float64(maxRuns) >= AlertAt
}
