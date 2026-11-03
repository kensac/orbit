package sandboxd

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// GitHub in a box (docs/workspace.md, GitHub in the box): orbit writes an
// installation token to /run/orbit/gh-token, where the image's gh wrapper
// and git credential helper read it. The file is the box user's, 0600, in a
// 0700 directory; it lives in the container's root filesystem, so a
// recreated box has none until orbit writes it again.

// ghTokenScript writes its stdin as the token, atomically.
//
//nolint:gosec // G101: a script that writes a token read from stdin, not a credential
const ghTokenScript = `set -eu
d=/run/orbit
if [ ! -d "$d" ] || [ ! -O "$d" ]; then sudo -n install -d -o "$(id -u)" -g "$(id -g)" -m 0700 "$d"; fi
umask 077
t=$(mktemp "$d/.gh-token.XXXXXX")
cat >"$t"
mv -f "$t" "$d/gh-token"`

// ghClearScript removes it, so gh and git use the person's own login.
const ghClearScript = `f=/run/orbit/gh-token
[ -e "$f" ] || exit 0
rm -f "$f" 2>/dev/null || sudo -n rm -f "$f"`

// ghTokenRE is an installation token's shape (ghs_ and base62); anything
// else is refused before it reaches the box.
var ghTokenRE = regexp.MustCompile(`^gh[a-z]_[A-Za-z0-9_]{20,250}$`)

func (s *Server) ghTokenPut(w http.ResponseWriter, r *http.Request) {
	c, name, ok := s.runningBox(w, r)
	if !ok {
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<10))
	tok := strings.TrimSpace(string(b))
	if err != nil || !ghTokenRE.MatchString(tok) {
		writeError(w, http.StatusBadRequest, "the body is a GitHub installation token")
		return
	}
	_, stderr, code, err := s.boxRun(r.Context(), c.ID, []string{"sh", "-c", ghTokenScript, "orbit-gh"}, nil, []byte(tok))
	if err == nil && code != 0 {
		err = errors.New(scriptError("gh-token", code, stderr))
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "gh-token: "+err.Error())
		return
	}
	s.log.Info("sandboxd: gh token written", "box", name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ghTokenDelete(w http.ResponseWriter, r *http.Request) {
	c, name, ok := s.runningBox(w, r)
	if !ok {
		return
	}
	_, stderr, code, err := s.boxRun(r.Context(), c.ID, []string{"sh", "-c", ghClearScript, "orbit-gh"}, nil, nil)
	if err == nil && code != 0 {
		err = errors.New(scriptError("gh-token", code, stderr))
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "gh-token: "+err.Error())
		return
	}
	s.log.Info("sandboxd: gh token removed", "box", name)
	w.WriteHeader(http.StatusNoContent)
}
