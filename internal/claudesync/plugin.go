package claudesync

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxPluginArchiveBytes = 32 << 20
	maxPluginFiles        = 2000
)

// PluginFile is one regular file in the complete, canonical plugin payload.
type PluginFile struct {
	Path   string
	Mode   int64
	Bytes  int64
	SHA256 string
	data   []byte
}

// PluginBundle contains one already-installed, user-enabled plugin. The
// digest binds its identity, version, paths, executable bits, sizes and bytes.
type PluginBundle struct {
	ID      string
	Version string
	Digest  string
	Archive []byte
	Files   []PluginFile
}

type pluginInstall struct {
	Scope       string `json:"scope"`
	InstallPath string `json:"installPath"`
	Version     string `json:"version"`
}

// NewPluginBundle copies one user-enabled plugin from Claude Code's local
// installed cache. It never copies settings or install records into the
// archive, and does not resolve marketplace sources or download packages.
func NewPluginBundle(claudeDir, id string) (PluginBundle, error) {
	name, market, ok := strings.Cut(id, "@")
	if !ok || !validPluginSegment(market) || !validPluginSegment(name) || strings.Contains(name, "@") {
		return PluginBundle{}, fmt.Errorf("plugin ID %q must be name@marketplace", id)
	}
	if err := requireRealDirectories(claudeDir, "plugins"); err != nil {
		return PluginBundle{}, fmt.Errorf("Claude plugin profile: %w", err)
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	settings, err := readBoundedJSONFile(settingsPath, 1<<20)
	if err != nil {
		return PluginBundle{}, fmt.Errorf("read Claude settings: %w", err)
	}
	var settingsDoc struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	settingsJSON, err := stripJSONC(settings)
	if err != nil || json.Unmarshal(settingsJSON, &settingsDoc) != nil {
		return PluginBundle{}, fmt.Errorf("read Claude enabled plugins: invalid settings JSON")
	}
	if !settingsDoc.EnabledPlugins[id] {
		return PluginBundle{}, fmt.Errorf("plugin %q is not enabled in local Claude settings", id)
	}
	pluginRoot := filepath.Join(claudeDir, "plugins")
	installed, err := readBoundedJSONFile(filepath.Join(pluginRoot, "installed_plugins.json"), 4<<20)
	if err != nil {
		return PluginBundle{}, fmt.Errorf("read installed Claude plugins: %w", err)
	}
	var index struct {
		Plugins map[string][]pluginInstall `json:"plugins"`
	}
	if err := json.Unmarshal(installed, &index); err != nil || index.Plugins == nil {
		return PluginBundle{}, fmt.Errorf("read installed Claude plugins: invalid install index")
	}
	var record *pluginInstall
	for i := range index.Plugins[id] {
		candidate := &index.Plugins[id][i]
		if candidate.Scope != "user" {
			continue
		}
		if record != nil {
			return PluginBundle{}, fmt.Errorf("plugin %q has multiple user installations", id)
		}
		record = candidate
	}
	if record == nil || !validPluginSegment(record.Version) {
		return PluginBundle{}, fmt.Errorf("plugin %q has no valid user install record", id)
	}
	cache := filepath.Join(pluginRoot, "cache", market, name, record.Version)
	cache, err = filepath.Abs(cache)
	if err != nil {
		return PluginBundle{}, err
	}
	installPath, err := filepath.Abs(record.InstallPath)
	if err != nil || filepath.Clean(installPath) != filepath.Clean(cache) {
		return PluginBundle{}, fmt.Errorf("plugin %q install path is not its local cache path", id)
	}
	if err := requireRealDirectories(claudeDir, filepath.Join("plugins", "cache", market, name, record.Version)); err != nil {
		return PluginBundle{}, fmt.Errorf("plugin %q cache path: %w", id, err)
	}
	files, err := readPluginTree(cache, id, record.Version)
	if err != nil {
		return PluginBundle{}, err
	}
	if err := validatePluginManifest(files, name, record.Version); err != nil {
		return PluginBundle{}, err
	}
	archive, err := canonicalPluginTar(id, record.Version, files)
	if err != nil {
		return PluginBundle{}, err
	}
	digest := pluginDigest(id, record.Version, files)
	return PluginBundle{ID: id, Version: record.Version, Digest: digest, Archive: archive, Files: pluginFileMetadata(files)}, nil
}

func stripJSONC(raw []byte) ([]byte, error) {
	clean := make([]byte, len(raw))
	quoted, escaped := false, false
	for i := 0; i < len(raw); {
		c := raw[i]
		if quoted {
			clean[i] = c
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			i++
			continue
		}
		if c == '"' {
			quoted = true
			clean[i] = c
			i++
			continue
		}
		if c == '/' && i+1 < len(raw) && (raw[i+1] == '/' || raw[i+1] == '*') {
			line := raw[i+1] == '/'
			clean[i], clean[i+1] = ' ', ' '
			i += 2
			closed := line
			for i < len(raw) {
				if line && (raw[i] == '\n' || raw[i] == '\r') {
					clean[i] = raw[i]
					i++
					closed = true
					break
				}
				if !line && raw[i] == '*' && i+1 < len(raw) && raw[i+1] == '/' {
					clean[i], clean[i+1] = ' ', ' '
					i += 2
					closed = true
					break
				}
				if raw[i] == '\n' || raw[i] == '\r' {
					clean[i] = raw[i]
				} else {
					clean[i] = ' '
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated JSON comment")
			}
			continue
		}
		clean[i] = c
		i++
	}
	if quoted {
		return nil, fmt.Errorf("unterminated JSON string")
	}
	out := make([]byte, 0, len(clean))
	quoted, escaped = false, false
	for i := 0; i < len(clean); {
		c := clean[i]
		if quoted {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			i++
			continue
		}
		if c == '"' {
			quoted = true
			out = append(out, c)
			i++
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(clean) && (clean[j] == ' ' || clean[j] == '\t' || clean[j] == '\r' || clean[j] == '\n') {
				j++
			}
			if j < len(clean) && (clean[j] == '}' || clean[j] == ']') {
				i++
				continue
			}
		}
		out = append(out, c)
		i++
	}
	return out, nil
}

func readBoundedJSONFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("file must be regular and not a symlink")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return nil, fmt.Errorf("file must be regular and no larger than %d bytes", limit)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return b, nil
}

func requireRealDirectories(root, relative string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("config directory is not a real directory")
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err = os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a real directory", part)
		}
	}
	return nil
}

