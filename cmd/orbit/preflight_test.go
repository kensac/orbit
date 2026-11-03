package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/testdb"
)

func TestPreflight(t *testing.T) {
	ctx := context.Background()
	s := testdb.Empty(t)
	if err := s.MigrateTo(ctx, 15); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := preflight(ctx, s, &out); err != nil {
		t.Fatalf("preflight from version 15: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "15 applied") || !strings.Contains(out.String(), "rolled back") {
		t.Fatalf("output:\n%s", out.String())
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := preflight(ctx, s, &out); err != nil || !strings.Contains(out.String(), "schema current") {
		t.Fatalf("after migrate: %v\n%s", err, out.String())
	}
}

func TestWorkerPreflightFlagCannotUseLocalConfig(t *testing.T) {
	if err := preflightCmd(context.Background(), []string{"--worker", "--config", "examples"}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("preflight with both modes = %v", err)
	}
}

func TestWorkerPreflightReadsControlConfigAndChecksCredentials(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte("defaults: {runner: r, channels: [c]}\nrunners: {r: {kind: claude-code}}\nchannels: {c: {kind: log}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents", "a.yaml"), []byte("name: a\ncharter: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SyncAgents(ctx, s, cfg); err != nil {
		t.Fatal(err)
	}
	lookup := func(k string) (string, bool) {
		values := map[string]string{"ORBIT_WORKERS": "3", "CLAUDE_CODE_OAUTH_TOKEN": "test-secret"}
		v, ok := values[k]
		return v, ok
	}
	beforeDeployment, beforeAgents, err := s.StoredConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := workerPreflight(ctx, s, &out, lookup); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 agent(s) covered") || !strings.Contains(out.String(), "3 worker(s)") || strings.Contains(out.String(), "test-secret") {
		t.Fatalf("worker preflight output = %q", out.String())
	}
	afterDeployment, afterAgents, err := s.StoredConfig(ctx)
	if err != nil || string(beforeDeployment) != string(afterDeployment) || len(beforeAgents) != len(afterAgents) {
		t.Fatalf("preflight changed stored config: err=%v agents %d -> %d", err, len(beforeAgents), len(afterAgents))
	}
	lookup = func(k string) (string, bool) {
		if k == "ORBIT_WORKERS" {
			return "3", true
		}
		return "", false
	}
	if err := workerPreflight(ctx, s, &out, lookup); err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY") {
		t.Fatalf("missing backend credential = %v", err)
	}
}
