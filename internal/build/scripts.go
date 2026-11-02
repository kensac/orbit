package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kensac/orbit/internal/sandbox"
)

// ScriptsFile is where a repository names its build scripts.
const ScriptsFile = ".orbit/build.yaml"

// Scripts are a repository's own build scripts, run in the sandbox by
// orbit, not by the model: setup after the checkout is unpacked and before
// the CLI starts (fetch modules, generate files), cleanup after the CLI
// finished and before orbit reads the tree back (remove what a build
// leaves that is not the change). Each runs only when its exact text is
// one a person reviewed: its sha256 is in the repository's
// trusted_scripts. Anything else fails the build before it runs, so a
// change to a script, by a person or by an earlier build on the PR's
// branch, waits for a person to read it.
type Scripts struct {
	Setup   string `yaml:"setup"`
	Cleanup string `yaml:"cleanup"`
}

// maxScriptsFile bounds .orbit/build.yaml: two short scripts.
const maxScriptsFile = 64 << 10

// scriptTimeout bounds one script: it runs inside the build's own timeout,
// and a setup that needs longer is a job for the image.
const scriptTimeout = 10 * time.Minute

// ScriptHash is the sha256 of a script's exact text, as trusted_scripts
// names it.
func ScriptHash(script string) string {
	sum := sha256.Sum256([]byte(script))
	return hex.EncodeToString(sum[:])
}

// readScripts reads the checkout's ScriptsFile with code: missing is no
// scripts; anything but a regular file, an unknown field or a file over
// the bound fails.
func readScripts(dir string) (Scripts, error) {
	var s Scripts
	p := filepath.Join(dir, filepath.FromSlash(ScriptsFile))
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxScriptsFile {
		return s, fmt.Errorf("%s must be a regular file of at most %d bytes", ScriptsFile, maxScriptsFile)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return s, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return s, fmt.Errorf("%s: %w", ScriptsFile, err)
	}
	return s, nil
}

// trusted fails unless every script present hashes to a trusted value,
// naming the hash a person would add after reading it.
func (s Scripts) trusted(repo string, hashes []string) error {
	for _, sc := range []struct{ name, text string }{{"setup", s.Setup}, {"cleanup", s.Cleanup}} {
		if sc.text == "" {
			continue
		}
		if h := ScriptHash(sc.text); !slices.Contains(hashes, h) {
			return fmt.Errorf("%w: the %s script in %s (sha256 %s) is not trusted; read it, then add the hash to build.repositories.%s.trusted_scripts",
				ErrPolicy, sc.name, ScriptsFile, h, repo)
		}
	}
	return nil
}

// runScript runs one trusted script in the sandbox with sh -c, with no
// environment beyond sandboxd's fixed one: not even the backend
// credential, which a script never needs.
func runScript(ctx context.Context, sb Sandbox, id, name, script string, d Deps) error {
	sctx, cancel := context.WithTimeout(ctx, scriptTimeout)
	defer cancel()
	out := &scriptTail{}
	start := time.Now()
	code, err := sb.Attach(sctx, id, sandbox.AttachRequest{Argv: []string{"/bin/sh", "-c", script}}, bytes.NewReader(nil), out, out)
	ev := map[string]any{"step": name, "sha256": ScriptHash(script), "exit": code, "ms": time.Since(start).Milliseconds(), "output": string(out.b)}
	if err != nil {
		ev["error"] = err.Error()
	}
	d.emit("orbit.build", ev)
	switch {
	case errors.Is(sctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("the %s script timed out after %s", name, scriptTimeout)
	case err != nil:
		return fmt.Errorf("the %s script: %w", name, err)
	case code != 0:
		return fmt.Errorf("the %s script exited %d: %s", name, code, lastLine(string(out.b)))
	}
	return nil
}

// scriptTail keeps the last scriptTailBytes a script wrote to stdout and
// stderr together: the end, where a failure is.
type scriptTail struct {
	mu sync.Mutex
	b  []byte
}

// scriptTailBytes bounds what a script's event keeps.
const scriptTailBytes = 8 << 10

func (t *scriptTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > scriptTailBytes {
		t.b = append(t.b[:0], t.b[len(t.b)-scriptTailBytes:]...)
	}
	return len(p), nil
}
