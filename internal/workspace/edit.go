package workspace

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// ErrExists is a create, rename or restore whose target is there.
var ErrExists = errors.New("exists")

// ErrTooLarge is an edit that would make the snapshot larger than MaxBytes.
var ErrTooLarge = fmt.Errorf("the workspace would be larger than %d MiB", MaxBytes>>20)

// Edit is a change being made to a snapshot: a copy of its paths that the
// operations below change, packed into a new snapshot at the end. The
// snapshot it started from is not changed.
type Edit struct {
	entries map[string]*Entry
	now     time.Time
}

// Edit starts a change of s.
func (s *Snapshot) Edit(now time.Time) *Edit {
	e := &Edit{entries: make(map[string]*Entry, len(s.entries)), now: now.UTC()}
	for p, x := range s.entries {
		c := *x
		e.entries[p] = &c
	}
	return e
}

// Lookup finds a path as the edit has it, as Snapshot.Lookup does.
func (e *Edit) Lookup(p string) (*Entry, error) {
	if x, ok := e.entries[p]; ok {
		return x, nil
	}
	for dir := parentOf(p); dir != ""; dir = parentOf(dir) {
		if x, ok := e.entries[dir]; ok {
			if x.Kind != KindDir {
				return nil, fmt.Errorf("%w: %q is under %q, which is not a directory", ErrPath, p, dir)
			}
			break
		}
	}
	return nil, fs.ErrNotExist
}

// parents makes the directories above p, refusing a path under a file or a
// symlink.
func (e *Edit) parents(p string) error {
	var missing []string
	for dir := parentOf(p); dir != ""; dir = parentOf(dir) {
		x, ok := e.entries[dir]
		if !ok {
			missing = append(missing, dir)
			continue
		}
		if x.Kind != KindDir {
			return fmt.Errorf("%w: %q is under %q, which is not a directory", ErrPath, p, dir)
		}
		break
	}
	for _, dir := range missing {
		e.entries[dir] = &Entry{Path: dir, Kind: KindDir, Mode: 0o755 | fs.ModeDir, ModTime: e.now}
	}
	return nil
}

// PutFile writes a file, creating its parents. An existing file keeps its
// mode; a new one is 0644.
func (e *Edit) PutFile(p string, data []byte) error {
	if p == "" {
		return fmt.Errorf("%w: the root is a directory", ErrPath)
	}
	if err := e.parents(p); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if x, ok := e.entries[p]; ok {
		if x.Kind != KindFile {
			return fmt.Errorf("%w: %q is a %s", ErrPath, p, kindName(x.Kind))
		}
		mode = x.Mode
	}
	e.entries[p] = &Entry{Path: p, Kind: KindFile, Bytes: int64(len(data)), Mode: mode, ModTime: e.now, data: data}
	return nil
}

func kindName(kind string) string {
	if kind == KindDir {
		return "directory"
	}
	return kind
}

// Mkdir makes a directory and its parents; ErrExists when p is there.
func (e *Edit) Mkdir(p string) error {
	if _, ok := e.entries[p]; ok {
		return fmt.Errorf("%w: %q", ErrExists, p)
	}
	if err := e.parents(p); err != nil {
		return err
	}
	e.entries[p] = &Entry{Path: p, Kind: KindDir, Mode: 0o755 | fs.ModeDir, ModTime: e.now}
	return nil
}

// under reports whether q is p or inside it.
func under(q, p string) bool { return q == p || strings.HasPrefix(q, p+"/") }

// Remove takes p and everything under it out of the tree.
func (e *Edit) Remove(p string) error {
	if p == "" {
		return fmt.Errorf("%w: the root cannot be removed", ErrPath)
	}
	if _, ok := e.entries[p]; !ok {
		return fs.ErrNotExist
	}
	for q := range e.entries {
		if under(q, p) {
			delete(e.entries, q)
		}
	}
	return nil
}