func readPluginTree(root, id, version string) ([]PluginFile, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("plugin %q cache is not a real directory", id)
	}
	var files []PluginFile
	var total int64
	err = filepath.WalkDir(root, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("plugin %q contains a symlink", id)
		}
		if entry.IsDir() {
			return nil
		}
		st, err := os.Lstat(filePath)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("plugin %q contains a non-regular file", id)
		}
		rel, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !validPluginPath(rel) {
			return fmt.Errorf("plugin %q contains an unsafe path", id)
		}
		base := strings.ToLower(filepath.Base(filepath.FromSlash(rel)))
		if base == ".mcp.json" || base == ".claude.json" || base == ".credentials.json" || base == "settings.json" || base == "auth.json" || strings.HasPrefix(base, ".env") {
			return fmt.Errorf("plugin %q contains a settings or credential file", id)
		}
		content, err := readBoundedFile(filePath, maxPluginArchiveBytes-total)
		if err != nil {
			return err
		}
		if len(content) == 0 {
			// Empty files are valid plugin payloads and remain in the digest.
		}
		total += int64(len(content))
		if total > maxPluginArchiveBytes || len(files) >= maxPluginFiles {
			return fmt.Errorf("plugin %q exceeds its file or byte limit", id)
		}
		mode := int64(0o644)
		if st.Mode()&0o111 != 0 {
			mode = 0o755
		}
		hash := sha256.Sum256(content)
		files = append(files, PluginFile{Path: rel, Mode: mode, Bytes: int64(len(content)), SHA256: hex.EncodeToString(hash[:]), data: content})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read plugin %q v%s: %w", id, version, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("plugin %q has no files", id)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func readBoundedFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > max {
		return nil, fmt.Errorf("plugin file exceeds its byte limit")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("plugin file exceeds its byte limit")
	}
	return b, nil
}

func validPluginSegment(s string) bool {
	if s == "" || len(s) > 64 || s == "." || s == ".." || strings.Contains(s, "..") {
		return false
	}
	for i, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || (i > 0 && (c == '.' || c == '_' || c == '-')) {
			continue
		}
		return false
	}
	return true
}

func validPluginPath(s string) bool {
	if s == "" || len(s) > 1024 || strings.ContainsAny(s, "\\\x00") || strings.HasPrefix(s, "/") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || len(part) > 255 || part == "." || part == ".." {
			return false
		}
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(s))) == s
}

func validatePluginManifest(files []PluginFile, expectedName, expectedVersion string) error {
	for _, f := range files {
		if f.Path != ".claude-plugin/plugin.json" {
			continue
		}
		if !utf8.Valid(f.data) {
			return fmt.Errorf("plugin manifest must be UTF-8")
		}
		var m struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(f.data, &m); err != nil {
			return fmt.Errorf("plugin manifest is invalid JSON")
		}
		if !validPluginSegment(m.Name) || m.Version != expectedVersion {
			return fmt.Errorf("plugin manifest name or version does not match its install record")
		}
		return nil
	}
	return fmt.Errorf("plugin %q has no .claude-plugin/plugin.json", expectedName)
}

func pluginDigest(id, version string, files []PluginFile) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "orbit-claude-plugin-v1\x00%s\x00%s\x00", id, version)
	for _, f := range files {
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\x00", f.Path, f.Mode, f.Bytes, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalPluginTar(id, version string, files []PluginFile) ([]byte, error) {
	name, market, _ := strings.Cut(id, "@")
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, f := range files {
		name := "plugin/" + market + "/" + name + "/" + version + "/" + f.Path
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: f.Mode, Size: f.Bytes, ModTime: time.Unix(0, 0).UTC()}); err != nil {
			return nil, err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if b.Len() > maxPluginArchiveBytes {
		return nil, fmt.Errorf("plugin archive exceeds %d bytes", maxPluginArchiveBytes)
	}
	return b.Bytes(), nil
}

func pluginFileMetadata(files []PluginFile) []PluginFile {
	out := make([]PluginFile, len(files))
	for i, f := range files {
		out[i] = PluginFile{Path: f.Path, Mode: f.Mode, Bytes: f.Bytes, SHA256: f.SHA256}
	}
	return out
}
