package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

// A session's transcript is stored in Postgres (session_transcripts), not
// in Garage: every host that runs agents already holds the database
// credential and nothing else is needed, a transcript is saved before its
// run is recorded as succeeded so the next claim anywhere finds it, and it
// is backed up with every other row by the nightly dump. A transcript is
// a few hundred KB to a few MB of JSONL and compresses about tenfold.

// MaxSessionArchive bounds one stored transcript, compressed. Past it the
// session stays on its own host only, and a run claimed elsewhere starts
// fresh (saying why); a session that large is near maxSessionDepth anyway.
const MaxSessionArchive = 64 << 20

// maxSessionBytes bounds what a restore writes, uncompressed: the archive
// is orbit's own, but a bound keeps a corrupt row from filling the volume.
const maxSessionBytes = 1 << 30

// sessionFiles is what Claude Code keeps of one session in its projects
// directory: <id>.jsonl, and <id>/ (subagent transcripts, large tool
// results) when there is one. Paths are relative to the directory.
func sessionFiles(dir, id string) ([]string, error) {
	files := []string{id + ".jsonl"}
	if _, err := os.Stat(filepath.Join(dir, files[0])); err != nil {
		return nil, err
	}
	sub := filepath.Join(dir, id)
	err := filepath.WalkDir(sub, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == sub {
			return fs.SkipDir
		}
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files, err
}

// archiveSession packs a session's files, or returns ok false when the
// transcript is not on this host.
func archiveSession(home, workdir, id string) (archive []byte, size int64, ok bool, err error) {
	p, found := runner.SessionFile(home, workdir, id)
	if !found {
		return nil, 0, false, nil
	}
	dir := filepath.Dir(p)
	files, err := sessionFiles(dir, id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, name := range files {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))) //nolint:gosec // orbit's own session directory
		if err != nil {
			return nil, 0, false, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return nil, 0, false, err
		}
		if _, err := tw.Write(b); err != nil {
			return nil, 0, false, err
		}
		size += int64(len(b))
	}
	if err := tw.Close(); err != nil {
		return nil, 0, false, err
	}
	if err := zw.Close(); err != nil {
		return nil, 0, false, err
	}
	return buf.Bytes(), size, true, nil
}

// unpackSession writes an archive back under dir. Only regular files named
// for the session are accepted.
func unpackSession(archive []byte, dir, id string) error {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || (name != id+".jsonl" && !strings.HasPrefix(name, id+"/")) || strings.Contains(name, "..") {
			return fmt.Errorf("stored session %s: unexpected entry %q", id, h.Name)
		}
		total += h.Size
		if total > maxSessionBytes {
			return fmt.Errorf("stored session %s: larger than %d bytes", id, maxSessionBytes)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // name checked above
		if err != nil {
			return err
		}
		_, cerr := io.CopyN(f, tr, h.Size)
		if err := errors.Join(cerr, f.Close()); err != nil {
			return err
		}
	}
}

// saveSession stores a session's transcript centrally, when the engine
// keeps a session store. A failure is logged, not fatal: the session is
// still on this host, and a run claimed elsewhere starts fresh.
func (e *Engine) saveSession(ctx context.Context, c *store.ClaimedRun, home, workdir, id string) {
	if !e.SessionStore || id == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	archive, size, ok, err := archiveSession(home, workdir, id)
	switch {
	case err != nil:
		e.Log.Warn("store session", "run", c.ID, "session", id, "err", err)
		return
	case !ok:
		return
	case len(archive) > MaxSessionArchive:
		e.Log.Warn("session too large to store centrally; it continues on this host only", "run", c.ID, "session", id, "bytes", len(archive))
		return
	}
	if err := e.Store.PutSessionTranscript(ctx, store.SessionTranscript{SessionID: id, Agent: c.Agent, RunID: c.ID,
		Attempt: c.Attempt, Worker: e.Worker, Archive: archive, Bytes: size}); err != nil {
		e.Log.Warn("store session", "run", c.ID, "session", id, "err", err)
	}
}

// ensureSession makes a session's transcript present on this host:
// already here, or written back from the session store. It reports
// whether the transcript is now on disk.
func (e *Engine) ensureSession(ctx context.Context, home, workdir, id string) (bool, error) {
	p, ok := runner.SessionFile(home, workdir, id)
	if !ok {
		return false, nil
	}
	if _, err := os.Stat(p); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if !e.SessionStore {
		return false, nil
	}
	archive, err := e.Store.GetSessionTranscript(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := unpackSession(archive, filepath.Dir(p), id); err != nil {
		return false, err
	}
	return true, nil
}
