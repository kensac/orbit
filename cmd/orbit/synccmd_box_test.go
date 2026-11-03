package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/sandbox"
)

func TestSyncClaudeAccountImportPlansAndAppliesBundle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("Use the project rules.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(root, "skills", "review")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: review\ndescription: Review changes.\n---\nCheck the diff.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const workspaceID = "11111111-1111-4111-8111-111111111111"
	const accountID = "22222222-2222-4222-8222-222222222222"
	currentDigest := sandbox.ClaudeConfigDigest(nil)
	var currentFiles []sandbox.ClaudeConfigFile
	var gets, puts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/"+workspaceID+"/accounts/"+accountID+"/claude-config" {
			t.Errorf("request path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.Method {
		case http.MethodGet:
			gets++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"config": sandbox.ClaudeConfigStatus{Digest: currentDigest, Files: currentFiles}})
		case http.MethodPut:
			puts++
			if got := r.Header.Get("If-Match"); got != currentDigest {
				t.Errorf("If-Match = %q, want %q", got, currentDigest)
			}
			archive, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(archive)
			if got := r.Header.Get("X-Orbit-Bundle-SHA256"); got != hex.EncodeToString(hash[:]) {
				t.Errorf("bundle digest = %q", got)
			}
			clean, manifest, err := sandbox.ParseClaudeConfigTar(archive)
			if err != nil {
				t.Fatalf("uploaded bundle: %v", err)
			}
			if len(manifest.Files) != 2 || len(clean) == 0 {
				t.Fatalf("uploaded manifest = %+v", manifest)
			}
			currentDigest = manifest.Digest
			currentFiles = manifest.Files
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"config": sandbox.ClaudeConfigStatus{Digest: currentDigest, Files: currentFiles}})
		default:
			t.Errorf("unexpected method %s", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	args := []string{"claude", "--api", server.URL, "--token", "test-token", "--workspace", workspaceID, "--account", accountID, "--claude-dir", root}
	var planOutput strings.Builder
	if err := syncCmd(context.Background(), args, &planOutput); err != nil {
		t.Fatal(err)
	}
	if puts != 0 {
		t.Fatalf("dry run made %d writes", puts)
	}
	var plan string
	for _, line := range strings.Split(planOutput.String(), "\n") {
		if strings.HasPrefix(line, "plan ") {
			plan = strings.TrimSpace(strings.TrimPrefix(line, "plan "))
			break
		}
	}
	if len(plan) != 64 {
		t.Fatalf("plan digest = %q", plan)
	}
	if strings.Contains(planOutput.String(), "Use the project rules") || strings.Contains(planOutput.String(), "Check the diff") {
		t.Fatal("plan output contains file contents")
	}

	applyArgs := append(append([]string(nil), args...), "--apply", plan)
	var applyOutput strings.Builder
	if err := syncCmd(context.Background(), applyArgs, &applyOutput); err != nil {
		t.Fatal(err)
	}
	if gets != 2 || puts != 1 || !strings.Contains(applyOutput.String(), "applied\t") {
		t.Fatalf("requests or output: gets=%d puts=%d output=%q", gets, puts, applyOutput.String())
	}
}
