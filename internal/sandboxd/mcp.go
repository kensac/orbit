package sandboxd

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/kensac/orbit/internal/sandbox"
)

// Connectors, MCP servers and skills in a box (docs/workspace.md,
// Connectors and MCP). Each is a fixed script; an account's runs with its
// directory as CLAUDE_CONFIG_DIR and without the subscription token, as
// its terminals do.

// accountEnv is the prologue of a script that runs claude as an account.
const accountEnv = `d=$HOME/.claude-accounts/$1
[ -d "$d" ] || { echo "no directory $d: the box does not mount this account" >&2; exit 3; }
export CLAUDE_CONFIG_DIR=$d
unset CLAUDE_CODE_OAUTH_TOKEN
for v in $(env | sed -n 's/^\(ANTHROPIC_[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$v"; done
cd "$HOME"
`

// mcpListScript prints `claude mcp list` for an account (its user servers,
// its claude.ai connectors, the shared plugins'), each health-checked.
const mcpListScript = accountEnv + `exec timeout 50 claude mcp list`

// mcpSyncScript mirrors the servers orbit manages, a JSON object of name to
// config on stdin, into the account's user scope: a server orbit added that
// is gone or changed is removed, and a new or changed one added with
// `claude mcp add-json -s user`. What orbit added is recorded in
// .orbit/mcp-managed.json, so a server the person added is never touched
// unless orbit manages one of that name. Names were checked by sandboxd.
const mcpSyncScript = accountEnv + `set -u
want=$(cat)
m=$d/.orbit/mcp-managed.json
mkdir -p "$d/.orbit"
old='{}'
[ -s "$m" ] && old=$(cat "$m")
for n in $(printf '%s' "$old" | jq -r --argjson w "$want" 'to_entries[] | select($w[.key] != .value) | .key'); do
	timeout 30 claude mcp remove -s user "$n" >/dev/null 2>&1 || true
done
fail=0
for n in $(printf '%s' "$want" | jq -r --argjson o "$old" 'to_entries[] | select($o[.key] != .value) | .key'); do
	timeout 30 claude mcp remove -s user "$n" >/dev/null 2>&1 || true
	if ! timeout 30 claude mcp add-json -s user "$n" "$(printf '%s' "$want" | jq -c --arg n "$n" '.[$n]')" >/dev/null; then
		echo "adding $n failed" >&2
		fail=1
	fi
done
[ "$fail" = 0 ] || exit 1
printf '%s' "$want" >"$m.new" && mv -f "$m.new" "$m"`

// skillsScript puts orbit's skills library into the shared profile
// (~/.claude/skills, linked into every account): a tar of <skill>/<file>
// on stdin, extracted aside and moved in. The skills orbit put there
// before (.orbit-managed) are replaced; a directory of the same name that
// is the person's own is left, and that skill skipped.
const skillsScript = `set -eu
s=$HOME/.claude/skills
mkdir -p "$s"
new=$(mktemp -d "$HOME/.claude/.orbit-skills.XXXXXX")
trap 'rm -rf "$new"' EXIT
tar -x -C "$new" --no-same-owner --no-same-permissions -f -
m=$s/.orbit-managed
[ -f "$m" ] || : >"$m"
while IFS= read -r n; do
	case $n in "" | */* | .*) continue ;; esac
	rm -rf "${s:?}/$n"
done <"$m"
: >"$m.new"
for p in "$new"/*/; do
	[ -d "$p" ] || continue
	n=$(basename "$p")
	if [ -e "$s/$n" ]; then
		echo "skipped $n: a skill of that name is the person's own" >&2
		continue
	fi
	mv "$p" "$s/$n"
	echo "$n" >>"$m.new"
done
mv -f "$m.new" "$m"`

