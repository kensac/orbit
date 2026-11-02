package bundle

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/client"
)

func apiFixture(t *testing.T, failThread bool) (*client.Client, *[]string) {
	t.Helper()
	calls := []string{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.RequestURI())
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("invalid authenticated read")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/agents/assistant":
			fmt.Fprint(w, `{"agent":{"name":"assistant","origin":"database","version":7,"spec_sha":"abc","spec":{"charter":"help"}},"memory":"current memory"}`)
		case "/api/v1/agents/assistant/memory":
			fmt.Fprint(w, `{"versions":[{"id":12,"content":"current memory"}]}`)
		case "/api/v1/agents/assistant/context":
			fmt.Fprint(w, `{"topics":[{"name":"finance"}],"skills":[{"name":"analysis","version":3}],"entries":3,"index":"index"}`)
		case "/api/v1/skills/analysis/versions/3":
			fmt.Fprint(w, `{"skill":{"name":"analysis","version":3,"content":"instructions","files":{"reference.txt":"text"}}}`)
		case "/api/v1/memory/entries":
			fmt.Fprint(w, `{"entries":[{"id":"entry","version":2,"body":"fact"}]}`)
		case "/api/v1/agents/assistant/conversations":
			if r.URL.Query().Get("archived") == "true" {
				fmt.Fprint(w, `{"conversations":[{"id":"archived"}],"has_more":false,"next_before":null}`)
			} else if r.URL.Query().Get("before") == "opaque /+?" {
				fmt.Fprint(w, `{"conversations":[{"id":"older"}],"has_more":false,"next_before":null}`)
			} else {
				fmt.Fprint(w, `{"conversations":[{"id":"newest"}],"has_more":true,"next_before":"opaque /+?"}`)
			}
		case "/api/v1/conversations/newest/thread":
			if failThread {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"error":"private-token and upstream secret"}`)
				return
			}
			if r.URL.Query().Get("before") == "thread-cursor" {
				fmt.Fprint(w, `{"items":[{"kind":"message","id":"first","body":"hello"}],"has_more":false,"next_before":null}`)
			} else {
				fmt.Fprint(w, `{"items":[{"kind":"run","id":"reply","report":"answer"}],"has_more":true,"next_before":"thread-cursor"}`)
			}
		case "/api/v1/conversations/older/thread", "/api/v1/conversations/archived/thread":
			fmt.Fprint(w, `{"items":[],"has_more":false,"next_before":null}`)
		default:
			t.Errorf("unexpected path %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	hc := client.HTTPClient()
	hc.Transport = srv.Client().Transport
	return &client.Client{BaseURL: srv.URL, Token: "private-token", HTTP: hc}, &calls
}

func TestExportTLSAPI(t *testing.T) {
	c, calls := apiFixture(t, false)
	target := filepath.Join(t.TempDir(), "export.zip")
	m, err := Export(context.Background(), c, "assistant", target)
	if err != nil {
		t.Fatal(err)
	}
	if !m.ResourceCoverageComplete || m.BackupComplete || m.ConsistentSnapshot {
		t.Fatalf("coverage %+v", m)
	}
	if err := Verify(target); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info, err)
	}
	if len(*calls) != 14 {
		t.Fatalf("calls %d: %v", len(*calls), *calls)
	}
	var cursor, revision bool
	for _, r := range m.Resources {
		if r.Cursor == "opaque /+?" {
			cursor = true
		}
		for _, id := range r.Entities {
			if id.Name == "analysis" && string(id.Version) == "3" {
				revision = true
			}
		}
	}
	if !cursor || !revision {
		t.Fatal("cursor or revision missing")
	}
	z, err := zip.OpenReader(target)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	for _, f := range z.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r)
		_ = r.Close()
		if bytes.Contains(b, []byte("private-token")) {
			t.Fatal("token leaked")
		}
	}
}

func TestPartialExport(t *testing.T) {
	c, _ := apiFixture(t, true)
	target := filepath.Join(t.TempDir(), "partial.zip")
	m, err := Export(context.Background(), c, "assistant", target)
	if !errors.Is(err, ErrIncomplete) || m.ResourceCoverageComplete {
		t.Fatalf("error %v coverage %v", err, m.ResourceCoverageComplete)
	}
	if err := Verify(target); err != nil {
		t.Fatal(err)
	}
	var failure bool
	for _, r := range m.Resources {
		if r.Coverage == "error" {
			failure = true
			if r.Error != "resource fetch failed" {
				t.Fatalf("unsafe error %q", r.Error)
			}
		}
	}
	if !failure {
		t.Fatal("missing failed resource")
	}
}

type fetchFunc func(context.Context, string, string, io.Reader, io.Writer) error

func (f fetchFunc) Request(c context.Context, m, p string, b io.Reader, w io.Writer) error {
	return f(c, m, p, b, w)
}

func TestNoOverwrite(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "existing.zip")
			if !race {
				if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f := fetchFunc(func(_ context.Context, _, _ string, _ io.Reader, _ io.Writer) error {
				if race {
					if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("private-token")
			})
			if _, err := Export(context.Background(), f, "assistant", target); err == nil || errors.Is(err, ErrIncomplete) {
				t.Fatalf("overwrite not refused: %v", err)
			}
			b, _ := os.ReadFile(target)
			if string(b) != "keep" {
				t.Fatal("existing output changed")
			}
		})
	}
}

func TestPaginationProblemsAndCaps(t *testing.T) {
	for _, body := range []string{`{"items":[],"has_more":true,"next_before":"same"}`, `{"items":[],"has_more":true,"next_before":null}`, `{"items":[]}`} {
		var dst bytes.Buffer
		e := exporter{zip: zip.NewWriter(&dst), fetch: fetchFunc(func(_ context.Context, _, _ string, _ io.Reader, w io.Writer) error {
			_, err := io.WriteString(w, body)
			return err
		})}
		if err := e.pages(context.Background(), "conversations/id/thread?limit=200", "items", nil); err != nil {
			t.Fatal(err)
		}
		_ = e.zip.Close()
		if !e.failed || len(e.manifest.Resources) > 2 {
			t.Fatalf("unbounded pagination: %+v", e.manifest)
		}
	}
	e := exporter{bytes: maxBundleBytes}
	_, _, err := e.get(context.Background(), "conversations/id/thread?before=resume", "page", "resume")
	if !errors.Is(err, ErrIncomplete) || !e.failed || e.manifest.Resources[0].Cursor != "resume" {
		t.Fatalf("cap error %v manifest %+v", err, e.manifest)
	}
	var b boundedBuffer
	if _, err := b.Write(make([]byte, maxResponseBytes+1)); err == nil || b.Len() != 0 {
		t.Fatal("response cap not enforced")
	}
}

func rewriteArchive(t *testing.T, source, target string, change func(string, []byte) (string, []byte), extra string) {
	t.Helper()
	src, err := zip.OpenReader(source)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	file, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(file)
	for _, f := range src.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		name, data := change(f.Name, data)
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if extra != "" {
		w, err := z.Create(extra)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("extra"))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyTampering(t *testing.T) {
	c, _ := apiFixture(t, false)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.zip")
	if _, err := Export(context.Background(), c, "assistant", source); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"hash", "unsafe", "unlisted", "duplicate-member", "duplicate-reference", "negative-size", "oversized-size", "fileless", "trailing-manifest"} {
		t.Run(kind, func(t *testing.T) {
			target := filepath.Join(dir, kind+".zip")
			extra := ""
			if kind == "unsafe" {
				extra = "../outside"
			}
			if kind == "unlisted" {
				extra = "resources/000999.json"
			}
			if kind == "duplicate-member" {
				extra = "manifest.json"
			}
			rewriteArchive(t, source, target, func(name string, b []byte) (string, []byte) {
				if kind == "hash" && name == "resources/000001.json" {
					b = bytes.Replace(b, []byte("help"), []byte("harm"), 1)
				}
				if name == "manifest.json" {
					switch kind {
					case "trailing-manifest":
						b = append(b, []byte(` {}`)...)
					case "duplicate-reference", "negative-size", "oversized-size", "fileless":
						var m Manifest
						if err := json.Unmarshal(b, &m); err != nil {
							t.Fatal(err)
						}
						switch kind {
						case "duplicate-reference":
							m.Resources = append(m.Resources, m.Resources[0])
						case "fileless":
							m.Resources[0].File = ""
							m.Resources[0].Bytes = 0
							m.Resources[0].SHA256 = ""
						case "oversized-size":
							m.Resources[0].Bytes = maxResponseBytes + 1
						default:
							m.Resources[0].Bytes = -1
						}
						b, _ = json.Marshal(m)
					}
				}
				return name, b
			}, extra)
			if err := Verify(target); err == nil {
				t.Fatal("tampered archive accepted")
			}
		})
	}
}

func TestUnpaginatedLimitMarked(t *testing.T) {
	var dst bytes.Buffer
	f := fetchFunc(func(_ context.Context, _, endpoint string, _ io.Reader, w io.Writer) error {
		body := `{"agent":{"name":"assistant","spec":{}}}`
		switch {
		case strings.Contains(endpoint, "/memory?"):
			body = `{"versions":[]}`
		case strings.HasSuffix(endpoint, "/context"):
			body = `{"topics":[],"skills":[]}`
		case strings.HasPrefix(endpoint, "memory/entries?"):
			body = `{"entries":[` + strings.TrimSuffix(strings.Repeat(`{"id":"x","version":1},`, 1000), ",") + `]}`
		case strings.Contains(endpoint, "/conversations?"):
			body = `{"conversations":[],"has_more":false,"next_before":null}`
		}
		_, err := io.WriteString(w, body)
		return err
	})
	e := exporter{fetch: f, zip: zip.NewWriter(&dst)}
	if err := e.run(context.Background(), "assistant"); err != nil {
		t.Fatal(err)
	}
	_ = e.zip.Close()
	if !e.failed {
		t.Fatal("unpaginated limit marked complete")
	}
	var found bool
	for _, r := range e.manifest.Resources {
		if strings.Contains(r.Error, "possibly truncated") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing truncation reason")
	}
}

func TestCanceledExportRetainsData(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := fetchFunc(func(_ context.Context, _, _ string, _ io.Reader, w io.Writer) error {
		_, err := io.WriteString(w, `{"agent":{"name":"assistant","spec":{}},"memory":"kept"}`)
		cancel()
		return err
	})
	target := filepath.Join(t.TempDir(), "canceled.zip")
	m, err := Export(ctx, f, "assistant", target)
	if !errors.Is(err, ErrIncomplete) || len(m.Resources) != 2 || m.Resources[0].File == "" || m.Resources[1].Error != "export canceled" {
		t.Fatalf("manifest %+v error %v", m, err)
	}
	if err := Verify(target); err != nil {
		t.Fatal(err)
	}
}

func TestPageCapPublishesPartial(t *testing.T) {
	calls := 0
	f := fetchFunc(func(_ context.Context, _, endpoint string, _ io.Reader, w io.Writer) error {
		calls++
		body := `{"agent":{"name":"assistant","spec":{}}}`
		switch {
		case strings.Contains(endpoint, "/memory?"):
			body = `{"versions":[]}`
		case strings.HasSuffix(endpoint, "/context"):
			body = `{"topics":[],"skills":[]}`
		case strings.HasPrefix(endpoint, "memory/entries?"):
			body = `{"entries":[]}`
		case strings.Contains(endpoint, "/conversations?"):
			body = fmt.Sprintf(`{"conversations":[],"has_more":true,"next_before":"cursor-%d"}`, calls)
		}
		_, err := io.WriteString(w, body)
		return err
	})
	target := filepath.Join(t.TempDir(), "capped.zip")
	m, err := Export(context.Background(), f, "assistant", target)
	if !errors.Is(err, ErrIncomplete) || calls != maxResponses || len(m.Resources) != maxResponses+1 {
		t.Fatalf("calls %d records %d error %v", calls, len(m.Resources), err)
	}
	last := m.Resources[len(m.Resources)-1]
	if last.Cursor == "" || last.Coverage != "error" || last.Error != "export resource or byte limit reached" {
		t.Fatalf("last %+v", last)
	}
	if err := Verify(target); err != nil {
		t.Fatal(err)
	}
}
