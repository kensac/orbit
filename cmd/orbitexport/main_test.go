package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/kensac/orbit/internal/bundle"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInvalidCommand(t *testing.T) {
	for _, args := range [][]string{{}, {"restore", "x"}, {"export", "assistant"}, {"verify", "missing.zip"}, {"-url", "http://unsafe", "export", "assistant", "new.zip"}, {"-url", "https://orbit.example", "-timeout", "0s", "export", "assistant", "new.zip"}} {
		if err := run(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

type unavailableAPI struct{}

func (unavailableAPI) Request(context.Context, string, string, io.Reader, io.Writer) error {
	return errors.New("unavailable")
}

func TestInspectOfflinePartial(t *testing.T) {
	// Unusable connection and login settings must not affect offline inspection.
	t.Setenv("ORBIT_URL", "http://invalid")
	t.Setenv("ORBIT_OIDC_ISSUER", "invalid")
	target := filepath.Join(t.TempDir(), "partial.zip")
	if _, err := bundle.Export(context.Background(), unavailableAPI{}, "assistant", target); !errors.Is(err, bundle.ErrIncomplete) {
		t.Fatal(err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"inspect", target}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var m bundle.Manifest
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Agent != "assistant" || m.ResourceCoverageComplete || m.BackupComplete || m.ConsistentSnapshot || len(m.Resources) == 0 || m.Resources[0].Coverage != "error" {
		t.Fatalf("incorrect inspection: %+v", m)
	}
	after, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("inspection changed archive", err)
	}
	out.Reset()
	if err := os.WriteFile(target, []byte("invalid archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"inspect", target}, &out, io.Discard); err == nil || out.Len() != 0 {
		t.Fatalf("invalid archive emitted output: %q, %v", out.String(), err)
	}
}

func TestInspectOutputBound(t *testing.T) {
	// A valid small manifest can expand sixfold when HTML characters are escaped.
	target := filepath.Join(t.TempDir(), "expanding.zip")
	f, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	member, err := z.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(member)
	enc.SetEscapeHTML(false)
	now := time.Now().UTC()
	if err := enc.Encode(bundle.Manifest{FormatVersion: 1, Agent: "assistant", Started: now, Finished: now, Exclusions: []string{strings.Repeat("<", 3<<20)}}); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Verify(target); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"inspect", target}, &out, io.Discard); err == nil || !strings.Contains(err.Error(), "output bound") || out.Len() != 0 {
		t.Fatalf("output bound failed: %d bytes, %v", out.Len(), err)
	}
}
