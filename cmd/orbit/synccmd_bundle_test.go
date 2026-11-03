package main

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncClaudeExportsAPlannedPortableBundle(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "CLAUDE.md"), []byte("Keep changes small.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(source, "skills", "review")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: review\ndescription: Review code.\n---\nCheck the diff.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(`{"apiKey":"must stay local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "claude-box.tar")
	args := []string{"claude", "--bundle", dest, "--claude-dir", source}
	var out bytes.Buffer
	if err := syncCmd(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	digest := planOf(t, out.String())
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dry run created the bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "CLAUDE.md"), []byte("Changed after planning.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncCmd(context.Background(), append(args, "--apply", digest), &out); err == nil || !strings.Contains(err.Error(), "bundle changed") {
		t.Fatalf("stale bundle plan = %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "CLAUDE.md"), []byte("Keep changes small.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncCmd(context.Background(), append(args, "--apply", digest), &out); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if strings.Contains(h.Name, "settings") || strings.Contains(h.Name, "plugin") {
			t.Fatalf("private config leaked into bundle: %s", h.Name)
		}
	}
	if strings.Join(names, ",") != "CLAUDE.md,skills/review/SKILL.md" {
		t.Fatalf("bundle entries = %v", names)
	}
	if err := syncCmd(context.Background(), append(args, "--apply", digest), &out); err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("existing bundle was overwritten: %v", err)
	}
}
