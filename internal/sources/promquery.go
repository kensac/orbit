package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// The bounds of one prometheus_query call: what a model may ask a Prometheus for, so a steered run cannot turn it into a load test or a dump of the database.
const (
	// MaxPromQuery is the longest PromQL accepted.
	MaxPromQuery = 4000
	// MaxPromRange is the longest range query.
	MaxPromRange = 31 * 24 * time.Hour
	// MaxPromPoints is the most points per series a range query may ask for (Prometheus's own limit is 11000).
	MaxPromPoints = 1000
	// DefaultPromPoints sets a range query's step when it names none.
	DefaultPromPoints = 250
	// MaxPromSeries is sent as limit: Prometheus returns at most this many series, with a warning when it cut.
	MaxPromSeries = 500
	// MaxPromBytes is the largest answer; more is an error asking for a narrower query, never a cut JSON document.
	MaxPromBytes = 256 << 10
	// PromTimeout is the evaluation timeout Prometheus is asked to keep.
	PromTimeout = 20 * time.Second
)

// PromQuery is one query a model asked for: an instant query at Time, or a range query when Start is set.
type PromQuery struct {
	Expr  string
	Time  time.Time
	Start time.Time
	End   time.Time
	Step  time.Duration
}

// Range reports whether the query is a range query.
func (q PromQuery) Range() bool { return !q.Start.IsZero() }

// Check holds the query to the bounds above, filling a range query's default step.
func (q *PromQuery) Check() error {
	if strings.TrimSpace(q.Expr) == "" {
		return fmt.Errorf("query is required")
	}
	if len(q.Expr) > MaxPromQuery {
		return fmt.Errorf("query is longer than %d bytes", MaxPromQuery)
	}
	if !q.Range() {
		return nil
	}
	span := q.End.Sub(q.Start)
	if span <= 0 {
		return fmt.Errorf("start must be before end")
	}
	if span > MaxPromRange {
		return fmt.Errorf("a range query spans at most %s", MaxPromRange)
	}
	if q.Step == 0 {
		q.Step = max(time.Second, (span / DefaultPromPoints).Truncate(time.Second))
	}
	if q.Step < time.Second {
		return fmt.Errorf("step is at least 1s")
	}
	if int64(span/q.Step) > MaxPromPoints {
		return fmt.Errorf("start to end at step %s is more than %d points per series; use a step of at least %s", q.Step, MaxPromPoints,
			(span/MaxPromPoints).Round(time.Second)+time.Second)
	}
	return nil
}

// PromQuery runs q against s, a prometheus source of the agent, with its URL and credentials rendered as a fetch renders them. The model's query is a value, never rendered: a ${VAR} or {{template}} in it is sent as written.
func (f *Fetcher) PromQuery(ctx context.Context, s spec.Source, vars Vars, q PromQuery) ([]byte, error) {
	if s.Kind != "prometheus" {
		return nil, fmt.Errorf("source %s is not a prometheus source", s.Name)
	}
	if err := q.Check(); err != nil {
		return nil, err
	}
	used := map[string]bool{}
	base, err := f.render(s.URL, vars, used)
	if err != nil {
		return nil, err
	}
	v := url.Values{"query": {q.Expr}, "timeout": {PromTimeout.String()}, "limit": {strconv.Itoa(MaxPromSeries)}}
	path := "/api/v1/query"
	if q.Range() {
		path = "/api/v1/query_range"
		v.Set("start", q.Start.UTC().Format(time.RFC3339))
		v.Set("end", q.End.UTC().Format(time.RFC3339))
		v.Set("step", strconv.FormatFloat(q.Step.Seconds(), 'f', -1, 64))
	} else if !q.Time.IsZero() {
		v.Set("time", q.Time.UTC().Format(time.RFC3339))
	}
	u, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil {
		return nil, err
	}
	u.RawQuery = v.Encode()
	b, _, err := f.get(ctx, u.String(), http.Header{}, MaxPromBytes, used)
	if err != nil {
		if strings.Contains(err.Error(), "exceeds max_bytes") {
			return nil, fmt.Errorf("the answer is larger than %d KiB: aggregate (sum by, topk) or use a coarser step", MaxPromBytes>>10)
		}
		return nil, err
	}
	var r struct {
		Status   string          `json:"status"`
		Error    string          `json:"error"`
		Data     json.RawMessage `json:"data"`
		Warnings []string        `json:"warnings"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", r.Error)
	}
	if len(r.Warnings) == 0 {
		return r.Data, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(r.Data, &data); err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	w, _ := json.Marshal(r.Warnings)
	data["warnings"] = w
	return json.Marshal(data)
}

// ParsePromTime reads a time a model wrote: RFC 3339, now, or now-<duration> with s, m, h or d units, relative to now.
func ParsePromTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "now" {
		return now, nil
	}
	if rest, ok := strings.CutPrefix(s, "now-"); ok {
		d, err := ParsePromDuration(rest)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q: %w", s, err)
		}
		return now.Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not RFC 3339, now or now-<duration>", s)
	}
	return t, nil
}

// ParsePromDuration reads a duration with s, m, h or d units (30s, 5m, 1h30m, 2d).
func ParsePromDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("%q is not a duration", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a duration", s)
	}
	return d, nil
}
