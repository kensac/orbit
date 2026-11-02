package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The kinds of a tree entry.
const (
	KindFile    = "file"
	KindDir     = "dir"
	KindSymlink = "symlink"
)

// ErrPath is a path the API refuses: not clean, absolute, or through a
// symlink (docs/api.md, Workspace, check "path").
var ErrPath = errors.New("path")

// CleanPath checks a path a client names: relative to the root, POSIX,
// already clean, no "..", no NUL. "" is the root.
func CleanPath(p string) (string, error) {
	switch {
	case p == "":
		return "", nil
	case strings.ContainsRune(p, 0):
		return "", fmt.Errorf("%w: contains a NUL", ErrPath)
	case strings.HasPrefix(p, "/"):
		return "", fmt.Errorf("%w: %q is absolute; paths are relative to the workspace root", ErrPath, p)
	case path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../"):
		return "", fmt.Errorf("%w: %q is not a clean path inside the tree", ErrPath, p)
	}
	return p, nil
}

// Entry is one path of a snapshot.
type Entry struct {
	Path    string
	Kind    string
	Bytes   int64
	Mode    fs.FileMode
	ModTime time.Time
	// Target is a symlink's link text.
	Target string
	// data is a file's bytes, a slice of the archive.
	data []byte
}

// Name is the entry's base name.
func (e *Entry) Name() string { return path.Base(e.Path) }

// Data is a file's content.
func (e *Entry) Data() []byte { return e.data }

// Version is a file's content address, "sha256:<hex>" of its bytes.
func Version(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Snapshot is a stored workspace tar, indexed for the file routes: every
// path, its children, and file content as slices of the archive (no copy).
// A Snapshot is immutable and safe for concurrent use.
type Snapshot struct {
	entries  map[string]*Entry
	children map[string][]*Entry
	size     int64

	mu       sync.Mutex
	versions map[string]string
	ignores  map[string][]ignoreRule
}

// Open indexes an archive. hide leaves out a path and everything under it
// (orbit's own state). A directory an entry implies but the tar does not
// list is added.
func Open(archive []byte, hide func(name string) bool) (*Snapshot, error) {
	s := &Snapshot{entries: map[string]*Entry{"": {Path: "", Kind: KindDir, Mode: 0o755 | fs.ModeDir}},
		children: map[string][]*Entry{}, size: int64(len(archive)),
		versions: map[string]string{}, ignores: map[string][]ignoreRule{}}
	r := bytes.NewReader(archive)
	tr := tar.NewReader(r)
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the snapshot: %w", err)
		}
		if n > MaxEntries {
			return nil, fmt.Errorf("the snapshot has more than %d entries", MaxEntries)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." || name == ".." || path.IsAbs(name) || strings.HasPrefix(name, "../") || (hide != nil && hide(name)) {
			continue
		}
		e := &Entry{Path: name, Mode: fs.FileMode(h.Mode & 0o777), ModTime: h.ModTime.UTC()}
		switch h.Typeflag {
		case tar.TypeDir:
			e.Kind, e.Mode = KindDir, e.Mode|fs.ModeDir
		case tar.TypeReg:
			// The data starts where the reader is now; Size bytes follow.
			off := int64(len(archive)) - int64(r.Len())
			if off < 0 || off+h.Size > int64(len(archive)) {
				return nil, fmt.Errorf("read the snapshot: %s runs past the archive", name)
			}
			e.Kind, e.Bytes, e.data = KindFile, h.Size, archive[off:off+h.Size:off+h.Size]
		case tar.TypeSymlink:
			e.Kind, e.Target, e.Mode = KindSymlink, h.Linkname, 0o777|fs.ModeSymlink
		default:
			continue
		}
		s.add(e)
	}
	for _, list := range s.children {
		sortEntries(list)
	}
	return s, nil
}

// add puts e in the index, with any parent directory it implies. A later
// entry of the same path replaces an earlier one, as tar extraction does.
func (s *Snapshot) add(e *Entry) {
	for dir := path.Dir(e.Path); ; dir = path.Dir(dir) {
		if dir == "." {
			dir = ""
		}
		if p, ok := s.entries[dir]; ok {
			if p.Kind == KindDir {
				break
			}
			s.unlink(p)
		}
		d := &Entry{Path: dir, Kind: KindDir, Mode: 0o755 | fs.ModeDir, ModTime: e.ModTime}
		s.entries[dir] = d
		s.link(d)
		if dir == "" {
			break
		}
	}
	if old, ok := s.entries[e.Path]; ok {
		s.unlink(old)
	}
	s.entries[e.Path] = e
	s.link(e)
}

