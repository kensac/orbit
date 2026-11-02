package extensions

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
)

// Package describes validated bytes, not an installation or execution permission.
type Package struct {
	Manifest       *Manifest `json:"manifest"`
	Digest         string    `json:"sha256"`
	ManifestDigest string    `json:"manifest_sha256"`
	Files          int       `json:"files"`
	Bytes          int64     `json:"bytes"`
}

// ValidateDir reads a package without executing code or mutating it. The caller
// must keep the directory immutable; validation is not a later install handle.
func ValidateDir(dir string) (*Package, error) {
	dir = rootName(dir)
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("package root must be a directory, not a link")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	result := &Package{}
	hash := sha256.New()

	names, err := packageFiles(root)
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	var manifest []byte
	for _, name := range names {
		capBytes := int64(MaxPackageBytes) - result.Bytes
		if name == "manifest.json" && capBytes > MaxManifestBytes {
			capBytes = MaxManifestBytes
		}
		body, mode, err := readPackageFile(root, name, capBytes)
		if err != nil {
			return nil, err
		}
		result.Files++
		result.Bytes += int64(len(body))
		files[name] = mode&0111 != 0 && validELF(body)
		// Paths are globally sorted; length prefixes disambiguate every field.
		_ = binary.Write(hash, binary.BigEndian, uint64(len(name)))
		_, _ = io.WriteString(hash, name)
		_ = binary.Write(hash, binary.BigEndian, uint32(mode))
		_ = binary.Write(hash, binary.BigEndian, uint64(len(body)))
		_, _ = hash.Write(body)
		if name == "manifest.json" {
			manifest = body
			result.Manifest, err = Parse(body)
			if err != nil {
				return nil, err
			}
		}
	}
	if result.Manifest == nil {
		return nil, fmt.Errorf("manifest.json is required")
	}
	entryOK := files[result.Manifest.Entrypoint]
	if !entryOK {
		return nil, fmt.Errorf("entrypoint must name a 64-bit little-endian Linux ELF executable (x86-64 or arm64)")
	}
	result.Digest = hex.EncodeToString(hash.Sum(nil))
	sum := sha256.Sum256(manifest)
	result.ManifestDigest = hex.EncodeToString(sum[:])
	return result, nil
}

// validELF checks the supported Linux ABI header; validation never loads code.
func validELF(b []byte) bool {
	if len(b) < 64 || string(b[:4]) != "\x7fELF" || b[4] != 2 || b[5] != 1 || b[6] != 1 || (b[7] != 0 && b[7] != 3) {
		return false
	}
	typ := binary.LittleEndian.Uint16(b[16:18])
	machine := binary.LittleEndian.Uint16(b[18:20])
	return (typ == 2 || typ == 3) && (machine == 62 || machine == 183) && binary.LittleEndian.Uint32(b[20:24]) == 1 && binary.LittleEndian.Uint16(b[52:54]) == 64
}

// packageFiles bounds directory reads before sorting or visiting their children.
func packageFiles(root *os.Root) ([]string, error) {
	var names []string
	entries := 0
	var visit func(string) error
	visit = func(dir string) error {
		f, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		children, readErr := f.ReadDir(512 - entries + 1)
		_ = f.Close()
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		entries += len(children)
		if entries > 512 {
			return fmt.Errorf("package exceeds 512 directory entries")
		}
		for _, d := range children {
			name := path.Join(dir, d.Name())
			if len(name) > 256 || strings.Count(name, "/") > MaxDepth {
				return fmt.Errorf("package exceeds path limits")
			}
			if d.IsDir() {
				if err := visit(name); err != nil {
					return err
				}
				continue
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("%s: only regular files allowed", name)
			}
			names = append(names, name)
			if len(names) > MaxFiles {
				return fmt.Errorf("package exceeds %d files", MaxFiles)
			}
		}
		return nil
	}
	if err := visit("."); err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func readPackageFile(root *os.Root, name string, capBytes int64) ([]byte, fs.FileMode, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, 0, err
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	stat, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	sys, ok := stat.Sys().(*syscall.Stat_t)
	if stat.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !stat.Mode().IsRegular() || !os.SameFile(before, stat) || !ok || sys.Nlink != 1 {
		return nil, 0, fmt.Errorf("%s: changed, special or file with multiple links", name)
	}
	if stat.Size() > capBytes {
		return nil, 0, fmt.Errorf("%s: package or manifest size limit exceeded", name)
	}
	body, err := io.ReadAll(io.LimitReader(f, capBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(body)) > capBytes || int64(len(body)) != stat.Size() {
		return nil, 0, fmt.Errorf("%s: size changed or limit exceeded", name)
	}
	after, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if after.Size() != stat.Size() || !after.ModTime().Equal(stat.ModTime()) {
		return nil, 0, fmt.Errorf("%s: changed during validation", name)
	}
	return body, stat.Mode().Perm(), nil
}

// rootName removes only terminal separators and dot components so Lstat sees the named root itself.
// Meaningful parent components, including .., retain their filesystem semantics.
func rootName(dir string) string {
	if dir == "" {
		return dir
	}
	separator := string(os.PathSeparator)
	for {
		dir = strings.TrimRight(dir, separator)
		if dir == "" {
			return separator
		}
		if !strings.HasSuffix(dir, separator+".") {
			return dir
		}
		dir = strings.TrimSuffix(dir, separator+".")
	}
}