func (s *Server) accountMCP(w http.ResponseWriter, r *http.Request) {
	c, _, slot, ok := s.accountBox(w, r)
	if !ok {
		return
	}
	stdout, _, code, err := s.boxRun(r.Context(), c.ID, []string{"sh", "-c", mcpListScript, "orbit-account", slot}, nil, nil)
	switch {
	case err != nil:
		writeError(w, http.StatusBadGateway, "mcp list failed")
		return
	case code == exitNoAccountDir:
		writeError(w, http.StatusConflict, "the account directory is unavailable")
		return
	case code != 0:
		writeError(w, http.StatusBadGateway, fmt.Sprintf("mcp list failed with exit code %d", code))
		return
	}
	servers := sandbox.ParseMCPList(stdout)
	if servers == nil {
		servers = []sandbox.MCPServer{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

func (s *Server) accountMCPSync(w http.ResponseWriter, r *http.Request) {
	c, name, slot, ok := s.accountBox(w, r)
	if !ok {
		return
	}
	var want map[string]json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, sandbox.MaxMCPServers*sandbox.MaxMCPConfig+1024))
	if err := dec.Decode(&want); err != nil {
		writeError(w, http.StatusBadRequest, "mcp servers: "+err.Error())
		return
	}
	if want == nil {
		want = map[string]json.RawMessage{}
	}
	if err := sandbox.ValidateMCPServers(want); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, _ := json.Marshal(want)
	_, stderr, code, err := s.boxRun(r.Context(), c.ID, []string{"sh", "-c", mcpSyncScript, "orbit-account", slot}, nil, body)
	switch {
	case err != nil:
		writeError(w, http.StatusBadGateway, "mcp sync: "+err.Error())
		return
	case code == exitNoAccountDir:
		writeError(w, http.StatusConflict, scriptError("mcp sync", code, stderr))
		return
	case code != 0:
		writeError(w, http.StatusBadGateway, scriptError("mcp sync", code, stderr))
		return
	}
	s.log.Info("sandboxd: account mcp servers synced", "box", name, "account", slot, "servers", len(want))
	w.WriteHeader(http.StatusNoContent)
}

// Bounds on the skills tar, over the store's own per-skill bounds for a
// library of many.
const (
	maxSkillsTar     = 64 << 20
	maxSkillsEntries = 5000
	maxSkillFile     = 4 << 20
)

var skillDirRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// cleanSkillsTar rebuilds the skills tar orbit sent from its regular files
// alone, each under a skill's directory at a clean path, owned by the box's
// user with plain modes, so what the box extracts is exactly a tree of
// files.
func cleanSkillsTar(r io.Reader) ([]byte, int, error) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	tr := tar.NewReader(io.LimitReader(r, maxSkillsTar+1))
	skills := map[string]bool{}
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("skills tar: %w", err)
		}
		if n++; n > maxSkillsEntries {
			return nil, 0, fmt.Errorf("skills tar: more than %d entries", maxSkillsEntries)
		}
		name := strings.TrimSuffix(h.Name, "/")
		dir, rest, _ := strings.Cut(name, "/")
		if !skillDirRE.MatchString(dir) || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
			return nil, 0, fmt.Errorf("skills tar: entry %q is not <skill>/<path>", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, 0, fmt.Errorf("skills tar: %q is not a regular file", h.Name)
		}
		if rest == "" || h.Size > maxSkillFile {
			return nil, 0, fmt.Errorf("skills tar: %q is not a file of at most %d bytes in a skill", h.Name, maxSkillFile)
		}
		mode := int64(0o644)
		if h.Mode&0o111 != 0 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: h.Size, Mode: mode, Uid: 10001, Gid: 10001}); err != nil {
			return nil, 0, err
		}
		if _, err := io.CopyN(tw, tr, h.Size); err != nil {
			return nil, 0, fmt.Errorf("skills tar: %q: %w", h.Name, err)
		}
		skills[dir] = true
	}
	if out.Len() > maxSkillsTar {
		return nil, 0, fmt.Errorf("skills tar: more than %d bytes", maxSkillsTar)
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	return out.Bytes(), len(skills), nil
}

func (s *Server) skillsPut(w http.ResponseWriter, r *http.Request) {
	c, name, ok := s.runningBox(w, r)
	if !ok {
		return
	}
	tb, n, err := cleanSkillsTar(http.MaxBytesReader(w, r.Body, maxSkillsTar+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, stderr, code, err := s.boxRun(r.Context(), c.ID, []string{"sh", "-c", skillsScript, "orbit-skills"}, nil, tb)
	if err == nil && code != 0 {
		err = errors.New(scriptError("skills", code, stderr))
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "skills: "+err.Error())
		return
	}
	if skipped := strings.TrimSpace(string(stderr)); skipped != "" {
		s.log.Warn("sandboxd: skills skipped", "box", name, "detail", truncate(stderr, 500))
	}
	s.log.Info("sandboxd: skills synced", "box", name, "skills", n)
	w.WriteHeader(http.StatusNoContent)
}
