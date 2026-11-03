package sandbox

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
)

func TestParseClaudeConfigTarCanonicalizesAllowlistedFiles(t *testing.T) {
	raw := claudeTar(t,
		tar.Header{Name: "skills/review/references/checklist.md", Typeflag: tar.TypeReg, Mode: 0o777, Uid: 22, Gid: 33}, "Check the diff.",
		tar.Header{Name: "CLAUDE.md", Typeflag: tar.TypeReg, Mode: 0o600}, "Read the project rules.",
		tar.Header{Name: "skills/review/SKILL.md", Typeflag: tar.TypeReg, Mode: 0o600}, "---\nname: review\ndescription: Review changes.\n---\nRead carefully.\n",
	)
	clean, got, err := ParseClaudeConfigTar(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 3 || got.Digest != ClaudeConfigDigest(got.Files) {
		t.Fatalf("manifest = %+v", got)
	}
	archiveHash := sha256.Sum256(clean)
	if len(hex.EncodeToString(archiveHash[:])) != 64 {
		t.Fatal("canonical archive digest has wrong length")
	}
	tr := tar.NewReader(bytes.NewReader(clean))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Mode != 0o644 || h.Uid != claudeConfigOwner || h.Gid != claudeConfigOwner || h.ModTime.Unix() != 0 || h.Typeflag != tar.TypeReg {
			t.Fatalf("untrusted metadata survived: %+v", h)
		}
	}
}

func TestSanitizeClaudeConfigTarRejectsNonAllowlistedPathsAndTypes(t *testing.T) {
	tests := []struct {
		name string
		h    tar.Header
		body string
	}{
		{name: "settings", h: tar.Header{Name: "settings.json", Typeflag: tar.TypeReg}, body: "{}"},
		{name: "plugins", h: tar.Header{Name: "plugins/installed_plugins.json", Typeflag: tar.TypeReg}, body: "{}"},
		{name: "MCP", h: tar.Header{Name: "mcp.json", Typeflag: tar.TypeReg}, body: "{}"},
		{name: "traversal", h: tar.Header{Name: "skills/review/../../settings.json", Typeflag: tar.TypeReg}, body: "{}"},
		{name: "symlink", h: tar.Header{Name: "CLAUDE.md", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}},
		{name: "directory", h: tar.Header{Name: "skills/review/", Typeflag: tar.TypeDir}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := ParseClaudeConfigTar(claudeTar(t, tt.h, tt.body)); err == nil {
				t.Fatal("unsafe config entry was accepted")
			}
		})
	}
}

func TestSanitizeClaudeConfigTarRequiresSkillManifest(t *testing.T) {
	raw := claudeTar(t, tar.Header{Name: "skills/review/references/checklist.md", Typeflag: tar.TypeReg}, "Check it.")
	if _, _, err := ParseClaudeConfigTar(raw); err == nil {
		t.Fatal("skill supporting file without SKILL.md was accepted")
	}
}

func TestClaudeConfigDigestIgnoresInputOrder(t *testing.T) {
	files := []ClaudeConfigFile{
		{Path: "skills/review/SKILL.md", Bytes: 9, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{Path: "CLAUDE.md", Bytes: 5, SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}
	if ClaudeConfigDigest(files) != ClaudeConfigDigest([]ClaudeConfigFile{files[1], files[0]}) {
		t.Fatal("manifest digest depends on file order")
	}
}

func claudeTar(t *testing.T, entries ...any) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for i := 0; i < len(entries); i += 2 {
		h := entries[i].(tar.Header)
		content, _ := entries[i+1].(string)
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(content))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
