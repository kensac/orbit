package api_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

func TestAccountClaudePluginRequiresExactOperatorPinAndIsAccountScoped(t *testing.T) {
	v := newBoxEnv(t)
	cfg := &spec.Config{Deployment: spec.Deployment{Box: &spec.BoxConfig{PluginDigests: map[string]string{
		"review@local-market": string(bytes.Repeat([]byte("a"), 64)),
	}}}}
	if err := v.api.Engine.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	ws := v.workspaceID(t)
	accounts := v.srv.URL + "/api/v1/workspaces/" + ws + "/accounts"
	code, created := call(t, http.MethodPost, accounts, v.person("kanishk"), `{"label":"personal","cols":80,"rows":24}`)
	if code != http.StatusCreated {
		t.Fatalf("create account: %d %v", code, created)
	}
	accountID := created["account"].(map[string]any)["id"].(string)
	url := accounts + "/" + accountID + "/claude-plugins/review@local-market"
	archive := pluginTarForAPI(t)
	archiveHash := sha256.Sum256(archive)
	archiveDigest := hex.EncodeToString(archiveHash[:])
	if code, out := putPlugin(t, url, v.person("kanishk"), archive, string(bytes.Repeat([]byte("b"), 64)), archiveDigest); code != http.StatusForbidden || out["check"] != "plugin_digest_mismatch" {
		t.Fatalf("wrong plugin pin: %d %v", code, out)
	}
	terminals, err := v.api.Store.ListTerminals(context.Background(), ws, store.TerminalRunning)
	if err != nil {
		t.Fatal(err)
	}
	hasAccountTerminal := false
	for _, terminal := range terminals {
		if terminal.AccountID != nil && *terminal.AccountID == accountID {
			hasAccountTerminal = true
			break
		}
	}
	if !hasAccountTerminal {
		if _, err := v.api.Store.CreateTerminal(context.Background(), store.NewBoxTerminal{WorkspaceID: ws, Preset: "shell", Title: "active", Cwd: "/work", Cols: 80, Rows: 24, CreatedBy: "test", AccountID: accountID}); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := putPlugin(t, url, v.person("kanishk"), archive, string(bytes.Repeat([]byte("a"), 64)), archiveDigest); code != http.StatusConflict || out["check"] != "account_busy" {
		t.Fatalf("active account terminal: %d %v", code, out)
	}
	terminals, err = v.api.Store.ListTerminals(context.Background(), ws, store.TerminalRunning)
	if err != nil {
		t.Fatal(err)
	}
	for _, terminal := range terminals {
		if terminal.AccountID != nil && *terminal.AccountID == accountID {
			if _, err := v.api.Store.EndTerminal(context.Background(), terminal.ID, store.EndKilled, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if code, out := putPlugin(t, url, v.person("kanishk"), archive, string(bytes.Repeat([]byte("a"), 64)), archiveDigest); code != http.StatusOK {
		t.Fatalf("pinned plugin import: %d %v", code, out)
	}
	v.fake.mu.Lock()
	defer v.fake.mu.Unlock()
	if len(v.fake.pluginBundles) != 1 || !bytes.Equal(v.fake.pluginBundles[0], archive) || len(v.fake.plugins) != 1 || v.fake.plugins[0].Plugin != "review@local-market" {
		t.Fatalf("sandbox received unexpected plugin import: %d %v", len(v.fake.pluginBundles), v.fake.plugins)
	}
}

func putPlugin(t *testing.T, url, token string, archive []byte, pluginDigest, archiveDigest string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("X-Orbit-Plugin-SHA256", pluginDigest)
	req.Header.Set("X-Orbit-Bundle-SHA256", archiveDigest)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("response %d: %s: %v", resp.StatusCode, body, err)
	}
	return resp.StatusCode, out
}

func pluginTarForAPI(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range []struct {
		path string
		data string
	}{
		{"plugin/local-market/review/1.4.0/.claude-plugin/plugin.json", `{"name":"review","version":"1.4.0"}`},
		{"plugin/local-market/review/1.4.0/hooks/check.sh", "#!/bin/sh\nexit 0\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.path, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(f.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
