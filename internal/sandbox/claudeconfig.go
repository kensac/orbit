package sandbox

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/store"
)

const (
	// MaxClaudeBundleBytes matches the portable bundle limit used by the local exporter.
	MaxClaudeBundleBytes = MaxClaudeConfigBytes
	maxClaudeConfigFiles = 2000
	claudeConfigOwner    = 10001
)

// ClaudeConfigFile describes one regular file in the shared Claude profile.
type ClaudeConfigFile struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// ClaudeConfigStatus is the allowlisted view of a box profile. Digest names
// the sorted path, size and content-hash manifest, never file contents.
type ClaudeConfigStatus struct {
	Digest string             `json:"digest"`
	Files  []ClaudeConfigFile `json:"files"`
}

// ClaudePluginInstallResult reports the one account-scoped plugin that was installed.
type ClaudePluginInstallResult struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Files   int    `json:"files"`
}

// ClaudeConfigDigest returns the digest of a file manifest in path order.
func ClaudeConfigDigest(files []ClaudeConfigFile) string {
	ordered := slices.Clone(files)
	slices.SortFunc(ordered, func(a, b ClaudeConfigFile) int { return strings.Compare(a.Path, b.Path) })
	h := sha256.New()
	for _, f := range ordered {
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s\x00", f.Path, f.Bytes, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SanitizeClaudeConfigTar accepts only the CLAUDE.md and regular skill files
// supported by local bundle export, and drops all caller-supplied metadata.
func SanitizeClaudeConfigTar(dst io.Writer, src io.Reader) (ClaudeConfigStatus, error) {
	status := ClaudeConfigStatus{Files: []ClaudeConfigFile{}}
	tw := tar.NewWriter(dst)
	tr := tar.NewReader(src)
	seen := map[string]bool{}
	skills := map[string]bool{}
	skillContent := map[string][]byte{}
	skillFiles := map[string]int{}
	skillBytes := map[string]int{}
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ClaudeConfigStatus{}, fmt.Errorf("read Claude bundle: %w", err)
		}
		if n >= maxClaudeConfigFiles {
			return ClaudeConfigStatus{}, fmt.Errorf("claude bundle has more than %d files", maxClaudeConfigFiles)
		}
		if h.Typeflag != tar.TypeReg {
			return ClaudeConfigStatus{}, fmt.Errorf("claude bundle entry %q is not a regular file", h.Name)
		}
		if !validClaudeConfigPath(h.Name) {
			return ClaudeConfigStatus{}, fmt.Errorf("claude bundle path %q is not allowed", h.Name)
		}
		if seen[h.Name] {
			return ClaudeConfigStatus{}, fmt.Errorf("claude bundle path %q appears twice", h.Name)
		}
		seen[h.Name] = true
		if strings.HasPrefix(h.Name, "skills/") {
			parts := strings.Split(h.Name, "/")
			if len(parts) >= 3 {
				skills[parts[1]] = true
			}
		}
		total += h.Size
		if h.Size < 0 || total > MaxClaudeBundleBytes {
			return ClaudeConfigStatus{}, fmt.Errorf("claude bundle content exceeds %d bytes", MaxClaudeBundleBytes)
		}
		content, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if err != nil {
			return ClaudeConfigStatus{}, fmt.Errorf("read %s: %w", h.Name, err)
		}
		if int64(len(content)) != h.Size {
			return ClaudeConfigStatus{}, fmt.Errorf("read %s: expected %d bytes, got %d", h.Name, h.Size, len(content))
		}
		if strings.HasPrefix(h.Name, "skills/") {
			parts := strings.Split(h.Name, "/")
			name := parts[1]
			if len(parts) == 3 && parts[2] == "SKILL.md" {
				if len(content) > 256<<10 || !utf8.Valid(content) {
					return ClaudeConfigStatus{}, fmt.Errorf("skill %q SKILL.md must be UTF-8 and at most 262144 bytes", name)
				}
				skillContent[name] = content
				skillBytes[name] += len(content)
			} else {
				if len(content) > 4<<20 {
					return ClaudeConfigStatus{}, fmt.Errorf("skill %q file %q exceeds 4194304 bytes", name, strings.Join(parts[2:], "/"))
				}
				skillFiles[name]++
				skillBytes[name] += len(content)
				if skillFiles[name] > 200 || skillBytes[name] > 16<<20 {
					return ClaudeConfigStatus{}, fmt.Errorf("skill %q exceeds its file or size limit", name)
				}
			}
		}
		fhash := sha256.Sum256(content)
		status.Files = append(status.Files, ClaudeConfigFile{Path: h.Name, Bytes: len(content), SHA256: hex.EncodeToString(fhash[:])})
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: h.Name, Mode: 0o644, Uid: claudeConfigOwner, Gid: claudeConfigOwner, Size: h.Size, ModTime: time.Unix(0, 0).UTC()}); err != nil {
			return ClaudeConfigStatus{}, err
		}
		if _, err := tw.Write(content); err != nil {
			return ClaudeConfigStatus{}, err
		}
	}
	if len(status.Files) == 0 {
		return ClaudeConfigStatus{}, errors.New("claude bundle has no files")
	}
	for name := range skills {
		if !seen[path.Join("skills", name, "SKILL.md")] {
			return ClaudeConfigStatus{}, fmt.Errorf("skill %q has no SKILL.md", name)
		}
		if _, err := store.SkillDescription(name, string(skillContent[name])); err != nil {
			return ClaudeConfigStatus{}, fmt.Errorf("skill %q has an invalid SKILL.md: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return ClaudeConfigStatus{}, err
	}
	slices.SortFunc(status.Files, func(a, b ClaudeConfigFile) int { return strings.Compare(a.Path, b.Path) })
	status.Digest = ClaudeConfigDigest(status.Files)
	return status, nil
}

func validClaudeConfigPath(name string) bool {
	if name == "CLAUDE.md" {
		return true
	}
	if strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) < 3 || parts[0] != "skills" || !store.SkillNameRE.MatchString(parts[1]) {
		return false
	}
	for i, part := range parts[2:] {
		if part == "SKILL.md" && (i != len(parts[2:])-1 || len(parts) != 3) {
			return false
		}
	}
	if len(parts) == 3 && parts[2] == "SKILL.md" {
		return true
	}
	return store.ValidSkillPath(strings.Join(parts[2:], "/")) == nil
}

// ParseClaudeConfigTar validates and canonicalizes an uploaded bundle.
func ParseClaudeConfigTar(raw []byte) ([]byte, ClaudeConfigStatus, error) {
	var clean bytes.Buffer
	status, err := SanitizeClaudeConfigTar(&clean, bytes.NewReader(raw))
	if err != nil {
		return nil, ClaudeConfigStatus{}, err
	}
	return clean.Bytes(), status, nil
}