// Move renames from to to, with everything under it; it never overwrites.
func (e *Edit) Move(from, to string) error {
	switch {
	case from == "" || to == "":
		return fmt.Errorf("%w: the root cannot be renamed", ErrPath)
	case under(to, from):
		return fmt.Errorf("%w: %q is inside %q", ErrPath, to, from)
	}
	if _, ok := e.entries[from]; !ok {
		return fs.ErrNotExist
	}
	if _, ok := e.entries[to]; ok {
		return fmt.Errorf("%w: %q", ErrExists, to)
	}
	if err := e.parents(to); err != nil {
		return err
	}
	moved := map[string]*Entry{}
	for q, x := range e.entries {
		if under(q, from) {
			c := *x
			c.Path = to + strings.TrimPrefix(q, from)
			moved[c.Path] = &c
			delete(e.entries, q)
		}
	}
	for q, x := range moved {
		e.entries[q] = x
	}
	e.entries[to].ModTime = e.now
	return nil
}

// Insert copies src's entry at from, with everything under it, to to (a
// restore from an earlier snapshot); it never overwrites.
func (e *Edit) Insert(src *Snapshot, from, to string) error {
	root, err := src.Lookup(from)
	if err != nil {
		return err
	}
	if to == "" {
		return fmt.Errorf("%w: the root exists", ErrExists)
	}
	if _, ok := e.entries[to]; ok {
		return fmt.Errorf("%w: %q", ErrExists, to)
	}
	if err := e.parents(to); err != nil {
		return err
	}
	return src.Walk(root, func(x *Entry) error {
		c := *x
		c.Path = to + strings.TrimPrefix(x.Path, from)
		e.entries[c.Path] = &c
		return nil
	})
}

// Archive packs the edited tree as a workspace tar (as Pack writes one), and
// counts its regular files. ErrTooLarge when it exceeds MaxBytes.
func (e *Edit) Archive() ([]byte, int, error) {
	paths := make([]string, 0, len(e.entries))
	for p := range e.entries {
		if p != "" {
			paths = append(paths, p)
		}
	}
	// A parent sorts before its children ("a" < "a/..."), so it is
	// written first.
	sort.Strings(paths)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	files := 0
	for _, p := range paths {
		x := e.entries[p]
		h := &tar.Header{Name: p, ModTime: x.ModTime, Mode: int64(x.Mode.Perm())}
		switch x.Kind {
		case KindDir:
			h.Typeflag, h.Name = tar.TypeDir, p+"/"
		case KindSymlink:
			h.Typeflag, h.Linkname = tar.TypeSymlink, x.Target
		default:
			h.Typeflag, h.Size = tar.TypeReg, x.Bytes
			files++
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, 0, err
		}
		if x.Kind == KindFile {
			if _, err := tw.Write(x.data); err != nil {
				return nil, 0, err
			}
		}
		if buf.Len() > MaxBytes {
			return nil, 0, ErrTooLarge
		}
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), files, nil
}

// Change is one path that differs between two snapshots.
type Change struct {
	// Op is added, modified, removed or renamed (From set).
	Op   string
	Path string
	From string
}

// Diff lists the regular files that differ from before to after, by path: a
// file removed from one path and added at another with the same content is
// a rename.
func Diff(before, after *Snapshot) []Change {
	files := func(s *Snapshot) map[string]*Entry {
		m := map[string]*Entry{}
		if s == nil {
			return m
		}
		for p, x := range s.entries {
			if x.Kind == KindFile {
				m[p] = x
			}
		}
		return m
	}
	a, b := files(before), files(after)
	var added, removed []string
	var out []Change
	for p, x := range b {
		y, ok := a[p]
		switch {
		case !ok:
			added = append(added, p)
		case !bytes.Equal(x.data, y.data) || x.Mode != y.Mode:
			out = append(out, Change{Op: "modified", Path: p})
		}
	}
	for p := range a {
		if _, ok := b[p]; !ok {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	gone := map[string]bool{}
	for _, p := range added {
		from := ""
		for _, q := range removed {
			if !gone[q] && bytes.Equal(a[q].data, b[p].data) {
				from = q
				break
			}
		}
		if from != "" {
			gone[from] = true
			out = append(out, Change{Op: "renamed", Path: p, From: from})
		} else {
			out = append(out, Change{Op: "added", Path: p})
		}
	}
	for _, q := range removed {
		if !gone[q] {
			out = append(out, Change{Op: "removed", Path: q})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Join is dir/name, or name at the root.
func Join(dir, name string) string {
	if dir == "" {
		return name
	}
	return path.Join(dir, name)
}
