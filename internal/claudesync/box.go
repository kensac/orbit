package claudesync

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/claudeimport"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/store"
)

const maxBoxClaudeFiles = 2000

// BoxFile is a file included in a portable, non-secret Claude config bundle.
type BoxFile struct {
	Path   string
	Bytes  int
	SHA256 string
}

// BoxBundle is a deterministic tar of CLAUDE.md and user skills. Its digest
// names the exact bytes that a later transfer would send.
type BoxBundle struct {
	Archive  []byte
	Digest   string
	Files    []BoxFile
	Warnings []string
}

// NewBoxBundle reads only CLAUDE.md and skills from Claude Code's config
// directory. Account state, settings, plugins and MCP configuration stay out:
// those can contain credentials or executable configuration and are handled
// by separate, explicit flows.
func NewBoxBundle(dir string) (BoxBundle, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return BoxBundle{}, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return BoxBundle{}, fmt.Errorf("Claude config path %q is not a directory", dir)
	}
	files := map[string][]byte{}
	var warnings []string
	add := func(name string, content []byte) error {
		if name == "" || strings.Contains(name, "\\") || filepath.IsAbs(name) || filepath.ToSlash(filepath.Clean(name)) != name || strings.HasPrefix(name, "../") || name == ".." {
			return fmt.Errorf("invalid Claude bundle path %q", name)
		}
		if _, exists := files[name]; exists {
			return fmt.Errorf("Claude bundle path %q appears twice", name)
		}
		files[name] = bytes.Clone(content)
		return nil
	}
	claudeMD := filepath.Join(dir, "CLAUDE.md")
	if st, err := os.Lstat(claudeMD); err == nil {
		if !st.Mode().IsRegular() {
			return BoxBundle{}, fmt.Errorf("%s is not a regular file", claudeMD)
		}
		b, err := os.ReadFile(claudeMD) //nolint:gosec // the operator named the local Claude config directory
		if err != nil {
			return BoxBundle{}, err
		}
		if err := add("CLAUDE.md", b); err != nil {
			return BoxBundle{}, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return BoxBundle{}, err
	}
	skillsDir := filepath.Join(dir, "skills")
	if st, err := os.Lstat(skillsDir); err == nil {
		if !st.IsDir() || st.Mode()&fs.ModeSymlink != 0 {
			return BoxBundle{}, fmt.Errorf("%s is not a directory", skillsDir)
		}
		skills, skipped, err := claudeimport.ReadSkillsDir(skillsDir)
		if err != nil {
			return BoxBundle{}, err
		}
		warnings = append(warnings, skipped...)
		for _, skill := range skills {
			base := filepath.ToSlash(filepath.Join("skills", skill.Name))
			if err := add(base+"/SKILL.md", []byte(skill.Content)); err != nil {
				return BoxBundle{}, err
			}
			for _, file := range skill.Files {
				if err := store.ValidSkillPath(file.Path); err != nil {
					return BoxBundle{}, err
				}
				if err := add(base+"/"+file.Path, file.Content); err != nil {
					return BoxBundle{}, err
				}
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return BoxBundle{}, err
	}
	if len(files) == 0 {
		return BoxBundle{}, errors.New("Claude config directory has no CLAUDE.md or skills to bundle")
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) > maxBoxClaudeFiles {
		return BoxBundle{}, fmt.Errorf("Claude config has more than %d files", maxBoxClaudeFiles)
	}
	var total int
	for _, name := range names {
		total += len(files[name])
		if total > sandbox.MaxClaudeConfigBytes {
			return BoxBundle{}, fmt.Errorf("Claude config content exceeds %d bytes", sandbox.MaxClaudeConfigBytes)
		}
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	modTime := time.Unix(0, 0).UTC()
	for _, name := range names {
		b := files[name]
		h := &tar.Header{Name: filepath.ToSlash(name), Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg, ModTime: modTime}
		if err := tw.WriteHeader(h); err != nil {
			return BoxBundle{}, err
		}
		if _, err := tw.Write(b); err != nil {
			return BoxBundle{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return BoxBundle{}, err
	}
	raw := archive.Bytes()
	if len(raw) > sandbox.MaxClaudeConfigBytes {
		return BoxBundle{}, fmt.Errorf("Claude config archive exceeds %d bytes", sandbox.MaxClaudeConfigBytes)
	}
	digest := sha256.Sum256(raw)
	out := BoxBundle{Archive: bytes.Clone(raw), Digest: hex.EncodeToString(digest[:]), Warnings: warnings}
	for _, name := range names {
		b := files[name]
		h := sha256.Sum256(b)
		out.Files = append(out.Files, BoxFile{Path: name, Bytes: len(b), SHA256: hex.EncodeToString(h[:])})
	}
	return out, nil
}
