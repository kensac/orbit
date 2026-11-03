package main

import (
	"bytes"
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

func TestSyncClaudePluginPlansThenSendsOnlyPinnedLocalPayload(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "plugins", "cache", "local-market", "review", "1.4.0")
	manifest := filepath.Join(cache, ".claude-plugin")
	if err := os.MkdirAll(manifest, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifest, "plugin.json"), []byte(`{"name":"review","version":"1.4.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cache, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "hooks", "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{"enabledPlugins":{"review@local-market":true},"secret":"do not send"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := json.Marshal(map[string]any{"version": 2, "plugins": map[string]any{"review@local-market": []any{map[string]any{
		"scope": "user", "installPath": cache, "version": "1.4.0",
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugins", "installed_plugins.json"), index, 0o600); err != nil {
		t.Fatal(err)
	}

	const workspaceID = "11111111-1111-4111-8111-111111111111"
	const accountID = "22222222-2222-4222-8222-222222222222"
	endpoint := "/api/v1/workspaces/" + workspaceID + "/accounts/" + accountID + "/claude-plugins/review@local-market"
	var puts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != endpoint {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusMethodNotAllowed)
			return
		}
		puts++
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		archive, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(archive)
		if got := r.Header.Get("X-Orbit-Bundle-SHA256"); got != hex.EncodeToString(h[:]) {
			t.Errorf("archive digest = %q", got)
		}
		if strings.Contains(string(archive), "do not send") || strings.Contains(string(archive), "settings.json") {
			t.Fatal("archive contains settings or a credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"plugin": sandbox.ClaudePluginInstallResult{Plugin: "review@local-market", Version: "1.4.0", Digest: r.Header.Get("X-Orbit-Plugin-SHA256"), Files: 2}})
	}))
	defer server.Close()

	args := []string{"claude", "--api", server.URL, "--token", "test-token", "--workspace", workspaceID, "--account", accountID, "--claude-dir", root, "--plugin", "review@local-market"}
	var planOutput bytes.Buffer
	if err := syncCmd(context.Background(), args, &planOutput); err != nil {
		t.Fatal(err)
	}
	if puts != 0 || !strings.Contains(planOutput.String(), "TRUST: plugin code runs with this account's privileges") {
		t.Fatalf("dry run sent data or omitted trust warning: %d %q", puts, planOutput.String())
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
	args = append(args, "--apply", plan)
	var applyOutput bytes.Buffer
	if err := syncCmd(context.Background(), args, &applyOutput); err != nil {
		t.Fatal(err)
	}
	if puts != 1 || !strings.Contains(applyOutput.String(), "installed\t") {
		t.Fatalf("apply: puts=%d output=%q", puts, applyOutput.String())
	}
}
