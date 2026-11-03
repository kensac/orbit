package api_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAccountClaudeConfigRequiresTargetDigestAndCopiesOnlyAllowlistedFiles(t *testing.T) {
	v := newBoxEnv(t)
	ws := v.workspaceID(t)
	accounts := v.srv.URL + "/api/v1/workspaces/" + ws + "/accounts"
	code, created := call(t, http.MethodPost, accounts, v.person("kanishk"), `{"label":"personal","cols":80,"rows":24}`)
	if code != http.StatusCreated {
		t.Fatalf("create account: %d %v", code, created)
	}
	accountID := created["account"].(map[string]any)["id"].(string)
	url := accounts + "/" + accountID + "/claude-config"
	code, read := call(t, http.MethodGet, url, v.person("kanishk"), "")
	if code != http.StatusOK {
		t.Fatalf("read config: %d %v", code, read)
	}
	current := read["config"].(map[string]any)
	expected := current["digest"].(string)
	archive := configTar(t, map[string]string{
		"CLAUDE.md":                     "Use the project rules.\n",
		"skills/review/SKILL.md":        "---\nname: review\ndescription: Review changes.\n---\nCheck the diff.\n",
		"skills/review/references/a.md": "Read the API contract.\n",
	})
	hash := sha256.Sum256(archive)
	bundleDigest := hex.EncodeToString(hash[:])
	code, applied := putConfig(t, url, v.person("kanishk"), archive, expected, bundleDigest)
	if code != http.StatusOK {
		t.Fatalf("apply config: %d %v", code, applied)
	}
	config := applied["config"].(map[string]any)
	if config["digest"] == expected || len(config["files"].([]any)) != 3 {
		t.Fatalf("updated config: %v", config)
	}
	v.fake.mu.Lock()
	defer v.fake.mu.Unlock()
	if len(v.fake.bundles) != 1 || !bytes.Equal(v.fake.bundles[0], archive) {
		t.Fatalf("box received unexpected payload: %d bundles", len(v.fake.bundles))
	}
}

func TestAccountClaudeConfigRejectsStaleAndUnsafeWrites(t *testing.T) {
	v := newBoxEnv(t)
	ws := v.workspaceID(t)
	accounts := v.srv.URL + "/api/v1/workspaces/" + ws + "/accounts"
	code, created := call(t, http.MethodPost, accounts, v.person("kanishk"), `{"label":"personal","cols":80,"rows":24}`)
	if code != http.StatusCreated {
		t.Fatalf("create account: %d %v", code, created)
	}
	accountID := created["account"].(map[string]any)["id"].(string)
	url := accounts + "/" + accountID + "/claude-config"
	_, read := call(t, http.MethodGet, url, v.person("kanishk"), "")
	expected := read["config"].(map[string]any)["digest"].(string)
	archive := configTar(t, map[string]string{"CLAUDE.md": "A project instruction.\n"})
	hash := sha256.Sum256(archive)
	digest := hex.EncodeToString(hash[:])
	if code, out := putConfig(t, url, v.person("kanishk"), archive, strings.Repeat("a", 64), digest); code != http.StatusConflict || out["check"] != "changed" {
		t.Fatalf("stale target: %d %v", code, out)
	}
	unsafe := configTar(t, map[string]string{"settings.json": `{"token":"secret"}`})
	unsafeHash := sha256.Sum256(unsafe)
	if code, out := putConfig(t, url, v.person("kanishk"), unsafe, expected, hex.EncodeToString(unsafeHash[:])); code != http.StatusBadRequest {
		t.Fatalf("unsafe file path: %d %v", code, out)
	}
	if code, out := putConfig(t, url, v.person("guest"), archive, expected, digest); code != http.StatusForbidden {
		t.Fatalf("nonmember write: %d %v", code, out)
	}
	v.fake.mu.Lock()
	defer v.fake.mu.Unlock()
	if len(v.fake.bundles) != 0 {
		t.Fatalf("rejected requests reached the box: %d bundles", len(v.fake.bundles))
	}
}

func putConfig(t *testing.T, url, token string, archive []byte, expected, digest string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("If-Match", expected)
	req.Header.Set("X-Orbit-Bundle-SHA256", digest)
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

func configTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for path, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: path, Typeflag: tar.TypeReg, Size: int64(len(content)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
