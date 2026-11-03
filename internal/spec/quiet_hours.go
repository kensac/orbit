package spec

import (
	"errors"
	"fmt"
	"time"
)

// QuietHours is a local wall-clock interval when scheduled runs must not
// start. If End is earlier than Start, the interval crosses midnight.
type QuietHours struct {
	Start string `yaml:"start" json:"start"`
	End   string `yaml:"end" json:"end"`
}

// Validate checks that quiet hours define a nonempty interval of local time.
func (q QuietHours) Validate() error {
	start, err := quietMinute(q.Start)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	end, err := quietMinute(q.End)
	if err != nil {
		return fmt.Errorf("end: %w", err)
	}
	if start == end {
		return errors.New("start and end must differ")
	}
	return nil
}

// Contains reports whether t falls in this interval in loc. The start is
// inclusive and the end exclusive; hours are wall-clock across DST changes.
func (q QuietHours) Contains(t time.Time, loc *time.Location) bool {
	start, err := quietMinute(q.Start)
	if err != nil {
		return false
	}
	end, err := quietMinute(q.End)
	if err != nil || start == end {
		return false
	}
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	minute := local.Hour()*60 + local.Minute()
	if start < end {
		return minute >= start && minute < end
	}
	return minute >= start || minute < end
}

func quietMinute(value string) (int, error) {
	if len(value) != 5 || value[2] != ':' {
		return 0, errors.New("must be local time in HH:MM format")
	}
	t, err := time.Parse("15:04", value)
	if err != nil || t.Format("15:04") != value {
		return 0, errors.New("must be local time in HH:MM format")
	}
	return t.Hour()*60 + t.Minute(), nil
}
