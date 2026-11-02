package usage

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/testdb"
)

// cached is the endpoint's body as Claude Code cached it (~/.claude.json
// cachedUsageUtilization.utilization, 2026-11-01), trimmed of members that
// were null.
const cached = `{"five_hour": null, "seven_day": {"utilization": 23, "resets_at": "2026-11-08T14:00:00.166280+00:00",
 "limit_dollars": null, "used_dollars": null, "remaining_dollars": null, "locked_reason": null},
 "seven_day_opus": null, "seven_day_sonnet": {"utilization": 85.5, "resets_at": null},
 "extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null},
 "limits": [{"kind": "weekly_all", "group": "weekly", "percent": 23, "severity": "normal",
  "resets_at": "2026-11-08T14:00:00.166280+00:00", "scope": null, "is_active": true}]}`

func TestParsePollReadsWindowsAsFractions(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	got := ParsePoll(json.RawMessage(cached), at, at)
	if len(got) != 2 {
		t.Fatalf("windows = %+v, want seven_day and seven_day_sonnet (null members, extra_usage and limits are not windows)", got)
	}
	seven := got[0]
	if seven.Window != "seven_day" || *seven.Utilization != 0.23 || seven.ResetsAt == nil ||
		!seven.ResetsAt.Equal(time.Date(2026, 11, 8, 14, 0, 0, 166280000, time.UTC)) || seven.Expired ||
		seven.Source != SourcePoll || seven.Status != nil || seven.RunID != nil || !seven.ObservedAt.Equal(at) {
		t.Fatalf("seven_day = %+v", seven)
	}
	if got[1].ResetsAt != nil || *got[1].Utilization != 0.855 {
		t.Fatalf("seven_day_sonnet = %+v", got[1])
	}
	if late := ParsePoll(json.RawMessage(cached), at, at.AddDate(0, 0, 8)); !late[0].Expired {
		t.Fatal("a window past its reset is not expired")
	}
	if ParsePoll(json.RawMessage(`[1,2]`), at, at) != nil || ParsePoll(json.RawMessage(`{"x": {"utilization": "high"}}`), at, at) != nil {
		t.Fatal("a body of another shape produced windows")
	}
}

func TestMergeKeepsTheFreshestReadingPerWindow(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	streamed := []runner.RateLimitWindow{
		{Window: "five_hour", Status: "allowed", Utilization: f(0.5), ObservedAt: t0, RunID: "r1"},
		{Window: "seven_day", Status: "allowed", Utilization: f(0.1), ObservedAt: t0, RunID: "r1"},
	}
	polled := []Window{
		{Window: "five_hour", Utilization: f(0.6), ObservedAt: t0.Add(time.Minute), Source: SourcePoll},
		{Window: "seven_day", Utilization: f(0.2), ObservedAt: t0, Source: SourcePoll},
		{Window: "seven_day_opus", Utilization: f(0.9), ObservedAt: t0, Source: SourcePoll},
	}
	got := Merge(streamed, polled)
	if len(got) != 3 || got[0].Source != SourcePoll || *got[0].Utilization != 0.6 ||
		got[1].Source != SourceStream || *got[1].RunID != "r1" || *got[1].Status != "allowed" || got[2].Window != "seven_day_opus" {
		t.Fatalf("merged = %+v, want five_hour polled (newer), seven_day streamed (a tie), seven_day_opus polled", got)
	}
	if w := Over(got); w == nil || w.Window != "seven_day_opus" {
		t.Fatalf("over = %+v, want the fullest window above %v", w, DeferLowAbove)
	}
	got[2].Expired = true
	if w := Over(got); w != nil {
		t.Fatalf("over = %+v, want nothing: the only full window has reset", w)
	}
}

func TestPollerAsksWithTheTokenAndRecordsEveryAnswer(t *testing.T) {
	var auth, beta string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, beta = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, cached)
		} else {
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error"}}`)
		}
	}))
	defer srv.Close()
	s := testdb.New(t)
	cfg := &spec.Deployment{UsagePoll: &spec.UsagePoll{Every: spec.Duration(5 * time.Minute), TokenEnv: "TOK", URL: srv.URL}}
	env := map[string]string{"TOK": "sk-ant-oat-secret"}
	p := &Poller{Store: s, Client: srv.Client(), Env: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		Config: func() *spec.Deployment { return cfg }, Worker: "control", Version: "test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()

	if err := p.maybePoll(ctx, cfg.UsagePoll); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-ant-oat-secret" || beta != oauthBeta {
		t.Fatalf("headers: authorization %q, beta %q", auth, beta)
	}
	r, err := Read(ctx, s, time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	if err != nil || len(r.Windows) != 2 || r.Windows[0].Source != SourcePoll || r.LastPollOK == nil || r.LastPoll.ID != r.LastPollOK.ID {
		t.Fatalf("reading = %+v %v", r, err)
	}

	// Within half the interval of the last poll, nobody polls again.
	auth = ""
	if err := p.maybePoll(ctx, cfg.UsagePoll); err != nil || auth != "" {
		t.Fatalf("polled again at once: %v %q", err, auth)
	}

	status = http.StatusUnauthorized
	if _, err := s.Pool.Exec(ctx, `UPDATE usage_samples SET polled_at = polled_at - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if err := p.maybePoll(ctx, cfg.UsagePoll); err != nil {
		t.Fatal(err)
	}
	r, _ = Read(ctx, s, time.Now())
	if r.LastPoll.OK || *r.LastPoll.HTTPStatus != 401 || !strings.Contains(*r.LastPoll.Error, "HTTP 401") ||
		strings.Contains(*r.LastPoll.Error, "secret") || r.LastPollOK == nil || len(r.Windows) != 2 {
		t.Fatalf("after a refused poll = %+v: want it recorded without the token, the last good reading kept", r.LastPoll)
	}

	delete(env, "TOK")
	if _, _, err := p.Poll(ctx, cfg.UsagePoll); err == nil || !strings.Contains(err.Error(), "TOK is not set") {
		t.Fatalf("without a token = %v", err)
	}
}
