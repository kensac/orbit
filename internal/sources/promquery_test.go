package sources

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

func TestPromQueryIsReadOnlyAndBounded(t *testing.T) {
	var paths []string
	var last url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		last = r.URL.Query()
		switch r.URL.Query().Get("query") {
		case "big":
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"x":"%s"}]}}`, strings.Repeat("a", MaxPromBytes))
		case "warn":
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]},"warnings":["results truncated due to limit"]}`)
		case "bad(":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
		default:
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"orbit"},"value":[1,"1"]}]}}`)
		}
	}))
	defer srv.Close()
	f := &Fetcher{Client: srv.Client(), Env: func(n string) (string, bool) { return "secret", n == "SECRET" }}
	src := spec.Source{Name: "prom", Kind: "prometheus", URL: srv.URL + "/", Expr: "up"}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	b, err := f.PromQuery(ctx, src, Vars{Now: now}, PromQuery{Expr: `up{job="${SECRET}"}`, Time: now})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"resultType":"vector","result":[{"metric":{"job":"orbit"},"value":[1,"1"]}]}` {
		t.Fatalf("answer = %s", b)
	}
	if last.Get("query") != `up{job="${SECRET}"}` || last.Get("time") != "2026-11-02T12:00:00Z" || last.Get("limit") != "500" || last.Get("timeout") != "20s" {
		t.Fatalf("the model's query is sent as written, never rendered: %v", last)
	}
	if _, err := f.PromQuery(ctx, src, Vars{Now: now}, PromQuery{Expr: "rate(x[5m])", Start: now.Add(-6 * time.Hour), End: now}); err != nil {
		t.Fatal(err)
	}
	if paths[1] != "GET /api/v1/query_range" || last.Get("step") != "86" || last.Get("start") != "2026-11-02T06:00:00Z" {
		t.Fatalf("range: %v %v", paths, last)
	}
	b, err = f.PromQuery(ctx, src, Vars{Now: now}, PromQuery{Expr: "warn"})
	if err != nil || !strings.Contains(string(b), `"warnings":["results truncated due to limit"]`) {
		t.Fatalf("warnings = %s %v", b, err)
	}
	n := len(paths)
	for q, want := range map[*PromQuery]string{
		{Expr: ""}: "query is required",
		{Expr: strings.Repeat("x", MaxPromQuery+1)}:                                    "longer than",
		{Expr: "x", Start: now, End: now.Add(-time.Hour)}:                              "start must be before end",
		{Expr: "x", Start: now.Add(-32 * 24 * time.Hour), End: now}:                    "spans at most",
		{Expr: "x", Start: now.Add(-24 * time.Hour), End: now, Step: 15 * time.Second}: "more than 1000 points per series; use a step of at least 1m27s",
		{Expr: "x", Start: now.Add(-time.Hour), End: now, Step: time.Millisecond}:      "step is at least 1s",
	} {
		if _, err := f.PromQuery(ctx, src, Vars{Now: now}, *q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", q.Expr[:min(len(q.Expr), 10)], err, want)
		}
	}
	if len(paths) != n {
		t.Fatal("a refused query reached Prometheus")
	}
	if _, err := f.PromQuery(ctx, src, Vars{Now: now}, PromQuery{Expr: "big"}); err == nil || !strings.Contains(err.Error(), "larger than 256 KiB") {
		t.Fatalf("big = %v", err)
	}
	if _, err := f.PromQuery(ctx, src, Vars{Now: now}, PromQuery{Expr: "bad("}); err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("bad = %v", err)
	}
	if _, err := f.PromQuery(ctx, spec.Source{Name: "h", Kind: "http", URL: srv.URL}, Vars{Now: now}, PromQuery{Expr: "up"}); err == nil {
		t.Fatal("an http source is not a Prometheus")
	}
}

func TestParsePromTime(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]string{"": "2026-11-02T12:00:00Z", "now": "2026-11-02T12:00:00Z", "now-90m": "2026-11-02T10:30:00Z",
		"now-2d": "2026-10-31T12:00:00Z", "2026-11-01T09:00:00-04:00": "2026-11-01T13:00:00Z"} {
		got, err := ParsePromTime(in, now)
		if err != nil || got.UTC().Format(time.RFC3339) != want {
			t.Errorf("%q = %v %v, want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"yesterday", "now-x", "now--1h", "now+1h"} {
		if _, err := ParsePromTime(in, now); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
}
