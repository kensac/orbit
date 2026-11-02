package sandbox

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFramesRoundTrip(t *testing.T) {
	var b bytes.Buffer
	big := bytes.Repeat([]byte("x"), 3*frameChunk+5)
	if err := WriteFrame(&b, FrameStdout, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := WriteChunked(&b, FrameStderr, big); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&b, FrameExit, []byte(`{"exit_code":0}`)); err != nil {
		t.Fatal(err)
	}
	typ, p, err := ReadFrame(&b)
	if err != nil || typ != FrameStdout || string(p) != "hello" {
		t.Fatalf("first frame: %d %q %v", typ, p, err)
	}
	var got []byte
	frames := 0
	for {
		typ, p, err = ReadFrame(&b)
		if err != nil {
			t.Fatal(err)
		}
		if typ != FrameStderr {
			break
		}
		if len(p) > frameChunk {
			t.Fatalf("chunk of %d bytes", len(p))
		}
		got = append(got, p...)
		frames++
	}
	if !bytes.Equal(got, big) || frames != 4 {
		t.Fatalf("chunked payload: %d bytes in %d frames", len(got), frames)
	}
	if typ != FrameExit {
		t.Fatalf("last frame type %d", typ)
	}
	if _, _, err := ReadFrame(&b); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream: %v", err)
	}
}

func TestReadFrameRejectsMalformed(t *testing.T) {
	for name, in := range map[string][]byte{
		"nonzero padding": {1, 0, 1, 0, 0, 0, 0, 0},
		"oversized":       {1, 0, 0, 0, 0x7f, 0, 0, 0},
		"truncated":       {1, 0, 0, 0, 0, 0, 0, 5, 'a'},
	} {
		if _, _, err := ReadFrame(bytes.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := WriteFrame(io.Discard, FrameStdout, make([]byte, MaxFrame+1)); err == nil {
		t.Error("an oversized frame was written")
	}
}

func TestAttachRequestValidate(t *testing.T) {
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		ok := AttachRequest{Argv: []string{"claude", "-p"}, Env: map[string]string{name: "t"}}
		if err := ok.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	many := make([]string, MaxArgs+1)
	for i := range many {
		many[i] = "a"
	}
	for name, r := range map[string]AttachRequest{
		"empty argv":       {},
		"empty argv0":      {Argv: []string{""}},
		"too many args":    {Argv: many},
		"too long":         {Argv: []string{"x", strings.Repeat("a", MaxArgvBytes)}},
		"NUL in argv":      {Argv: []string{"sh", "a\x00b"}},
		"other env":        {Argv: []string{"x"}, Env: map[string]string{"GITHUB_TOKEN": "t"}},
		"both credentials": {Argv: []string{"x"}, Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "t", "ANTHROPIC_API_KEY": "k"}},
		"other auth env":   {Argv: []string{"x"}, Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "t"}},
		"path env":         {Argv: []string{"x"}, Env: map[string]string{"PATH": "/evil"}},
		"NUL in env":       {Argv: []string{"x"}, Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "a\x00"}},
		"oversized value":  {Argv: []string{"x"}, Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": strings.Repeat("a", MaxEnvValue+1)}},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestIDs(t *testing.T) {
	if !ValidID(strings.Repeat("ab", 32)) || ValidID("abc") || ValidID(strings.Repeat("AB", 32)) || ValidID("../"+strings.Repeat("a", 61)) {
		t.Fatal("ValidID")
	}
	if !ValidRunID("3f2a9c1e-0000-4000-8000-000000000000") || ValidRunID("") || ValidRunID("a/b") || ValidRunID(strings.Repeat("a", 65)) {
		t.Fatal("ValidRunID")
	}
}

func TestNewClientAddresses(t *testing.T) {
	if _, err := NewClient("unix:/run/sandboxd.sock", "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient("100.123.116.85:8091", "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient("", "t"); err == nil {
		t.Fatal("empty address accepted")
	}
	if _, err := NewClient("unix:/x", ""); err == nil {
		t.Fatal("empty token accepted")
	}
}
