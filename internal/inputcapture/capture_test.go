package inputcapture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/client"
)

func artifact(name, content string) Artifact {
	sum := sha256.Sum256([]byte(content))
	return Artifact{ID: "artifact", RunID: "run", Attempt: 2, Direction: "in", Name: name, MediaType: "application/json", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content))}
}

func server(t *testing.T, arts []Artifact, content string, contentStatus int) (*client.Client, *int) {
	t.Helper()
	calls := new(int)
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("unexpected request %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/v1/runs/run/artifacts":
			_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": arts})
		case "/api/v1/artifacts/artifact":
			*calls++
			w.WriteHeader(contentStatus)
			_, _ = w.Write([]byte(content))
		default:
			t.Errorf("forbidden endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.Close)
	hc := s.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client.Client{BaseURL: s.URL, Token: "test-secret", HTTP: hc}, calls
}

func TestCaptureProvenanceAndExclusions(t *testing.T) {
	a := artifact("inputs/session/data.json", "{\"x\":1}")
	withheld := artifact("sources/private.json", "secret")
	withheld.ID = "withheld"
	old := a
	old.Attempt = 1
	old.ID = "old"
	c, calls := server(t, []Artifact{a, withheld, old}, "{\"x\":1}", 200)
	target := filepath.Join(t.TempDir(), "capture")
	m, err := Capture(context.Background(), c, "run", 2, target)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Complete || len(m.Inputs) != 1 || m.Inputs[0].Artifact != a || *calls != 1 {
		t.Fatalf("capture: %+v calls %d", m, *calls)
	}
	for _, name := range []string{"manifest.json", "input-001.bin"} {
		b, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "test-secret") {
			t.Fatal("credential leaked")
		}
		info, _ := os.Stat(filepath.Join(target, name))
		if info.Mode().Perm() != 0600 {
			t.Fatal(info.Mode())
		}
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0700 {
		t.Fatal(info.Mode())
	}
	if _, err = Capture(context.Background(), c, "run", 2, target); err == nil {
		t.Fatal("clobbered capture")
	}
}

func TestPoisonAndPartial(t *testing.T) {
	for _, name := range []string{"inputs/../../outside", "inputs/a/../b", "inputs/a\\b", "inputs/", "inputs/a\x00b"} {
		t.Run(name, func(t *testing.T) {
			c, calls := server(t, []Artifact{artifact(name, "ok")}, "ok", 200)
			target := filepath.Join(t.TempDir(), "capture")
			m, err := Capture(context.Background(), c, "run", 2, target)
			if !errors.Is(err, ErrIncomplete) || m.Complete || *calls != 0 {
				t.Fatalf("%+v %v", m, err)
			}
		})
	}
	t.Run("partial", func(t *testing.T) {
		a := artifact("inputs/a", "ok")
		b := artifact("inputs/b", "ok")
		b.ID = "missing"
		c, _ := server(t, []Artifact{a, b}, "ok", 200)
		// Use a wrapper to fail the second fetch without exposing its error text.
		f := failSecond{c}
		target := filepath.Join(t.TempDir(), "capture")
		m, err := Capture(context.Background(), f, "run", 2, target)
		if !errors.Is(err, ErrIncomplete) || len(m.Inputs) != 1 {
			t.Fatalf("%+v %v", m, err)
		}
		data, _ := os.ReadFile(filepath.Join(target, "manifest.json"))
		if strings.Contains(string(data), "secret-error") {
			t.Fatal("error leaked")
		}
	})
}

type failSecond struct{ *client.Client }

func (f failSecond) Request(ctx context.Context, method, path string, body io.Reader, out io.Writer) error {
	if path == "artifacts/missing" {
		return errors.New("secret-error")
	}
	return f.Client.Request(ctx, method, path, body, out)
}

func TestInvalidContentAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		status        int
		cancel        bool
	}{
		{"digest", "no", 200, false}, {"oversize", "too long", 200, false}, {"status", "secret-error", 500, false}, {"cancel", "ok", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := server(t, []Artifact{artifact("inputs/a", "ok")}, tc.content, tc.status)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			target := filepath.Join(t.TempDir(), "capture")
			m, err := Capture(ctx, c, "run", 2, target)
			if !errors.Is(err, ErrIncomplete) || m.Complete || len(m.Inputs) != 0 {
				t.Fatalf("%+v %v", m, err)
			}
			if _, err := os.Stat(filepath.Join(target, "manifest.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMetadataBounds(t *testing.T) {
	for _, mutate := range []func(*Artifact){func(a *Artifact) { a.Bytes = MaxArtifactBytes + 1 }, func(a *Artifact) { a.SHA256 = "bad" }, func(a *Artifact) { a.RunID = "other" }, func(a *Artifact) { a.ID = "../poison" }} {
		a := artifact("inputs/a", "ok")
		mutate(&a)
		c, calls := server(t, []Artifact{a}, "ok", 200)
		m, err := Capture(context.Background(), c, "run", 2, filepath.Join(t.TempDir(), "capture"))
		if !errors.Is(err, ErrIncomplete) || m.Complete || *calls != 0 {
			t.Fatalf("%+v %v", m, err)
		}
	}
}

func TestListingCapAndSymlinkNoClobber(t *testing.T) {
	a := artifact("inputs/a", "ok")
	arts := make([]Artifact, MaxArtifacts)
	for i := range arts {
		arts[i] = a
	}
	c, calls := server(t, arts, "ok", 200)
	target := filepath.Join(t.TempDir(), "capture")
	if _, err := Capture(context.Background(), c, "run", 2, target); !errors.Is(err, ErrIncomplete) || *calls != 0 {
		t.Fatalf("cap: %v", err)
	}
	parent := t.TempDir()
	destination := t.TempDir()
	link := filepath.Join(parent, "link")
	if err := os.Symlink(destination, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(context.Background(), c, "run", 2, link); err == nil {
		t.Fatal("accepted existing symlink")
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink modified: %v", err)
	}
}

func TestRecordedMemoryInput(t *testing.T) {
	a := artifact("inputs/session/_memory.md", "recorded memory")
	c, calls := server(t, []Artifact{a}, "recorded memory", 200)
	m, err := Capture(context.Background(), c, "run", 2, filepath.Join(t.TempDir(), "capture"))
	if err != nil || !m.Complete || len(m.Inputs) != 1 || m.Inputs[0].Artifact != a || *calls != 1 {
		t.Fatalf("recorded memory input: %+v %v", m, err)
	}
}
