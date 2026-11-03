package claudesync

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewPluginBundlePinsCompleteEnabledPayload(t *testing.T) {
	root := makeLocalPlugin(t, "review@local-market", "1.4.0")
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte("{\n // local profile\n \"enabledPlugins\": {\"review@local-market\": true,},\n}"), 0o600); err != nil {
		t.Fatal(err)
	}
	one, err := NewPluginBundle(root, "review@local-market")
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewPluginBundle(root, "review@local-market")
	if err != nil {
		t.Fatal(err)
	}
	archiveHash := sha256.Sum256(one.Archive)
	if one.Digest != pluginDigest(one.ID, one.Version, one.Files) || one.Digest == hex.EncodeToString(archiveHash[:]) {
		t.Fatalf("plugin digest is not the canonical content digest: %s", one.Digest)
	}
	if one.Digest == "" || one.Digest != two.Digest || string(one.Archive) != string(two.Archive) {
		t.Fatalf("bundle is not deterministic: %s %s", one.Digest, two.Digest)
	}
	if len(one.Files) != 2 || one.Files[1].Mode != 0o755 {
		t.Fatalf("files = %+v", one.Files)
	}
	tr := tar.NewReader(strings.NewReader(string(one.Archive)))
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		if h.ModTime.Unix() != 0 || (h.Mode != 0o644 && h.Mode != 0o755) {
			t.Fatalf("noncanonical tar metadata: %+v", h)
		}
	}
	if strings.Join(names, ",") != "plugin/local-market/review/1.4.0/.claude-plugin/plugin.json,plugin/local-market/review/1.4.0/hooks/check.sh" {
		t.Fatalf("archive entries = %v", names)
	}
	if strings.Contains(string(one.Archive), "settings.json") || strings.Contains(string(one.Archive), "secret") {
		t.Fatalf("private settings leaked into plugin archive")
	}
}

func TestNewPluginBundleRejectsDisabledMalformedAndChangedPlugin(t *testing.T) {
	root := makeLocalPlugin(t, "review@local-market", "1.4.0")
	settings := filepath.Join(root, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"enabledPlugins":{"review@local-market":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPluginBundle(root, "review@local-market"); err == nil {
		t.Fatal("disabled plugin was accepted")
	}
	if _, err := NewPluginBundle(root, "../../settings.json"); err == nil {
		t.Fatal("path traversal plugin id was accepted")
	}
	if err := os.WriteFile(settings, []byte(`{"enabledPlugins":{"review@local-market":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "plugins", "cache", "local-market", "review", "1.4.0", ".claude-plugin", "plugin.json")
	if err := os.WriteFile(manifest, []byte(`{"name":"review","version":"9.9.9"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPluginBundle(root, "review@local-market"); err == nil {
		t.Fatal("changed plugin version was accepted")
	}
}

func TestNewPluginBundleRejectsSymlinkAndUnsafeInstallPath(t *testing.T) {
	root := makeLocalPlugin(t, "review@local-market", "1.4.0")
	cache := filepath.Join(root, "plugins", "cache", "local-market", "review", "1.4.0")
	if err := os.Symlink(filepath.Join(cache, "hooks", "check.sh"), filepath.Join(cache, "copy.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPluginBundle(root, "review@local-market"); err == nil {
		t.Fatal("symlink was accepted")
	}
	if err := os.Remove(filepath.Join(cache, "copy.sh")); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(root, "plugins", "installed_plugins.json")
	index, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(index, &doc); err != nil {
		t.Fatal(err)
	}
	plugins := doc["plugins"].(map[string]any)
	records := plugins["review@local-market"].([]any)
	record := records[0].(map[string]any)
	record["installPath"] = filepath.Join(root, "outside")
	index, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, index, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPluginBundle(root, "review@local-market"); err == nil {
		t.Fatal("unsafe install path was accepted")
	}
}

func makeLocalPlugin(t *testing.T, id, version string) string {
	t.Helper()
	root := t.TempDir()
	name, market, _ := strings.Cut(id, "@")
	cache := filepath.Join(root, "plugins", "cache", market, name, version)
	if err := os.MkdirAll(filepath.Join(cache, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cache, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, ".claude-plugin", "plugin.json"), []byte(`{"name":"review","version":"`+version+`","description":"local"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "hooks", "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := []byte(`{"enabledPlugins":{"review@local-market":true},"secret":"must-not-export"}`)
	if err := os.WriteFile(filepath.Join(root, "settings.json"), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	installed := map[string]any{"version": 2, "plugins": map[string]any{id: []any{map[string]any{
		"scope": "user", "installPath": cache, "version": version,
	}}}}
	encoded, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugins", "installed_plugins.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestNewPluginBundleRejectsMCPConfig(t *testing.T) {
	root := makeLocalPlugin(t, "review@local-market", "1.4.0")
	path := filepath.Join(root, "plugins", "cache", "local-market", "review", "1.4.0", ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"private":{"env":{"TOKEN":"secret"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPluginBundle(root, "review@local-market"); err == nil {
		t.Fatal("MCP config was accepted")
	}
}
