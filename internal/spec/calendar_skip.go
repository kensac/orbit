package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	calendarWindowStart = "{{window_start}}"
	calendarWindowEnd   = "{{window_end}}"
	calendarArgsMax     = 32 << 10
	calendarWindowMax   = 24 * time.Hour
)

var calendarPathRE = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)

// CalendarSkip checks a connector's read-only event listing before an
// unattended scheduled run. Event data is used by orbit and never shown to
// the agent.
type CalendarSkip struct {
	Connector    string         `yaml:"connector" json:"connector"`
	Tool         string         `yaml:"tool" json:"tool"`
	WindowBefore Duration       `yaml:"window_before,omitempty" json:"window_before,omitempty"`
	WindowAfter  Duration       `yaml:"window_after" json:"window_after"`
	Arguments    map[string]any `yaml:"arguments" json:"arguments"`
	EventsPath   string         `yaml:"events_path,omitempty" json:"events_path,omitempty"`
	StartPath    string         `yaml:"start_path,omitempty" json:"start_path,omitempty"`
	EndPath      string         `yaml:"end_path,omitempty" json:"end_path,omitempty"`
}

// Validate checks the calendar query, event paths and scheduled window.
func (q CalendarSkip) Validate() error {
	if !ConnectorNameRE.MatchString(q.Connector) {
		return fmt.Errorf("connector must match %s", ConnectorNameRE)
	}
	if !ConnectorToolRE.MatchString(q.Tool) {
		return fmt.Errorf("tool must match %s", ConnectorToolRE)
	}
	before, after := time.Duration(q.WindowBefore), time.Duration(q.WindowAfter)
	if before < 0 || before > calendarWindowMax {
		return fmt.Errorf("window_before must be between 0 and %s", calendarWindowMax)
	}
	if after <= 0 || after > calendarWindowMax || before+after > calendarWindowMax {
		return fmt.Errorf("window_after must be positive and window_before plus window_after must not exceed %s", calendarWindowMax)
	}
	if len(q.Arguments) == 0 {
		return errors.New("arguments must include {{window_start}} and {{window_end}}")
	}
	b, err := json.Marshal(q.Arguments)
	if err != nil {
		return fmt.Errorf("arguments must be JSON-compatible: %w", err)
	}
	if len(b) > calendarArgsMax {
		return fmt.Errorf("arguments exceed %d bytes", calendarArgsMax)
	}
	var args any
	if err := json.Unmarshal(b, &args); err != nil {
		return fmt.Errorf("arguments must be JSON-compatible: %w", err)
	}
	start, end, err := calendarPlaceholders(args)
	if err != nil {
		return err
	}
	if !start || !end {
		return errors.New("arguments must include {{window_start}} and {{window_end}}")
	}
	for field, value := range map[string]string{"events_path": q.EventsPath, "start_path": q.StartPath, "end_path": q.EndPath} {
		if value != "" && !calendarPathRE.MatchString(value) {
			return fmt.Errorf("%s must be a dotted JSON field path", field)
		}
	}
	return nil
}

func calendarPlaceholders(value any) (bool, bool, error) {
	start, end := false, false
	switch v := value.(type) {
	case map[string]any:
		for _, child := range v {
			a, b, err := calendarPlaceholders(child)
			if err != nil {
				return false, false, err
			}
			start, end = start || a, end || b
		}
	case []any:
		for _, child := range v {
			a, b, err := calendarPlaceholders(child)
			if err != nil {
				return false, false, err
			}
			start, end = start || a, end || b
		}
	case string:
		if strings.Contains(v, "${") {
			return false, false, errors.New("arguments cannot contain environment references; connector credentials are added by orbit")
		}
		if strings.Contains(v, "{{") {
			switch v {
			case calendarWindowStart:
				start = true
			case calendarWindowEnd:
				end = true
			default:
				return false, false, fmt.Errorf("arguments contain unsupported template %q", v)
			}
		}
	}
	return start, end, nil
}

// RenderArguments returns a copy with the scheduled window rendered as
// RFC3339 UTC strings.
func (q CalendarSkip) RenderArguments(start, end time.Time) (json.RawMessage, error) {
	b, err := json.Marshal(q.Arguments)
	if err != nil {
		return nil, fmt.Errorf("marshal arguments: %w", err)
	}
	var value any
	if err := json.Unmarshal(b, &value); err != nil {
		return nil, fmt.Errorf("decode arguments: %w", err)
	}
	value = renderCalendarWindow(value, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	b, err = json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("render arguments: %w", err)
	}
	return json.RawMessage(bytes.Clone(b)), nil
}

func renderCalendarWindow(value any, start, end string) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			out[key] = renderCalendarWindow(child, start, end)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = renderCalendarWindow(child, start, end)
		}
		return out
	case string:
		switch v {
		case calendarWindowStart:
			return start
		case calendarWindowEnd:
			return end
		}
	}
	return value
}
