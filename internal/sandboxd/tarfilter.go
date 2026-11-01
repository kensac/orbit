package sandboxd

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/sandbox"
)

// maxEntries bounds a workdir's entry count, either direction. orbit's
// checkout with its .git is a few thousand entries.
const maxEntries = 200_000

// cleanName returns name relative and clean, or an error for a name that is
// absolute, empty or climbs out.
func cleanName(name string) (string, error) {
	if strings.IndexByte(name, 0) >= 0 {
		return "", errors.New("entry name contains a NUL byte")
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("absolute entry %q", name)
	}
	c := path.Clean(name)
	if c == "." {
		return ".", nil
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("entry %q leaves the workdir", name)
	}
	return c, nil
}

// allowedType is a regular file, a directory, a symlink or a hard link.
// Devices, FIFOs and anything else are refused in both directions: nothing
// a build needs, and a device node in a volume is a way past the container.
func allowedType(t byte) bool {
	switch t {
	case tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink:
		return true
	}
	return false
}

// SanitizeIn copies the caller's workdir tar to dst as the tar docker
// unpacks at /work: every entry owned by UID, permission bits only (no
// setuid, setgid or sticky), no extended attributes, names relative and
// inside, and a first "./" entry so /work itself is owned by UID.
func SanitizeIn(dst io.Writer, src io.Reader) error {
	tw := tar.NewWriter(dst)
	now := time.Now()
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "./", Mode: 0o755, Uid: UID, Gid: UID, ModTime: now}); err != nil {
		return err
	}
	tr := tar.NewReader(src)
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if n >= maxEntries {
			return fmt.Errorf("archive has more than %d entries", maxEntries)
		}
		if !allowedType(h.Typeflag) {
			return fmt.Errorf("entry %q has type %q; only files, directories and links are accepted", h.Name, h.Typeflag)
		}
		name, err := cleanName(h.Name)
		if err != nil {
			return err
		}
		if name == "." {
			continue
		}
		out := &tar.Header{
			Typeflag: h.Typeflag,
			Name:     name,
			Mode:     h.Mode & 0o777,
			Uid:      UID,
			Gid:      UID,
			ModTime:  h.ModTime,
		}
		switch h.Typeflag {
		case tar.TypeDir:
			out.Name += "/"
			out.Mode |= 0o700
		case tar.TypeReg:
			out.Size = h.Size
			out.Mode |= 0o600
		case tar.TypeSymlink:
			out.Linkname = h.Linkname
		case tar.TypeLink:
			l, err := cleanName(h.Linkname)
			if err != nil || l == "." {
				return fmt.Errorf("hard link %q to %q leaves the workdir", h.Name, h.Linkname)
			}
			out.Linkname = l
		}
		total += out.Size
		if total > sandbox.MaxArchiveBytes {
			return fmt.Errorf("archive content exceeds %d bytes", sandbox.MaxArchiveBytes)
		}
		if err := tw.WriteHeader(out); err != nil {
			return err
		}
		if out.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, out.Size); err != nil {
				return fmt.Errorf("read %s: %w", name, err)
			}
		}
	}
	return tw.Close()
}

// SanitizeOut copies docker's tar of /work (entries prefixed "work/") to dst
// with entries relative to the workdir, owners and extended attributes
// dropped, and the same type, name and size rules as SanitizeIn. A workdir
// the model filled with something else fails the readback, with the entry
// named, rather than being silently trimmed.
func SanitizeOut(dst io.Writer, src io.Reader) error {
	prefix := strings.TrimPrefix(sandbox.WorkDir, "/")
	strip := func(name string) (string, error) {
		c, err := cleanName(name)
		if err != nil {
			return "", err
		}
		if c == prefix {
			return ".", nil
		}
		rel, ok := strings.CutPrefix(c, prefix+"/")
		if !ok {
			return "", fmt.Errorf("entry %q is outside %s", name, sandbox.WorkDir)
		}
		return rel, nil
	}
	tw := tar.NewWriter(dst)
	tr := tar.NewReader(src)
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read workdir: %w", err)
		}
		if n >= maxEntries {
			return fmt.Errorf("workdir has more than %d entries", maxEntries)
		}
		if !allowedType(h.Typeflag) {
			return fmt.Errorf("workdir entry %q has type %q; only files, directories and links are read back", h.Name, h.Typeflag)
		}
		name, err := strip(h.Name)
		if err != nil {
			return err
		}
		if name == "." {
			continue
		}
		out := &tar.Header{Typeflag: h.Typeflag, Name: name, Mode: h.Mode & 0o777, ModTime: h.ModTime}
		switch h.Typeflag {
		case tar.TypeDir:
			out.Name += "/"
		case tar.TypeReg:
			out.Size = h.Size
		case tar.TypeSymlink:
			// Kept as written: whether a link is acceptable is the diff
			// policy's decision, and it needs to see the link to refuse it.
			out.Linkname = h.Linkname
		case tar.TypeLink:
			l, err := strip(h.Linkname)
			if err != nil || l == "." {
				return fmt.Errorf("hard link %q to %q leaves the workdir", h.Name, h.Linkname)
			}
			out.Linkname = l
		}
		total += out.Size
		if total > sandbox.MaxArchiveBytes {
			return fmt.Errorf("workdir content exceeds %d bytes", sandbox.MaxArchiveBytes)
		}
		if err := tw.WriteHeader(out); err != nil {
			return err
		}
		if out.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, out.Size); err != nil {
				return fmt.Errorf("read %s: %w", name, err)
			}
		}
	}
	return tw.Close()
}
