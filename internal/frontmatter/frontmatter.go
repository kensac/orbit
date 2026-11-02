// Package frontmatter reads the YAML header Claude Code puts at the top of a memory file or a SKILL.md, as leniently as Claude Code does.
package frontmatter

import (
	"bytes"
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNone is a document that does not open with a "---" header.
var ErrNone = errors.New("no frontmatter")

// Split separates a leading "---" block from the rest, which is returned trimmed.
func Split(b []byte) ([]byte, string, error) {
	b = bytes.TrimPrefix(b, []byte("\ufeff"))
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return nil, "", ErrNone
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		if strings.HasSuffix(rest, "\n---") {
			return []byte(rest[:len(rest)-4]), "", nil
		}
		return nil, "", errors.New("frontmatter is not closed")
	}
	return []byte(rest[:end]), strings.TrimSpace(rest[end+5:]), nil
}

// Fields reads a header's top-level string fields, and those under metadata as "metadata.<key>". It parses YAML first and, for a header that is not YAML (Claude Code writes values that open with a quoted phrase), reads "key: value" lines as Claude Code would.
func Fields(fm []byte) map[string]string {
	out := map[string]string{}
	var doc map[string]any
	if yaml.Unmarshal(fm, &doc) == nil {
		for k, v := range doc {
			switch t := v.(type) {
			case string:
				out[k] = t
			case map[string]any:
				for mk, mv := range t {
					if s, ok := mv.(string); ok {
						out[k+"."+mk] = s
					}
				}
			}
		}
		return out
	}
	parent := ""
	for _, line := range strings.Split(string(fm), "\n") {
		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v = unquote(strings.TrimSpace(v))
		if !indented {
			parent = k
			if v != "" {
				out[k] = v
			}
		} else if parent != "" && v != "" {
			out[parent+"."+k] = v
		}
	}
	return out
}

// unquote strips one pair of quotes around a whole value.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}
