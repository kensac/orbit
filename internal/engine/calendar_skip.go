package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/connectors"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

const (
	calendarCallTimeout = 30 * time.Second
	calendarResultMax   = 1 << 20
	calendarEventsMax   = 1000
)

// calendarSkip queries the configured read-only connector tool before the
// first scheduled attempt. The result stays in orbit and is reduced to a
// boolean overlap decision.
func (e *Engine) calendarSkip(ctx context.Context, snap *Snapshot, a *spec.Agent, c *store.ClaimedRun) (*gateSkip, error) {
	q := a.CalendarSkip
	if q == nil || c.Trigger != "schedule" || c.Attempt != 1 {
		return nil, nil
	}
	if c.ScheduledFor == nil {
		return nil, errors.New("scheduled run has no fire time")
	}
	d := snap.Config.Deployment
	cn, ok := d.Connectors[q.Connector]
	if !ok {
		return nil, fmt.Errorf("connector %q is unavailable", q.Connector)
	}
	auth, err := e.connectorAuth(d, q.Connector, cn)
	if err != nil {
		return nil, fmt.Errorf("connector %s authentication: %w", q.Connector, err)
	}
	cl := connectors.New(q.Connector, cn, auth, e.Client)
	defer cl.Close()

	callCtx, cancel := context.WithTimeout(ctx, calendarCallTimeout)
	defer cancel()
	tools, err := cl.ListTools(callCtx)
	if err != nil {
		return nil, fmt.Errorf("connector %s: list tools: %w", q.Connector, err)
	}
	toolFound := false
	for _, tool := range tools {
		if tool.Name != q.Tool {
			continue
		}
		if !tool.ReadOnly {
			return nil, fmt.Errorf("connector %s tool %s is not marked read-only", q.Connector, q.Tool)
		}
		if slices.Contains(cn.WriteTools, q.Tool) {
			return nil, fmt.Errorf("connector %s tool %s is configured as a write tool", q.Connector, q.Tool)
		}
		toolFound = true
		break
	}
	if !toolFound {
		return nil, fmt.Errorf("connector %s does not provide calendar tool %s", q.Connector, q.Tool)
	}

	fire := *c.ScheduledFor
	windowStart := fire.Add(-time.Duration(q.WindowBefore))
	windowEnd := fire.Add(time.Duration(q.WindowAfter))
	args, err := q.RenderArguments(windowStart, windowEnd)
	if err != nil {
		return nil, fmt.Errorf("render calendar query: %w", err)
	}
	result, err := cl.Call(callCtx, q.Tool, args)
	if err != nil {
		return nil, fmt.Errorf("connector %s tool %s: %w", q.Connector, q.Tool, err)
	}
	if result.IsError {
		return nil, fmt.Errorf("connector %s tool %s returned an error", q.Connector, q.Tool)
	}
	if len(result.Raw) > calendarResultMax {
		return nil, fmt.Errorf("connector %s tool %s result exceeds %d bytes", q.Connector, q.Tool, calendarResultMax)
	}
	value, err := calendarResultValue(result.Raw)
	if err != nil {
		return nil, fmt.Errorf("connector %s tool %s result: %w", q.Connector, q.Tool, err)
	}
	eventsPath := q.EventsPath
	if eventsPath == "" {
		eventsPath = "items"
	}
	eventsValue, ok := calendarPath(value, eventsPath)
	if !ok {
		return nil, fmt.Errorf("connector %s tool %s result has no events at %q", q.Connector, q.Tool, eventsPath)
	}
	events, ok := eventsValue.([]any)
	if !ok {
		return nil, fmt.Errorf("connector %s tool %s result events at %q are not a list", q.Connector, q.Tool, eventsPath)
	}
	if len(events) > calendarEventsMax {
		return nil, fmt.Errorf("connector %s tool %s returned more than %d events", q.Connector, q.Tool, calendarEventsMax)
	}
	for _, event := range events {
		busy, err := calendarEventOverlaps(event, q, windowStart, windowEnd, a.Location())
		if err != nil {
			return nil, fmt.Errorf("connector %s tool %s returned an invalid event", q.Connector, q.Tool)
		}
		if busy {
			return &gateSkip{gate: store.GateCalendarBusy, summary: fmt.Sprintf(
				"a calendar event overlaps the scheduled window %s through %s",
				windowStart.In(a.Location()).Format("2006-01-02 15:04 MST"),
				windowEnd.In(a.Location()).Format("2006-01-02 15:04 MST"))}, nil
		}
	}
	return nil, nil
}

func calendarResultValue(raw json.RawMessage) (any, error) {
	var envelope struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, errors.New("is not valid MCP JSON")
	}
	if envelope.IsError {
		return nil, errors.New("reported an error")
	}
	if len(envelope.StructuredContent) > 0 && string(envelope.StructuredContent) != "null" {
		var value any
		if err := json.Unmarshal(envelope.StructuredContent, &value); err != nil {
			return nil, errors.New("structured content is not valid JSON")
		}
		return value, nil
	}
	for _, block := range envelope.Content {
		if block.Type != "text" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(block.Text), &value); err == nil {
			return value, nil
		}
	}
	return nil, errors.New("has no JSON event data")
}

func calendarEventOverlaps(value any, q *spec.CalendarSkip, from, to time.Time, loc *time.Location) (bool, error) {
	event, ok := value.(map[string]any)
	if !ok {
		return false, errors.New("event is not an object")
	}
	if status, _ := event["status"].(string); status == "cancelled" {
		return false, nil
	}
	if transparency, _ := event["transparency"].(string); transparency == "transparent" {
		return false, nil
	}
	startPath, endPath := q.StartPath, q.EndPath
	if startPath == "" {
		startPath = "start"
	}
	if endPath == "" {
		endPath = "end"
	}
	startValue, ok := calendarPath(event, startPath)
	if !ok {
		return false, errors.New("event has no start")
	}
	endValue, ok := calendarPath(event, endPath)
	if !ok {
		return false, errors.New("event has no end")
	}
	start, err := calendarTime(startValue, loc)
	if err != nil {
		return false, err
	}
	end, err := calendarTime(endValue, loc)
	if err != nil || !end.After(start) {
		return false, errors.New("event has invalid bounds")
	}
	return start.Before(to) && end.After(from), nil
}

func calendarTime(value any, loc *time.Location) (time.Time, error) {
	switch v := value.(type) {
	case map[string]any:
		if s, ok := v["dateTime"].(string); ok {
			return parseCalendarDateTime(s, loc)
		}
		if s, ok := v["date"].(string); ok {
			return time.ParseInLocation(time.DateOnly, s, loc)
		}
	case string:
		return parseCalendarDateTime(v, loc)
	}
	return time.Time{}, errors.New("event bound is not a time")
}

func parseCalendarDateTime(value string, loc *time.Location) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return at, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04"} {
		if at, err := time.ParseInLocation(layout, value, loc); err == nil {
			return at, nil
		}
	}
	return time.Time{}, errors.New("event time is not RFC3339 or a local date-time")
}

func calendarPath(value any, path string) (any, bool) {
	if path == "" {
		return value, true
	}
	current := value
	for _, part := range strings.Split(path, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}
