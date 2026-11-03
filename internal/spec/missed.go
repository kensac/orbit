package spec

import "time"

// A schedule fire that starts too late is skipped as missed rather than run: a morning digest at three in the afternoon is noise, and by the time a fire is a whole period late its next one is due anyway.

// Bounds on a trigger's missed_grace. DefaultMissedGraceMax caps the default: past an hour, a fire is "hours late". MaxMissedGrace is a week, the longest period an agent is likely to have.
const (
	DefaultMissedGraceMax = time.Hour
	MinMissedGrace        = time.Minute
	MaxMissedGrace        = 7 * 24 * time.Hour
)

// MissedGraceFor is how late fire at of schedule trigger t may start: its missed_grace, or by default the time from at to the trigger's next fire, at most DefaultMissedGraceMax (an ended series gets the cap). loc is the agent's timezone.
func MissedGraceFor(t Trigger, loc *time.Location, at time.Time) time.Duration {
	if t.MissedGrace > 0 {
		return time.Duration(t.MissedGrace)
	}
	sched, err := TriggerSchedule(t, loc)
	if err != nil {
		return DefaultMissedGraceMax
	}
	next := sched.Next(at.In(loc))
	if next.IsZero() {
		return DefaultMissedGraceMax
	}
	return min(next.Sub(at), DefaultMissedGraceMax)
}
