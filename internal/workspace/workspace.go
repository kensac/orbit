// Package workspace packs a chat agent's workspace (its conversation's
// working directory) as a tar and unpacks one, for the sandbox it runs in and
// for the snapshot stored after each run (docs/ARCHITECTURE.md, Chat agents).
// Unpacking writes through os.Root, so no entry, symlinks included, can
// place anything outside the directory it is unpacked into.
package workspace

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MaxBytes bounds a workspace's tar: sandboxd's own limit on a tar either
// way (sandbox.MaxArchiveBytes).
const MaxBytes = 256 << 20

// MaxEntries bounds its entries, as sandboxd's tar filter does.
const MaxEntries = 100_000

// Tree is one directory packed under a prefix of the tar ("" for the root).
type Tree struct {
	Prefix string
	Dir    string
	// Skip leaves out a path relative to Dir (slash-separated, a directory
	// without its slash), and everything under it.
	Skip func(rel string) bool
}

// Pack writes trees into one tar. A missing directory packs nothing.
func Pack(trees ...Tree) ([]byte, int, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	files := 0
	for _, t := range trees {
		if _, err := os.Stat(t.Dir); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(t.Dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(t.Dir, p)
			if err != nil || rel == "." {
				return err
			}
			rel = filepath.ToSlash(rel)
			if t.Skip != nil && t.Skip(rel) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			name := path.Join(t.Prefix, rel)
			h := &tar.Header{Name: name, ModTime: info.ModTime(), Mode: int64(info.Mode().Perm())}
			switch {
			case d.IsDir():
				h.Typeflag, h.Name = tar.TypeDir, name+"/"
			case info.Mode()&fs.ModeSymlink != 0:
				link, err := os.Readlink(p)
				if err != nil {
					return err
				}
				h.Typeflag, h.Linkname = tar.TypeSymlink, link
			case info.Mode().IsRegular():
				h.Typeflag, h.Size = tar.TypeReg, info.Size()
				files++
			default:
				return nil
			}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if h.Typeflag == tar.TypeReg {
				f, err := os.Open(p) //nolint:gosec // a path the walk found under orbit's own directory
				if err != nil {
					return err
				}
				_, err = io.Copy(tw, f)
				_ = f.Close()
				if err != nil {
					return err
				}
			}
			if buf.Len() > MaxBytes {
				return fmt.Errorf("the workspace is larger than %d MiB", MaxBytes>>20)
			}
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), files, nil
}

// Route picks where an entry goes: the directory to write it under and its
// path there, or ok false to drop it.
type Route func(name string) (dir, rel string, ok bool)

// Into routes every entry under one directory.
func Into(dir string) Route {
	return func(name string) (string, string, bool) { return dir, name, true }
}

// Unpack writes an archive's regular files, directories and symlinks into
// the directories route names. Each directory is cleared first when clear
// lists it, so a file the archive no longer has is gone. Nothing lands
// outside a routed directory.
func Unpack(archive io.Reader, route Route, clear ...string) (files int, err error) {
	for _, dir := range clear {
		if err := os.RemoveAll(dir); err != nil {
			return 0, err
		}
	}
	roots := map[string]*os.Root{}
	defer func() {
		for _, r := range roots {
			_ = r.Close()
		}
	}()
	root := func(dir string) (*os.Root, error) {
		if r, ok := roots[dir]; ok {
			return r, nil
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		r, err := os.OpenRoot(dir)
		if err != nil {
			return nil, err
		}
		roots[dir] = r
		return r, nil
	}
	tr := tar.NewReader(io.LimitReader(archive, MaxBytes+1<<20))
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, fmt.Errorf("read the workspace: %w", err)
		}
		if n > MaxEntries {
			return files, fmt.Errorf("the workspace has more than %d entries", MaxEntries)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			continue
		}
		dir, rel, ok := route(name)
		if !ok || rel == "" || rel == "." {
			continue
		}
		r, err := root(dir)
		if err != nil {
			return files, err
		}
		if parent := path.Dir(rel); parent != "." {
			if err := r.MkdirAll(parent, 0o700); err != nil {
				return files, fmt.Errorf("%s: %w", name, err)
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := r.MkdirAll(rel, 0o700); err != nil {
				return files, fmt.Errorf("%s: %w", name, err)
			}
		case tar.TypeReg:
			total += h.Size
			if total > MaxBytes {
				return files, fmt.Errorf("the workspace is larger than %d MiB", MaxBytes>>20)
			}
			mode := os.FileMode(0o600)
			if h.Mode&0o111 != 0 {
				mode = 0o700
			}
			_ = r.Remove(rel)
			f, err := r.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return files, fmt.Errorf("%s: %w", name, err)
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return files, fmt.Errorf("%s: %w", name, err)
			}
			if !h.ModTime.IsZero() {
				_ = r.Chtimes(rel, h.ModTime, h.ModTime)
			}
			files++
		case tar.TypeSymlink:
			_ = r.Remove(rel)
			if err := r.Symlink(h.Linkname, rel); err != nil {
				return files, fmt.Errorf("%s: %w", name, err)
			}
		}
	}
}

// File is one regular file of a workspace.
type File struct {
	Path       string    `json:"path"`
	Bytes      int64     `json:"bytes"`
	ModifiedAt time.Time `json:"modified_at"`
}

// List returns an archive's regular files, by path, those hide drops left out.
func List(archive []byte, hide func(name string) bool) ([]File, error) {
	tr := tar.NewReader(bytes.NewReader(archive))
	var out []File
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if h.Typeflag != tar.TypeReg || (hide != nil && hide(name)) {
			continue
		}
		out = append(out, File{Path: name, Bytes: h.Size, ModifiedAt: h.ModTime.UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Read returns one regular file of an archive, or fs.ErrNotExist.
func Read(archive []byte, name string) ([]byte, error) {
	name = path.Clean(strings.TrimPrefix(name, "/"))
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fs.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Clean(strings.TrimPrefix(h.Name, "./")) == name {
			return io.ReadAll(tr)
		}
	}
}
