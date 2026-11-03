package sandboxd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kensac/orbit/internal/sandbox"
)

func TestClaudeConfigScriptsListAndInstallWithoutFollowingLinks(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	root := filepath.Join(t.TempDir(), ".claude")
	if err := os.MkdirAll(filepath.Join(root, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("old instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := runConfigScript(t, python, claudeConfigStatusScript, []string{root}, nil)
	current, err := parseClaudeConfigStatus(status)
	if err != nil {
		t.Fatal(err)
	}
	archive := configArchive(t)
	clean, incoming, err := sandbox.ParseClaudeConfigTar(archive)
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(t.TempDir(), "stage")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTarFiles(t, stage, clean)
	request, err := json.Marshal(struct {
		Expected string                     `json:"expected"`
		Files    []sandbox.ClaudeConfigFile `json:"files"`
	}{Expected: current.Digest, Files: incoming.Files})
	if err != nil {
		t.Fatal(err)
	}
	out := runConfigScript(t, python, claudeConfigInstallScript, []string{stage, root}, request)
	var result struct {
		Conflict bool                       `json:"conflict"`
		Files    []sandbox.ClaudeConfigFile `json:"files"`
	}
	if err := json.Unmarshal(out, &result); err != nil || result.Conflict {
		t.Fatalf("install result = %s, %v", out, err)
	}
	installed, err := parseClaudeConfigStatus([]byte(mustJSON(t, result.Files)))
	if err != nil || installed.Digest == current.Digest {
		t.Fatalf("installed manifest = %+v, %v", installed, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "CLAUDE.md")); err != nil || string(b) != "new instructions\n" {
		t.Fatalf("CLAUDE.md = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "skills", "review", "SKILL.md")); err != nil || !bytes.Contains(b, []byte("Review changes")) {
		t.Fatalf("skill file = %q, %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("manual edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request, _ = json.Marshal(struct {
		Expected string                     `json:"expected"`
		Files    []sandbox.ClaudeConfigFile `json:"files"`
	}{Expected: current.Digest, Files: incoming.Files})
	out = runConfigScript(t, python, claudeConfigInstallScript, []string{stage, root}, request)
	if err := json.Unmarshal(out, &result); err != nil || !result.Conflict {
		t.Fatalf("stale apply result = %s, %v", out, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "CLAUDE.md")); err != nil || string(b) != "manual edit\n" {
		t.Fatalf("stale apply changed CLAUDE.md to %q: %v", b, err)
	}
}

func runConfigScript(t *testing.T, python, script string, args []string, stdin []byte) []byte {
	t.Helper()
	argv := append([]string{"-c", script}, args...)
	cmd := exec.CommandContext(context.Background(), python, argv...)
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 4 {
			return out
		}
		t.Fatalf("python script failed: %v\n%s", err, out)
	}
	return out
}

func configArchive(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	files := map[string]string{
		"CLAUDE.md":              "new instructions\n",
		"skills/review/SKILL.md": "---\nname: review\ndescription: Review changes.\n---\nInspect carefully.\n",
	}
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func writeTarFiles(t *testing.T, root string, archive []byte) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(root, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
