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

func noEnv(string) (string, bool) { return "", false }

func TestPaginationMergesEveryPage(t *testing.T) {
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		p := r.URL.Query().Get("page")
		fmt.Fprintf(w, `{"items":[{"p":%s},{"p":%s}],"meta":{"pages":3}}`, p, p)
	}))
	defer srv.Close()
	f := &Fetcher{Client: srv.Client(), Env: noEnv}
	got, err := f.Fetch(context.Background(), spec.Source{
		Name: "x", Kind: "http", URL: srv.URL, MaxBytes: 1 << 20,
		Query:    map[string]string{"start": "{{prev_month_start}}"},
		Paginate: &spec.Paginate{PageParam: "page", ItemsPath: "items", TotalPagesPath: "meta.pages"},
	}, Vars(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Content) != `[{"p":1},{"p":1},{"p":2},{"p":2},{"p":3},{"p":3}]` {
		t.Fatalf("content = %s", got.Content)
	}
	if len(queries) != 3 || queries[0].Get("start") != "2026-10-01" {
		t.Fatalf("queries = %v", queries)
	}
}

func TestPaginationStopsOnAnEmptyPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{"items":[]}`)
			return
		}
		fmt.Fprint(w, `{"items":[1]}`)
	}))
	defer srv.Close()
	f := &Fetcher{Client: srv.Client(), Env: noEnv}
	got, err := f.Fetch(context.Background(), spec.Source{Name: "x", Kind: "http", URL: srv.URL, MaxBytes: 1 << 20,
		Paginate: &spec.Paginate{PageParam: "page", ItemsPath: "items"}}, Vars(time.Now()))
	if err != nil || string(got.Content) != `[1]` {
		t.Fatalf("got %s %v", got.Content, err)
	}
}

func TestMaxBytesIsEnforcedAcrossPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"items":["%s"]}`, strings.Repeat("x", 600))
	}))
	defer srv.Close()
	f := &Fetcher{Client: srv.Client(), Env: noEnv}
	_, err := f.Fetch(context.Background(), spec.Source{Name: "x", Kind: "http", URL: srv.URL, MaxBytes: 1000,
		Paginate: &spec.Paginate{PageParam: "page", ItemsPath: "items", MaxPages: 10}}, Vars(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("err = %v, want max_bytes", err)
	}
}

func TestFailedRequestNamesTheStatusNotTheSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	f := &Fetcher{Client: srv.Client(), Env: func(string) (string, bool) { return "s3cret", true }}
	_, err := f.Fetch(context.Background(), spec.Source{Name: "x", Kind: "http", URL: srv.URL, MaxBytes: 1 << 20,
		Query: map[string]string{"api_key": "${KEY}"}}, Vars(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownTemplateVariable(t *testing.T) {
	f := &Fetcher{Client: http.DefaultClient, Env: noEnv}
	_, err := f.Fetch(context.Background(), spec.Source{Name: "x", Kind: "http", URL: "http://x/{{nope}}", MaxBytes: 1}, Vars(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "{{nope}}") {
		t.Fatalf("err = %v", err)
	}
}

func TestVars(t *testing.T) {
	v := Vars(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if v["prev_month_start"] != "2026-02-01" || v["prev_month_end"] != "2026-02-28" || v["days_ago_15"] != "2026-02-28" {
		t.Fatalf("vars = %v %v %v", v["prev_month_start"], v["prev_month_end"], v["days_ago_15"])
	}
}
