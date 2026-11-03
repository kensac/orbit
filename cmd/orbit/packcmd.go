package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/jsonstrict"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

const packFormat = "orbit.agent-pack"

type packDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type packSkill struct {
	Name    string       `json:"name"`
	Version int          `json:"version"`
	SHA256  string       `json:"sha256"`
	Files   []packDigest `json:"files"`
}
type packManifest struct {
	Format      string          `json:"format"`
	Version     int             `json:"version"`
	Agent       json.RawMessage `json:"agent"`
	AgentSHA256 string          `json:"agent_sha256"`
	Enabled     bool            `json:"enabled"`
	Environment []string        `json:"environment_variables"`
	Skills      []packSkill     `json:"skills"`
	Files       []packDigest    `json:"files"`
}

type packFile struct {
	path string
	data []byte
}

func packCmd(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "inspect" {
		if len(args) != 2 {
			return errors.New("pack inspect: name one pack directory")
		}
		m, err := inspectPack(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("valid Orbit agent pack v%d: %s (%d skills, %d files)\n", m.Version, agentName(m.Agent), len(m.Skills), len(m.Files))
		return nil
	}
	fs := flag.NewFlagSet("pack", flag.ContinueOnError)
	var api apiFlags
	api.register(fs)
	out := fs.String("output", "", "new directory for the plugin (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *out == "" {
		return errors.New("pack: use orbit pack <agent> --output <new-directory>, or orbit pack inspect <directory>")
	}
	return exportPack(ctx, &api, fs.Arg(0), *out)
}

func exportPack(ctx context.Context, api *apiFlags, name, dest string) error {
	var list struct {
		Agents []store.AgentRow `json:"agents"`
	}
	if err := api.call(ctx, http.MethodGet, "/api/v1/agents", nil, &list); err != nil {
		return err
	}
	var row *store.AgentRow
	for i := range list.Agents {
		if list.Agents[i].Name == name {
			row = &list.Agents[i]
			break
		}
	}
	if row == nil || !row.Present {
		return fmt.Errorf("pack: agent %q is not currently defined", name)
	}
	if err := jsonstrict.ValidateJSON(row.Spec, 256<<10); err != nil {
		return fmt.Errorf("pack: agent spec is invalid: %w", err)
	}
	var a spec.Agent
	if err := json.Unmarshal(row.Spec, &a); err != nil {
		return fmt.Errorf("pack: read agent spec: %w", err)
	}
	if a.Name != name {
		return fmt.Errorf("pack: server returned spec for %q, expected %q", a.Name, name)
	}
	if err := rejectInlineCredentials(&a); err != nil {
		return err
	}
	if claudeproto.ScrubCredentials(string(row.Spec)) != string(row.Spec) {
		return errors.New("pack: agent spec contains a credential-shaped value; remove it or replace it with an environment reference")
	}
	envNames := []string{}
	for _, source := range a.Sources {
		envNames = append(envNames, source.EnvRefs()...)
	}
	slices.Sort(envNames)
	envNames = slices.Compact(envNames)

	var skillList struct {
		Skills []struct {
			store.Skill
			Agents []string `json:"agents"`
		} `json:"skills"`
	}
	if err := api.call(ctx, http.MethodGet, "/api/v1/skills?status=active", nil, &skillList); err != nil {
		return err
	}
	requested := map[string]bool{}
	for _, n := range a.Skills {
		requested[n] = true
	}
	for _, k := range skillList.Skills {
		if slices.Contains(k.Agents, name) {
			requested[k.Name] = true
		}
	}
	var skills []packSkill
	files := []packFile{{path: ".claude-plugin/plugin.json", data: []byte(pluginManifest(name, a.Description))}, {path: "agents/" + name + ".md", data: []byte(pluginAgent(name, a.Description, a.Charter))}}
	files = append(files, packFile{path: "README.md", data: []byte(packReadme(name))})
	names := make([]string, 0, len(requested))
	for n := range requested {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, sn := range names {
		if !specSkillName(sn) {
			return fmt.Errorf("pack: invalid skill name %q", sn)
		}
		var got struct {
			Skill store.Skill `json:"skill"`
		}
		if err := api.call(ctx, http.MethodGet, "/api/v1/skills/"+sn, nil, &got); err != nil {
			return fmt.Errorf("pack: skill %q is unavailable in the library: %w", sn, err)
		}
		k := got.Skill
		if k.Name != sn || k.Version < 1 || store.SkillDigest(k.Content, k.Files) != k.SHA256 {
			return fmt.Errorf("pack: skill %q has inconsistent version data", sn)
		}
		base := "skills/" + sn
		content := []byte(k.Content)
		if regexp.MustCompile(`(?im)^allowed-tools\s*:`).Match(content) {
			return fmt.Errorf("pack: skill %q declares allowed-tools, which could widen the exported agent", sn)
		}
		if claudeproto.ScrubCredentials(string(content)) != string(content) {
			return fmt.Errorf("pack: skill %q contains a credential-shaped value; remove it before exporting", sn)
		}
		files = append(files, packFile{base + "/SKILL.md", content})
		fsDigests := []packDigest{{Path: base + "/SKILL.md", SHA256: digest(content)}}
		for _, f := range k.Files {
			if claudeproto.ScrubCredentials(string(f.Content)) != string(f.Content) {
				return fmt.Errorf("pack: skill %q file %q contains a credential-shaped value; remove it before exporting", sn, f.Path)
			}
			p, err := safeRelative(f.Path)
			if err != nil {
				return fmt.Errorf("pack: skill %q: %w", sn, err)
			}
			p = base + "/" + p
			files = append(files, packFile{p, append([]byte(nil), f.Content...)})
			fsDigests = append(fsDigests, packDigest{p, digest(f.Content)})
		}
		sort.Slice(fsDigests, func(i, j int) bool { return fsDigests[i].Path < fsDigests[j].Path })
		skills = append(skills, packSkill{Name: sn, Version: k.Version, SHA256: k.SHA256, Files: fsDigests})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	manifest := packManifest{Format: packFormat, Version: 1, Agent: append(json.RawMessage(nil), row.Spec...), AgentSHA256: digest(row.Spec), Enabled: row.Enabled, Environment: envNames, Skills: skills}
	for _, f := range files {
		manifest.Files = append(manifest.Files, packDigest{f.path, digest(f.data)})
	}
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	mb = append(mb, '\n')
	files = append(files, packFile{"orbit.json", mb})
	if err := publishPack(dest, files); err != nil {
		return err
	}
	if _, err := inspectPack(dest); err != nil {
		return fmt.Errorf("pack: generated pack failed inspection: %w", err)
	}
	fmt.Printf("created Claude Code plugin at %s\n", dest)
	return nil
}

func pluginManifest(name, description string) string {
	b, _ := json.MarshalIndent(map[string]string{"name": "orbit-" + name, "description": description, "version": "1.0.0"}, "", "  ")
	return string(append(b, '\n'))
}

func pluginAgent(name, description, charter string) string {
	// JSON quoting provides a safe single-line YAML scalar for frontmatter.
	d, _ := json.Marshal(description)
	n, _ := json.Marshal(name)
	return "---\nname: " + string(n) + "\ndescription: " + string(d) + "\ntools:\n  - Read\n  - Glob\n  - Grep\n---\n\n" + charter + "\n\nThis is a portable export of an Orbit agent. Review orbit.json for the original Orbit rules and grants. Claude Code does not enforce those Orbit grants.\n"
}

func rejectInlineCredentials(a *spec.Agent) error {
	ref := regexp.MustCompile(`\$\{[A-Z_][A-Z0-9_]*\}`)
	for _, s := range a.Sources {
		if u, err := parseCredentialURL(s.URL); err != nil {
			return fmt.Errorf("pack: source %q: %w", s.Name, err)
		} else if u {
			return fmt.Errorf("pack: source %q URL may contain inline credentials", s.Name)
		}
		for key, value := range s.Headers {
			k := strings.ToLower(key)
			if credentialLabel(k) {
				if !ref.MatchString(value) {
					return fmt.Errorf("pack: source %q header %q must use an environment reference; refusing to export inline credentials", s.Name, key)
				}
			}
		}
		for key, value := range s.Query {
			if credentialLabel(strings.ToLower(key)) && value != "" && !ref.MatchString(value) {
				return fmt.Errorf("pack: source %q query %q must use an environment reference; refusing to export inline credentials", s.Name, key)
			}
		}
		if s.Kind == "lake" {
			for field, value := range map[string]string{"access_key_id": s.AccessKeyID, "secret_access_key": s.SecretAccessKey, "session_token": s.SessionToken} {
				if value != "" && !ref.MatchString(value) {
					return fmt.Errorf("pack: source %q %s must use an environment reference; refusing to export inline credentials", s.Name, field)
				}
			}
		}
	}
	return nil
}

func credentialLabel(s string) bool {
	for _, part := range []string{"auth", "token", "secret", "password", "cookie", "credential", "api-key", "api_key", "apikey", "access-key", "access_key"} {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

func parseCredentialURL(raw string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	// Reject URL userinfo and secret-looking query values without exporting them.
	if strings.Contains(raw, "@") && strings.Contains(raw, "://") {
		u := strings.SplitN(raw, "://", 2)[1]
		if strings.Contains(strings.SplitN(u, "/", 2)[0], "@") {
			return true, nil
		}
	}
	low := strings.ToLower(raw)
	for _, key := range []string{"token=", "secret=", "password=", "api_key=", "apikey=", "access_key=", "authorization=", "credential=", "cookie="} {
		if i := strings.Index(low, key); i >= 0 {
			v := raw[i+len(key):]
			if j := strings.IndexAny(v, "&#"); j >= 0 {
				v = v[:j]
			}
			if v != "" && !strings.Contains(v, "${") {
				return true, nil
			}
		}
	}
	return false, nil
}

func inspectPack(root string) (packManifest, error) {
	var m packManifest
	// The operator supplies the inspection root; this checks that exact selected path before any reads.
	st, err := os.Lstat(root) //nolint:gosec // root is the directory the operator explicitly asked to inspect.
	if err != nil {
		return m, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return m, errors.New("pack inspect: root must be a real directory")
	}
	mb, err := os.ReadFile(filepath.Join(root, "orbit.json")) //nolint:gosec // fixed file beneath the user-selected inspection root.
	if err != nil {
		return m, err
	}
	if err := jsonstrict.ValidateJSON(mb, 4<<20); err != nil {
		return m, fmt.Errorf("pack inspect: invalid orbit.json: %w", err)
	}
	d := json.NewDecoder(strings.NewReader(string(mb)))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("pack inspect: %w", err)
	}
	if m.Format != packFormat || m.Version != 1 {
		return m, errors.New("pack inspect: unsupported format or version")
	}
	if err := jsonstrict.ValidateJSON(m.Agent, 256<<10); err != nil {
		return m, fmt.Errorf("pack inspect: invalid embedded agent spec: %w", err)
	}
	if digest(m.Agent) != m.AgentSHA256 {
		return m, errors.New("pack inspect: agent spec digest mismatch")
	}
	var a spec.Agent
	if err := json.Unmarshal(m.Agent, &a); err != nil {
		return m, err
	}
	if a.Name == "" {
		return m, errors.New("pack inspect: agent name is empty")
	}
	if err := rejectInlineCredentials(&a); err != nil {
		return m, err
	}
	if claudeproto.ScrubCredentials(string(m.Agent)) != string(m.Agent) {
		return m, errors.New("pack inspect: agent spec contains a credential-shaped value")
	}
	refs := []string{}
	for _, s := range a.Sources {
		refs = append(refs, s.EnvRefs()...)
	}
	slices.Sort(refs)
	refs = slices.Compact(refs)
	if !slices.Equal(refs, m.Environment) {
		return m, errors.New("pack inspect: environment variable list does not match agent spec")
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		p, e := safeRelative(f.Path)
		if e != nil || p != f.Path || seen[p] || p == "orbit.json" {
			return m, fmt.Errorf("pack inspect: unsafe or duplicate file path %q", f.Path)
		}
		seen[p] = true
		if !allowedPackPath(p, m.Agent) {
			return m, fmt.Errorf("pack inspect: unsupported plugin component %q", p)
		}
		b, e := readPackFile(root, p)
		if e != nil {
			return m, e
		}
		if digest(b) != f.SHA256 {
			return m, fmt.Errorf("pack inspect: digest mismatch for %s", p)
		}
		if claudeproto.ScrubCredentials(string(b)) != string(b) {
			return m, fmt.Errorf("pack inspect: credential-shaped value in %s", p)
		}
	}
	actual, err := walkPack(root)
	if err != nil {
		return m, err
	}
	sort.Strings(actual)
	expected := make([]string, 0, len(seen))
	for p := range seen {
		expected = append(expected, p)
	}
	sort.Strings(expected)
	if !slices.Equal(actual, expected) {
		return m, errors.New("pack inspect: file inventory differs from manifest")
	}
	if !seen[".claude-plugin/plugin.json"] || !seen["agents/"+a.Name+".md"] {
		return m, errors.New("pack inspect: plugin manifest or agent file is missing")
	}
	if stringMustRead(root, ".claude-plugin/plugin.json") != pluginManifest(a.Name, a.Description) || stringMustRead(root, "agents/"+a.Name+".md") != pluginAgent(a.Name, a.Description, a.Charter) || stringMustRead(root, "README.md") != packReadme(a.Name) {
		return m, errors.New("pack inspect: generated plugin files do not match the manifest")
	}
	seenSkills := map[string]bool{}
	listedSkillFiles := map[string]bool{}
	for _, s := range m.Skills {
		if !specSkillName(s.Name) || s.Version <= 0 || seenSkills[s.Name] {
			return m, fmt.Errorf("pack inspect: invalid or duplicate skill entry %q", s.Name)
		}
		seenSkills[s.Name] = true
		var content string
		var skillFiles []store.SkillFile
		for _, f := range s.Files {
			prefix := "skills/" + s.Name + "/"
			if !strings.HasPrefix(f.Path, prefix) || !seen[f.Path] || digestMustRead(root, f.Path) != f.SHA256 {
				return m, fmt.Errorf("pack inspect: skill file digest mismatch for %s", f.Path)
			}
			if listedSkillFiles[f.Path] {
				return m, fmt.Errorf("pack inspect: duplicate skill file %s", f.Path)
			}
			listedSkillFiles[f.Path] = true
			b, e := readPackFile(root, f.Path)
			if e != nil {
				return m, e
			}
			if f.Path == prefix+"SKILL.md" {
				content = string(b)
			} else {
				skillFiles = append(skillFiles, store.SkillFile{Path: strings.TrimPrefix(f.Path, prefix), Content: b})
			}
		}
		if content == "" || store.SkillDigest(content, skillFiles) != s.SHA256 {
			return m, fmt.Errorf("pack inspect: skill version digest mismatch for %s", s.Name)
		}
		if regexp.MustCompile(`(?im)^allowed-tools\s*:`).MatchString(content) {
			return m, fmt.Errorf("pack inspect: skill %q declares allowed-tools, which could widen the exported agent", s.Name)
		}
	}
	for p := range seen {
		if strings.HasPrefix(p, "skills/") && !listedSkillFiles[p] {
			return m, fmt.Errorf("pack inspect: unlisted skill file %s", p)
		}
	}
	return m, nil
}

func allowedPackPath(p string, raw json.RawMessage) bool {
	var a spec.Agent
	if json.Unmarshal(raw, &a) != nil {
		return false
	}
	if p == ".claude-plugin/plugin.json" || p == "README.md" || p == "agents/"+a.Name+".md" {
		return true
	}
	if !strings.HasPrefix(p, "skills/") {
		return false
	}
	parts := strings.Split(p, "/")
	if len(parts) < 3 || !specSkillName(parts[1]) {
		return false
	}
	for _, part := range parts[2:] {
		if part == "" || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

func packReadme(name string) string {
	return fmt.Sprintf(`# Orbit agent: %s

This directory is a portable export of an Orbit agent and its active library skills.

## Install and use

From this directory, run:

	claude --plugin-dir .

The plugin provides the %s subagent. Review its instructions and the Orbit rules in orbit.json before use.

## Scope and safety

Orbit grants, sources, connectors, schedules, and action rules are recorded for review in orbit.json. Claude Code does not enforce Orbit grants. This plugin uses only the read tools listed in its agent definition; it does not auto-connect to Orbit MCP or copy Orbit hooks. It does not contain Orbit credentials, memory, or account state. The environment variable names in orbit.json identify configuration requirements; values must be supplied separately to the system that consumes them.
`, name, name)
}

func safeRelative(p string) (string, error) {
	if p == "" || strings.Contains(p, "\\") || filepath.IsAbs(p) || strings.HasPrefix(p, "/") || regexp.MustCompile(`^[A-Za-z]:`).MatchString(p) {
		return "", fmt.Errorf("unsafe path %q", p)
	}
	clean := filepath.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.ToSlash(clean) != p {
		return "", fmt.Errorf("unsafe path %q", p)
	}
	return p, nil
}
func readPackFile(root, p string) ([]byte, error) {
	if _, err := safeRelative(p); err != nil {
		return nil, err
	}
	cur := root
	for _, part := range strings.Split(filepath.FromSlash(p), string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur) //nolint:gosec // p was checked as a relative path and each symlink component is rejected.
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("pack inspect: symlink is forbidden: %s", p)
		}
	}
	return os.ReadFile(cur) //nolint:gosec // p was checked as a relative path and its components were inspected for symlinks.
}
func walkPack(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == root {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("pack inspect: symlink is forbidden: %s", p)
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if rel != "orbit.json" {
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}
func publishPack(dest string, files []packFile) error {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(abs); err == nil {
		return fmt.Errorf("pack: output already exists: %s", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(abs)
	lock, err := os.OpenFile(filepath.Join(parent, ".orbit-pack.lock"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("pack: cannot reserve output parent: %w", err)
	}
	defer func() { _ = lock.Close(); _ = os.Remove(lock.Name()) }()
	if _, err = os.Lstat(abs); err == nil {
		return fmt.Errorf("pack: output already exists: %s", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".orbit-pack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, f := range files {
		p, err := safeRelative(f.path)
		if err != nil {
			return err
		}
		target := filepath.Join(stage, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(target, f.data, 0600); err != nil {
			return err
		}
	}
	if _, err := inspectPack(stage); err != nil {
		return fmt.Errorf("pack: generated pack failed inspection: %w", err)
	}
	if err := os.Rename(stage, abs); err != nil {
		return err
	}
	return nil
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func digestMustRead(root, p string) string {
	b, e := readPackFile(root, p)
	if e != nil {
		return ""
	}
	return digest(b)
}
func stringMustRead(root, p string) string {
	b, err := readPackFile(root, p)
	if err != nil {
		return ""
	}
	return string(b)
}
func agentName(raw []byte) string { var a spec.Agent; _ = json.Unmarshal(raw, &a); return a.Name }
func specSkillName(s string) bool { return regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`).MatchString(s) }
