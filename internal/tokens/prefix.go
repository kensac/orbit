package tokens

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// Prefix is what a request sends before the conversation, as digests: the
// part of every backend call the prompt cache can reuse from the call
// before it. The API caches in order tools, system, messages, so when any
// part here differs from the conversation's previous run, everything after
// it (the whole session transcript on a resume) is written to the cache
// again instead of read from it.
type Prefix struct {
	// SHA is over every part but the charter.
	SHA string
	// Parts is each part's digest, by name (PrefixParts).
	Parts map[string]string
}

// PrefixParts names the parts of a Prefix, in the order they are hashed.
// charter leads a fresh session's first message, so it is part of what a
// fresh run's cache can reuse, but a resumed session sends it after the
// transcript, where it busts nothing; it is recorded and left out of SHA.
var PrefixParts = []string{"model", "tools", "mcp", "schema", "skills", "system", "workdir", "charter"}

// NewPrefix digests a request's prefix parts by name (PrefixParts); a part absent from values is the empty string.
func NewPrefix(values map[string]string) Prefix {
	p := Prefix{Parts: map[string]string{}}
	all := sha256.New()
	for _, name := range PrefixParts {
		d := digest(values[name])
		p.Parts[name] = d
		if name != "charter" {
			all.Write([]byte(name + "=" + d + "\n"))
		}
	}
	p.SHA = hex.EncodeToString(all.Sum(nil))
	return p
}

// ChangedParts lists the parts whose digests differ between two prefixes'
// parts, in PrefixParts order; a part only one of them has counts as changed.
func ChangedParts(prev, cur map[string]string) []string {
	changed := []string{}
	seen := map[string]bool{}
	for _, name := range PrefixParts {
		seen[name] = true
		if prev[name] != cur[name] {
			changed = append(changed, name)
		}
	}
	var extra []string
	for name := range cur {
		if !seen[name] && prev[name] != cur[name] {
			extra = append(extra, name)
		}
	}
	for name := range prev {
		if _, ok := cur[name]; !ok && !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	return append(changed, extra...)
}

// digest is a part's short digest: 16 hex digits of its SHA-256, enough to
// tell versions of one part apart.
func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}