func parentOf(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

func (s *Snapshot) link(e *Entry) {
	if e.Path == "" {
		return
	}
	p := parentOf(e.Path)
	s.children[p] = append(s.children[p], e)
}

func (s *Snapshot) unlink(e *Entry) {
	p := parentOf(e.Path)
	list := s.children[p]
	for i, c := range list {
		if c == e {
			s.children[p] = append(list[:i:i], list[i+1:]...)
			return
		}
	}
}

// sortEntries orders a directory's listing: directories first, then by
// name, bytewise.
func sortEntries(list []*Entry) {
	sort.Slice(list, func(i, j int) bool { return entryLess(list[i], list[j]) })
}

func entryLess(a, b *Entry) bool {
	ad, bd := a.Kind == KindDir, b.Kind == KindDir
	if ad != bd {
		return ad
	}
	return a.Name() < b.Name()
}

// Size is the archive's length in bytes.
func (s *Snapshot) Size() int64 { return s.size }

// Lookup finds a path. A missing path is fs.ErrNotExist; a path whose
// parent is a symlink is ErrPath (a symlink is never followed).
func (s *Snapshot) Lookup(p string) (*Entry, error) {
	if e, ok := s.entries[p]; ok {
		return e, nil
	}
	for dir := parentOf(p); dir != ""; dir = parentOf(dir) {
		if e, ok := s.entries[dir]; ok {
			if e.Kind == KindSymlink {
				return nil, fmt.Errorf("%w: %q is through the symlink %q, which is not followed", ErrPath, p, dir)
			}
			break
		}
	}
	return nil, fs.ErrNotExist
}

// Children lists a directory, directories first then by name.
func (s *Snapshot) Children(dir string) []*Entry { return s.children[dir] }

// HasChildren reports whether a directory has any entry.
func (s *Snapshot) HasChildren(dir string) bool { return len(s.children[dir]) > 0 }

// VersionOf is a file's version, computed once.
func (s *Snapshot) VersionOf(e *Entry) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.versions[e.Path]; ok {
		return v
	}
	v := Version(e.data)
	s.versions[e.Path] = v
	return v
}

// Walk visits e and, for a directory, everything under it, parents before
// children.
func (s *Snapshot) Walk(e *Entry, fn func(*Entry) error) error {
	if err := fn(e); err != nil {
		return err
	}
	if e.Kind != KindDir {
		return nil
	}
	for _, c := range s.children[e.Path] {
		if err := s.Walk(c, fn); err != nil {
			return err
		}
	}
	return nil
}

// WriteTarGz writes e (a directory's whole subtree, or one file) as a
// gzipped tar, with paths relative to e's parent so the archive unpacks
// into a directory of e's name (the root's are relative to the root).
func (s *Snapshot) WriteTarGz(w io.Writer, e *Entry) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	base := parentOf(e.Path)
	err := s.Walk(e, func(x *Entry) error {
		if x.Path == "" {
			return nil
		}
		name := strings.TrimPrefix(strings.TrimPrefix(x.Path, base), "/")
		h := &tar.Header{Name: name, ModTime: x.ModTime, Mode: int64(x.Mode.Perm())}
		switch x.Kind {
		case KindDir:
			h.Typeflag, h.Name = tar.TypeDir, name+"/"
		case KindSymlink:
			h.Typeflag, h.Linkname = tar.TypeSymlink, x.Target
		default:
			h.Typeflag, h.Size = tar.TypeReg, x.Bytes
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if x.Kind == KindFile {
			_, err := tw.Write(x.data)
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// binarySniff is how much of a file git reads to call it binary (a NUL in
// its first 8000 bytes, buffer_is_binary in git's xdiff-interface.c).
const binarySniff = 8000

// IsText reports whether b can be edited as text: no NUL in its first 8000
// bytes, as git decides, and valid UTF-8.
func IsText(b []byte) bool {
	if bytes.IndexByte(b[:min(len(b), binarySniff)], 0) >= 0 {
		return false
	}
	return utf8.Valid(b)
}
