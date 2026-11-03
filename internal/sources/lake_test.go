package sources

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/transforms"
)

func TestLakeReadsSortedBoundedObjectsWithoutExposingCredentials(t *testing.T) {
	files := map[string]string{
		"positions/date=2026-11-03/a.json.gz": `{"hex":"A","count":2}` + "\n" + `{"hex":"A","count":3}` + "\n",
		"positions/date=2026-11-03/b.json.gz": `{"hex":"B","count":4}` + "\n",
	}
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=lake-access/") || strings.Contains(r.Header.Get("Authorization"), "lake-secret") {
			t.Errorf("authorization header leaked or was not signed: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Amz-Security-Token") != "lake-session" || r.Header.Get("X-Amz-Date") != "20261103T120000Z" {
			t.Errorf("signed headers = %v", r.Header)
		}
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/adsb" {
			if r.URL.Query().Get("prefix") != "positions/date=2026-11-03/" || r.URL.Query().Get("max-keys") != "101" {
				t.Errorf("list query = %v", r.URL.Query())
			}
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>positions/date=2026-11-03/b.json.gz</Key></Contents><Contents><Key>positions/date=2026-11-03/a.json.gz</Key></Contents></ListBucketResult>`)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/adsb/")
		content, ok := files[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(content))
		_ = zw.Close()
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	env := func(name string) (string, bool) {
		v, ok := map[string]string{"LAKE_ID": "lake-access", "LAKE_SECRET": "lake-secret", "LAKE_SESSION": "lake-session"}[name]
		return v, ok
	}
	f := &Fetcher{Client: srv.Client(), Env: env, Restricted: true, Now: func() time.Time { return now }, Credentials: spec.Credentials{
		"LAKE_ID": {URLPrefix: srv.URL + "/"}, "LAKE_SECRET": {URLPrefix: srv.URL + "/"}, "LAKE_SESSION": {URLPrefix: srv.URL + "/"},
	}}
	got, err := f.Fetch(context.Background(), spec.Source{Name: "aircraft", Kind: "lake", URL: srv.URL, Bucket: "adsb", Prefix: "positions/date={{today}}/", Region: "garage",
		AccessKeyID: "${LAKE_ID}", SecretAccessKey: "${LAKE_SECRET}", SessionToken: "${LAKE_SESSION}", MaxBytes: 1 << 20}, Vars{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"hex":"A","count":2},{"hex":"A","count":3},{"hex":"B","count":4}]`
	if string(got.Content) != want {
		t.Fatalf("content = %s, want %s", got.Content, want)
	}
	reduced, err := transforms.Apply("json-group-sum", []byte(`{"group_by":["hex"],"sum":["count"]}`), got.Content)
	if err != nil || !strings.Contains(string(reduced), `"sum(count)":"5"`) || !strings.Contains(string(reduced), `"sum(count)":"4"`) {
		t.Fatalf("lake reduction = %s, err = %v", reduced, err)
	}
	if strings.Contains(fmt.Sprint(got.Meta), "lake-secret") || len(seen) != 3 || seen[1] != "/adsb/positions/date=2026-11-03/a.json.gz" {
		t.Fatalf("meta = %v, requests = %v", got.Meta, seen)
	}
}

func TestLakeRefusesCredentialEscapeRedirectAndObjectOverflow(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/redirect/adsb" {
			http.Redirect(w, r, "/adsb", http.StatusFound)
			return
		}
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>p/a.json.gz</Key></Contents><Contents><Key>p/b.json.gz</Key></Contents></ListBucketResult>`)
	}))
	defer srv.Close()
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("signed redirect reached another server") }))
	defer other.Close()
	env := func(name string) (string, bool) { return "secret", name == "KEY" || name == "SECRET" }
	base := spec.Source{Name: "x", Kind: "lake", URL: srv.URL, Bucket: "adsb", Prefix: "p/", Region: "garage", AccessKeyID: "${KEY}", SecretAccessKey: "${SECRET}", MaxObjects: 1, MaxBytes: 1 << 20}
	f := &Fetcher{Client: srv.Client(), Env: env, Credentials: spec.Credentials{"KEY": {URLPrefix: other.URL + "/"}, "SECRET": {URLPrefix: other.URL + "/"}}}
	if _, err := f.Fetch(context.Background(), base, Vars{}); err == nil || !strings.Contains(err.Error(), "may be sent only to") || hits != 0 {
		t.Fatalf("off-prefix err = %v, hits = %d", err, hits)
	}
	f.Credentials = spec.Credentials{"KEY": {URLPrefix: srv.URL + "/"}, "SECRET": {URLPrefix: srv.URL + "/"}}
	if _, err := f.Fetch(context.Background(), base, Vars{}); err == nil || !strings.Contains(err.Error(), "more than max_objects") || hits != 1 {
		t.Fatalf("overflow err = %v, hits = %d", err, hits)
	}
	f.Credentials = nil
	base.URL = srv.URL + "/redirect"
	if _, err := f.Fetch(context.Background(), base, Vars{}); err == nil || !strings.Contains(err.Error(), "redirect refused") {
		t.Fatalf("redirect err = %v", err)
	}
}

func TestLakeDoesNotReturnServerErrorBodiesOrKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied lake-secret", http.StatusForbidden)
	}))
	defer srv.Close()
	env := func(name string) (string, bool) {
		return map[string]string{"KEY": "lake-access", "SECRET": "lake-secret"}[name], true
	}
	f := &Fetcher{Client: srv.Client(), Env: env, Credentials: spec.Credentials{"KEY": {URLPrefix: srv.URL + "/"}, "SECRET": {URLPrefix: srv.URL + "/"}}}
	source := spec.Source{Name: "x", Kind: "lake", URL: srv.URL, Bucket: "adsb", Prefix: "p/", Region: "garage", AccessKeyID: "${KEY}", SecretAccessKey: "${SECRET}", MaxBytes: 1 << 20}
	_, err := f.Fetch(context.Background(), source, Vars{})
	if err == nil || strings.Contains(err.Error(), "lake-secret") || strings.Contains(err.Error(), "lake-access") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("lake error = %v", err)
	}
}
