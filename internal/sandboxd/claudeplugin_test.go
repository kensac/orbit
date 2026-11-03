package sandboxd

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestClaudePluginInstallIsPinnedAndAccountScoped(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	archive, digest := testClaudePluginArchive(t)
	archiveHash := sha256.Sum256(archive)
	root := t.TempDir()
	accountA := filepath.Join(root, "account-a")
	accountB := filepath.Join(root, "account-b")
	for _, account := range []string{accountA, accountB} {
		if err := os.MkdirAll(account, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(account, "settings.json"), []byte(`{"model":"sonnet"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{accountA, "review@local-market", digest, hex.EncodeToString(archiveHash[:])}
	out := runPluginScript(t, python, args, archive)
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil || result["digest"] != digest {
		t.Fatalf("install result = %s, %v", out, err)
	}
	settingsA := readPluginSettings(t, accountA)
	if settingsA["model"] != "sonnet" || settingsA["enabledPlugins"].(map[string]any)["review@local-market"] != true {
		t.Fatalf("account A settings = %v", settingsA)
	}
	settingsB := readPluginSettings(t, accountB)
	if settingsB["model"] != "sonnet" || settingsB["enabledPlugins"] != nil {
		t.Fatalf("account B settings changed: %v", settingsB)
	}
	if _, err := os.Stat(filepath.Join(accountB, "plugins")); !os.IsNotExist(err) {
		t.Fatalf("plugin was installed into account B: %v", err)
	}
	indexPath := filepath.Join(accountA, "plugins", "installed_plugins.json")
	index := readPluginSettings(t, filepath.Dir(indexPath))
	records := index["plugins"].(map[string]any)["review@local-market"].([]any)
	if len(records) != 1 {
		t.Fatalf("install records = %v", records)
	}
	_ = runPluginScript(t, python, args, archive)
	index = readPluginSettings(t, filepath.Dir(indexPath))
	records = index["plugins"].(map[string]any)["review@local-market"].([]any)
	if len(records) != 1 {
		t.Fatalf("idempotent retry added records: %v", records)
	}
	installedDir := filepath.Join(accountA, "plugins", "cache", "local-market", "review", "1.4.0")
	if err := os.WriteFile(filepath.Join(installedDir, "unexpected.sh"), []byte("exit 9"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, append([]string{"-c", claudePluginInstallScript}, args...)...)
	cmd.Stdin = bytes.NewReader(archive)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("existing payload with extra file accepted: %s", out)
	}
}

func TestClaudePluginInstallRejectsDigestMismatchBeforeWriting(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	archive, _ := testClaudePluginArchive(t)
	hash := sha256.Sum256(archive)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-c", claudePluginInstallScript, root, "review@local-market", string(bytes.Repeat([]byte("0"), 64)), hex.EncodeToString(hash[:]))
	cmd.Stdin = bytes.NewReader(archive)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("mismatched pin accepted: %s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "plugins")); !os.IsNotExist(err) {
		t.Fatalf("mismatch wrote plugin files: %v", err)
	}
}

func testClaudePluginArchive(t *testing.T) ([]byte, string) {
	t.Helper()
	files := []struct {
		path string
		mode int64
		data []byte
	}{
		{".claude-plugin/plugin.json", 0o644, []byte(`{"name":"review","version":"1.4.0"}`)},
		{"hooks/check.sh", 0o755, []byte("#!/bin/sh\nexit 0\n")},
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, f := range files {
		name := "plugin/local-market/review/1.4.0/" + f.path
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: f.mode, Size: int64(len(f.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "orbit-claude-plugin-v1\x00review@local-market\x001.4.0\x00")
	for _, f := range files {
		fileHash := sha256.Sum256(f.data)
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\x00", f.path, f.mode, len(f.data), hex.EncodeToString(fileHash[:]))
	}
	return archive.Bytes(), hex.EncodeToString(h.Sum(nil))
}

func runPluginScript(t *testing.T, python string, args []string, archive []byte) []byte {
	t.Helper()
	cmd := exec.Command(python, append([]string{"-c", claudePluginInstallScript}, args...)...)
	cmd.Stdin = bytes.NewReader(archive)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plugin script failed: %v\n%s", err, out)
	}
	return out
}

func readPluginSettings(t *testing.T, dir string) map[string]any {
	t.Helper()
	path := filepath.Join(dir, "settings.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		path = filepath.Join(dir, "installed_plugins.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
