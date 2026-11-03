package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kensac/orbit/internal/claudeimport"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

// A session's transcript is stored in Postgres (session_transcripts), not in Garage: every host that runs agents already holds the database credential and nothing else is needed, a transcript is saved before its run is recorded as succeeded so the next claim anywhere finds it, and it is backed up with every other row by the nightly dump. A transcript is a few hundred KB to a few MB of JSONL and compresses about tenfold.

// MaxSessionArchive bounds one stored transcript, compressed. Past it the session stays on its own host only, and a run claimed elsewhere starts fresh (saying why); a session that large is near maxSessionDepth anyway.
const MaxSessionArchive = 64 << 20

// maxSessionBytes bounds what a restore writes, uncompressed: the archive is orbit's own, but a bound keeps a corrupt row from filling the volume.
const maxSessionBytes = 1 << 30

// sessionFiles is what Claude Code keeps of one session in its projects directory: <id>.jsonl, and <id>/ (subagent transcripts, large tool results) when there is one. Paths are relative to the directory.
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

// archiveSession packs a session's files, or returns ok false when the transcript is not on this host.
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
	contents := make([][]byte, len(files))
	for i, name := range files {
		if contents[i], err = os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))); err != nil { //nolint:gosec // orbit's own session directory
			return nil, 0, false, err
		}
	}
	archive, size, err = packSession(files, contents)
	return archive, size, err == nil, err
}

// packSession is a stored transcript: a gzipped tar of the session's files (names relative to the projects directory), as unpackSession reads it.
func packSession(names []string, contents [][]byte) ([]byte, int64, error) {
	var buf bytes.Buffer
	var size int64
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for i, name := range names {
		b := contents[i]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return nil, 0, err
		}
		if _, err := tw.Write(b); err != nil {
			return nil, 0, err
		}
		size += int64(len(b))
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), size, nil
}

// ErrSeedTooLarge is an imported session whose transcript is over MaxSessionArchive compressed: no host could be handed it.
var ErrSeedTooLarge = errors.New("the session's transcript is too large to continue on the server")

// SessionSeed builds the transcript a conversation started from an imported session resumes (POST /conversations with from_session): the session's current lines, each with its sessionId set to a session id of orbit's own. That id is derived from the session and its content hash, so the same snapshot always gives the same id (stored once) and a session that grew gives a new one; the person's own id is never reused, so nothing orbit writes can be taken for the laptop's file.
func (e *Engine) SessionSeed(ctx context.Context, ref string) (*store.SessionSeed, *store.ExternalSession, error) {
	x, err := e.Store.FindExternalSession(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	lines, err := e.Store.ExternalSessionLines(ctx, x.ID, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	if len(lines) == 0 {
		return nil, nil, fmt.Errorf("%w: the session has no lines", store.ErrInvalid)
	}
	id := seedSessionID(x.ID, x.ContentSHA256)
	var jsonl bytes.Buffer
	for _, l := range lines {
		b, err := claudeimport.RestoreLine([]byte(l.Content), id)
		if err != nil {
			return nil, nil, fmt.Errorf("session %s line %d: %w", x.ID, l.N, err)
		}
		jsonl.Write(b)
		jsonl.WriteByte('\n')
	}
	archive, size, err := packSession([]string{id + ".jsonl"}, [][]byte{jsonl.Bytes()})
	if err != nil {
		return nil, nil, err
	}
	if len(archive) > MaxSessionArchive {
		return nil, nil, fmt.Errorf("%w (%d bytes compressed, at most %d)", ErrSeedTooLarge, len(archive), MaxSessionArchive)
	}
	title := ""
	switch {
	case x.Title != nil:
		title = *x.Title
	case x.FirstPrompt != nil:
		title = *x.FirstPrompt
	}
	return &store.SessionSeed{ExternalID: x.ID, SessionID: id, Title: title, Archive: archive, Bytes: size}, x, nil
}

// seedSessionID is a UUID (version 8, RFC 9562's custom layout) from the SHA-256 of an imported session's id and content hash.
func seedSessionID(externalID, content string) string {
	h := sha256.Sum256([]byte("orbit-seed\x00" + externalID + "\x00" + content))
	h[6] = h[6]&0x0f | 0x80
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// unpackSession writes an archive back under dir. Only regular files named for the session are accepted.
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

// saveSession stores a session's transcript centrally, when the engine keeps a session store. A failure is logged, not fatal: the session is still on this host, and a run claimed elsewhere starts fresh.
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

// ensureSession makes a session's transcript present on this host: already here, or written back from the session store. It reports whether the transcript is now on disk.
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
	return e.restoreSession(ctx, p, id)
}

// ensureSeed makes an imported session's transcript present on this host. It is in session_transcripts whatever SessionStore says: it was never on any host's disk.
func (e *Engine) ensureSeed(ctx context.Context, home, workdir, id string) (bool, error) {
	p, ok := runner.SessionFile(home, workdir, id)
	if !ok {
		return false, nil
	}
	if _, err := os.Stat(p); err == nil {
		return true, nil
	}
	return e.restoreSession(ctx, p, id)
}

// restoreSession writes a stored transcript back at p, the session's path on this host, reporting false when the store has none.
func (e *Engine) restoreSession(ctx context.Context, p, id string) (bool, error) {
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
