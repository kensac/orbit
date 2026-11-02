package extensions

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	entries := 0
	entryOK := false
	files := map[string]bool{}
	var manifest []byte
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		entries++
		if entries > 512 || len(name) > 256 || strings.Count(name, "/") > MaxDepth {
			return fmt.Errorf("package exceeds path or entry limits")
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files allowed", name)
		}
		before, err := d.Info()
		if err != nil {
			return err
		}
		f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			return err
		}
		sys, ok := stat.Sys().(*syscall.Stat_t)
		if stat.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !stat.Mode().IsRegular() || !os.SameFile(before, stat) || !ok || sys.Nlink != 1 {
			return fmt.Errorf("%s: changed, special or file with multiple links", name)
		}
		result.Files++
		if result.Files > MaxFiles {
			return fmt.Errorf("package exceeds %d files", MaxFiles)
		}
		capBytes := int64(MaxPackageBytes) - result.Bytes
		if name == "manifest.json" && capBytes > MaxManifestBytes {
			capBytes = MaxManifestBytes
		}
		if stat.Size() > capBytes {
			return fmt.Errorf("%s: package or manifest size limit exceeded", name)
		}
		body, err := io.ReadAll(io.LimitReader(f, capBytes+1))
		if err != nil {
			return err
		}
		if int64(len(body)) > capBytes || int64(len(body)) != stat.Size() {
			return fmt.Errorf("%s: size changed or limit exceeded", name)
		}
		after, err := f.Stat()
		if err != nil {
			return err
		}
		if after.Size() != stat.Size() || !after.ModTime().Equal(stat.ModTime()) {
			return fmt.Errorf("%s: changed during validation", name)
		}
		files[name] = stat.Mode().Perm()&0111 != 0 && validELF(body)
		result.Bytes += int64(len(body))
		// WalkDir sorts paths; length-prefixing prevents ambiguous concatenation.
		_ = binary.Write(hash, binary.BigEndian, uint64(len(name)))
		_, _ = io.WriteString(hash, name)
		_ = binary.Write(hash, binary.BigEndian, uint32(stat.Mode().Perm()))
		_ = binary.Write(hash, binary.BigEndian, uint64(len(body)))
		_, _ = hash.Write(body)
		if name == "manifest.json" {
			manifest = body
		}
		if manifest != nil && result.Manifest == nil {
			result.Manifest, err = Parse(manifest)
			if err != nil {
				return err
			}
		}
		// Entrypoint may sort before the manifest, so verify it after the walk.
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result.Manifest == nil {
		return nil, fmt.Errorf("manifest.json is required")
	}
	entryOK = files[result.Manifest.Entrypoint]
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
