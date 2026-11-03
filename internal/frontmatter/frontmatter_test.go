package frontmatter

import (
	"errors"
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	for name, c := range map[string]struct {
		in, fm, body string
		err          error
	}{
		"header and body":   {"---\nname: a\n---\n\n  body\n", "name: a", "body", nil},
		"byte order mark":   {"\ufeff---\nname: a\n---\nbody", "name: a", "body", nil},
		"crlf":              {"---\r\nname: a\r\n---\r\nbody\r\n", "name: a", "body", nil},
		"header only":       {"---\nname: a\n---", "name: a", "", nil},
		"no header":         {"# Title\n---\n", "", "", ErrNone},
		"dashes not a line": {"----\nx\n", "", "", ErrNone},
	} {
		fm, body, err := Split([]byte(c.in))
		if !errors.Is(err, c.err) || string(fm) != c.fm || body != c.body {
			t.Errorf("%s: Split = %q, %q, %v; want %q, %q, %v", name, fm, body, err, c.fm, c.body, c.err)
		}
	}
	if _, _, err := Split([]byte("---\nname: a\nbody\n")); err == nil || errors.Is(err, ErrNone) {
		t.Fatalf("an unclosed header: err = %v", err)
	}
}

func TestFields(t *testing.T) {
	for name, c := range map[string]struct {
		in   string
		want map[string]string
	}{
		"yaml": {"name: a\ndescription: \"b: c\"\ncount: 3\nmetadata:\n  type: user\n  n: 2\n",
			map[string]string{"name": "a", "description": "b: c", "metadata.type": "user"}},
		// Claude Code writes a value that opens with a quoted phrase and goes on, which is not YAML.
		"not yaml": {"name: a\ndescription: \"Use it\" when asked\nmetadata:\n  type: 'user'\n",
			map[string]string{"name": "a", "description": "\"Use it\" when asked", "metadata.type": "user"}},
		"quoted whole": {"name: \"a\"\nx: \"y\" z\n\tkind: 'k'\n", map[string]string{"name": "a", "x": "\"y\" z", "x.kind": "k"}},
		"empty":        {"", map[string]string{}},
	} {
		if got := Fields([]byte(c.in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Fields = %v, want %v", name, got, c.want)
		}
	}
}
