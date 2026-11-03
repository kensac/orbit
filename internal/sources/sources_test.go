package sources

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
	}, Vars{Now: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)})
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
		Paginate: &spec.Paginate{PageParam: "page", ItemsPath: "items"}}, Vars{Now: time.Now()})
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
		Paginate: &spec.Paginate{PageParam: "page", ItemsPath: "items", MaxPages: 10}}, Vars{Now: time.Now()})
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
		Query: map[string]string{"api_key": "${KEY}"}}, Vars{Now: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownTemplateVariable(t *testing.T) {
	f := &Fetcher{Client: http.DefaultClient, Env: noEnv}
	_, err := f.Fetch(context.Background(), spec.Source{Name: "x", Kind: "http", URL: "http://x/{{nope}}", MaxBytes: 1}, Vars{Now: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "{{nope}}") {
		t.Fatalf("err = %v", err)
	}
}

func TestVars(t *testing.T) {
	v := Vars{Now: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)}
	for in, want := range map[string]string{
		"{{prev_month_start}}":        "2026-02-01",
		"{{prev_month_end}}":          "2026-02-28",
		"{{days_ago_15}}":             "2026-02-28",
		"{{days_ago 15}}":             "2026-02-28",
		"{{days_ago 0}}":              "2026-03-15",
		"{{ days_ago 400 }}":          "2025-02-08",
		"{{months_ago_start 0}}":      "2026-03-01",
		"{{months_ago_end 0}}":        "2026-03-31",
		"{{months_ago_start 1}}":      "2026-02-01",
		"{{months_ago_end 1}}":        "2026-02-28",
		"{{months_ago_start 3}}":      "2025-12-01",
		"{{months_ago_end 3}}":        "2025-12-31",
		"{{months_ago_end 13}}":       "2025-02-28",
		"{{months_ago_end 25}}":       "2024-02-29",
		"a={{today}}&b={{now}}":       "a=2026-03-15&b=2026-03-15T00:00:00Z",
		"{{months_ago_start 120}}":    "2016-03-01",
		"from {{months_ago_start 2}}": "from 2026-01-01",
	} {
		if got, err := v.Expand(in); err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"{{nope}}", "{{days_ago_0}}", "{{days_ago_367}}", "{{days_ago_015}}", "{{days_ago -1}}",
		"{{days_ago 3661}}", "{{months_ago_start}}", "{{months_ago_end 121}}", "{{months_ago_start x}}", "{{months_ago_start 01}}",
		"{{today 1}}", "{{months_ago_start 1 2}}", "{{today", "{{ }}"} {
		if got, err := v.Expand(in); err == nil {
			t.Errorf("Expand(%q) = %q, want an error", in, got)
		}
	}
}

// A month's end is the month's own last day in the agent's zone, not UTC's.
func TestMonthsAgoInTheAgentsZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	v := Vars{Now: time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC).In(ny)} // Oct 31, 21:00 in New York
	got, err := v.Expand("{{months_ago_start 0}} {{months_ago_end 0}} {{months_ago_start 2}}")
	if err != nil || got != "2026-10-01 2026-10-31 2026-08-01" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// A message run takes its trigger's query values over the source's own;
// every other trigger keeps the source's.
func TestByTriggerOverridesTheQuery(t *testing.T) {
	var got []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Query())
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	s := spec.Source{Name: "x", Kind: "http", URL: srv.URL, MaxBytes: 1 << 20,
		Query: map[string]string{"start_date": "{{prev_month_start}}", "end_date": "{{today}}"},
		ByTrigger: map[string]spec.SourceOverride{"message": {Query: map[string]string{
			"start_date": "{{months_ago_start 3}}", "account": "all"}}}}
	f := &Fetcher{Client: srv.Client(), Env: noEnv}
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	for _, trig := range []string{"message", "schedule"} {
		if _, err := f.Fetch(context.Background(), s, Vars{Now: now, Trigger: trig}); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 {
		t.Fatalf("requests = %v", got)
	}
	if m := got[0]; m.Get("start_date") != "2026-08-01" || m.Get("end_date") != "2026-11-02" || m.Get("account") != "all" {
		t.Fatalf("message run's query = %v", m)
	}
	if sc := got[1]; sc.Get("start_date") != "2026-10-01" || sc.Get("end_date") != "2026-11-02" || sc.Has("account") {
		t.Fatalf("scheduled run's query = %v", sc)
	}
	if s.Query["start_date"] != "{{prev_month_start}}" {
		t.Fatalf("the override changed the source: %v", s.Query)
	}
}

func TestARestrictedFetcherReadsNoFile(t *testing.T) {
	s := spec.Source{Name: "env", Kind: "file", Path: "/proc/self/environ", MaxBytes: 1 << 20}
	if _, err := (&Fetcher{Env: noEnv, Restricted: true}).Fetch(context.Background(), s, Vars{}); err == nil || !strings.Contains(err.Error(), "only for an agent in the configuration files") {
		t.Fatalf("err = %v", err)
	}
}

func TestACredentialGoesOnlyUnderItsPrefix(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+" "+r.Header.Get("X-Key"))
		if r.URL.Path == "/api/hop" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	env := func(k string) (string, bool) {
		return map[string]string{"KEY": "secret", "ORBIT_API_TOKEN": "op"}[k], k == "KEY" || k == "ORBIT_API_TOKEN"
	}
	creds := spec.Credentials{"KEY": {URLPrefix: srv.URL + "/api/"}}
	vars := Vars{Now: time.Now()}
	fetch := func(f *Fetcher, url string, headers map[string]string) error {
		_, err := f.Fetch(context.Background(), spec.Source{Name: "s", Kind: "http", URL: url, Headers: headers, MaxBytes: 1 << 20}, vars)
		return err
	}
	f := &Fetcher{Client: srv.Client(), Env: env, Credentials: creds, Restricted: true}
	if err := fetch(f, srv.URL+"/api/v1", map[string]string{"X-Key": "${KEY}"}); err != nil {
		t.Fatal(err)
	}
	if err := fetch(f, srv.URL+"/other", map[string]string{"X-Key": "${KEY}"}); err == nil || !strings.Contains(err.Error(), "may be sent only to") {
		t.Fatalf("off-prefix err = %v", err)
	}
	if err := fetch(f, srv.URL+"/api/hop", map[string]string{"X-Key": "${KEY}"}); err == nil || !strings.Contains(err.Error(), "redirect refused") {
		t.Fatalf("redirect err = %v", err)
	}
	if err := fetch(f, srv.URL+"/api/x", map[string]string{"X-Key": "${ORBIT_API_TOKEN}"}); err == nil || !strings.Contains(err.Error(), "not in orbit.yaml's credentials") {
		t.Fatalf("undeclared err = %v", err)
	}
	if want := []string{"/api/v1 secret"}; !slices.Equal(got[:1], want) || len(got) != 2 || got[1] != "/api/hop secret" {
		t.Fatalf("requests = %q", got)
	}
	// A file agent's fetcher still sends any variable that is not a credential, and a credential only under its prefix.
	f.Restricted = false
	if err := fetch(f, srv.URL+"/api/x", map[string]string{"X-Key": "${ORBIT_API_TOKEN}"}); err != nil {
		t.Fatal(err)
	}
	if err := fetch(f, srv.URL+"/other", map[string]string{"X-Key": "${KEY}"}); err == nil {
		t.Fatal("a file agent sent a credential off its prefix")
	}
}
