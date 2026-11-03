package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncClaudeExportsMCPConfigPatchWithDigestCAS(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, "claude.json")
	dest := filepath.Join(home, "orbit-mcp-patch.yaml")
	if err := os.WriteFile(source, []byte(`{"oauthAccount":{"accessToken":"hidden"},"mcpServers":{"docs":{"type":"http","url":"https://docs.example/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"claude", "--mcp-patch", dest, "--mcp-config", source}
	var out strings.Builder
	if err := syncCmd(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote patch: %v", err)
	}
	if strings.Contains(out.String(), "hidden") || !strings.Contains(out.String(), "plan ") {
		t.Fatalf("dry run leaked local config or omitted digest: %s", out.String())
	}
	digest := strings.TrimSpace(strings.TrimPrefix(strings.Split(out.String(), "plan ")[1], ""))
	digest = strings.Fields(digest)[0]
	if err := syncCmd(context.Background(), append(args, "--apply", strings.Repeat("0", 64)), &out); err == nil || !strings.Contains(err.Error(), "does not match plan") {
		t.Fatalf("stale apply = %v", err)
	}
	if err := syncCmd(context.Background(), append(args, "--apply", digest), &out); err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(patch), "hidden") || !strings.Contains(string(patch), "mcp_servers") {
		t.Fatalf("bad exported patch: %s", patch)
	}
	info, err := os.Stat(dest)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("patch mode = %v, %v", info.Mode(), err)
	}
	if err := syncCmd(context.Background(), append(args, "--apply", digest), &out); err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("existing patch overwrite = %v", err)
	}
}
