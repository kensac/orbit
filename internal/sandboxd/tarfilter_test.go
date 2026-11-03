package sandboxd

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"testing"
)

type entry struct {
	h    tar.Header
	body string
}

func mkTar(t *testing.T, es ...entry) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range es {
		h := e.h
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

func readTar(t *testing.T, r io.Reader) []entry {
	t.Helper()
	var out []entry
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out = append(out, entry{*h, string(b)})
	}
}

func TestSanitizeInRewritesOwnershipAndModes(t *testing.T) {
	in := mkTar(t,
		entry{h: tar.Header{Typeflag: tar.TypeDir, Name: "./", Mode: 0o700}},
		entry{h: tar.Header{Typeflag: tar.TypeDir, Name: "cmd", Mode: 0o755, Uid: 0}},
		entry{h: tar.Header{Typeflag: tar.TypeReg, Name: "cmd/main.go", Mode: 0o4755, Uid: 0, Gid: 0, Uname: "root",
			PAXRecords: map[string]string{"SCHILY.xattr.security.capability": "x"}}, body: "package main"},
		entry{h: tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "cmd/main.go"}},
		entry{h: tar.Header{Typeflag: tar.TypeLink, Name: "hard", Linkname: "./cmd/main.go"}},
	)
	var out bytes.Buffer
	if err := SanitizeIn(&out, in); err != nil {
		t.Fatal(err)
	}
	es := readTar(t, &out)
	if len(es) != 5 {
		t.Fatalf("%d entries", len(es))
	}
	if es[0].h.Name != "./" || es[0].h.Uid != UID || es[0].h.Typeflag != tar.TypeDir {
		t.Fatalf("root entry %+v", es[0].h)
	}
	for _, e := range es {
		if e.h.Uid != UID || e.h.Gid != UID || e.h.Uname != "" || len(e.h.PAXRecords) != 0 {
			t.Errorf("%s: owner %d:%d %q pax %v", e.h.Name, e.h.Uid, e.h.Gid, e.h.Uname, e.h.PAXRecords)
		}
		if e.h.Mode&^0o777 != 0 {
			t.Errorf("%s: mode %o keeps special bits", e.h.Name, e.h.Mode)
		}
	}
	if es[2].h.Name != "cmd/main.go" || es[2].body != "package main" || es[2].h.Mode != 0o755 {
		t.Fatalf("file %+v %q", es[2].h, es[2].body)
	}
	if es[4].h.Linkname != "cmd/main.go" {
		t.Fatalf("hard link %q", es[4].h.Linkname)
	}
}

func TestSanitizeInRefuses(t *testing.T) {
	for name, e := range map[string]entry{
		"absolute":      {h: tar.Header{Typeflag: tar.TypeReg, Name: "/etc/passwd"}, body: "x"},
		"climbing":      {h: tar.Header{Typeflag: tar.TypeReg, Name: "a/../../x"}, body: "x"},
		"parent":        {h: tar.Header{Typeflag: tar.TypeDir, Name: ".."}},
		"char device":   {h: tar.Header{Typeflag: tar.TypeChar, Name: "null", Devmajor: 1, Devminor: 3}},
		"block device":  {h: tar.Header{Typeflag: tar.TypeBlock, Name: "sda"}},
		"fifo":          {h: tar.Header{Typeflag: tar.TypeFifo, Name: "p"}},
		"hard link out": {h: tar.Header{Typeflag: tar.TypeLink, Name: "h", Linkname: "../../etc/shadow"}},
		"hard link abs": {h: tar.Header{Typeflag: tar.TypeLink, Name: "h", Linkname: "/etc/shadow"}},
	} {
		if err := SanitizeIn(io.Discard, mkTar(t, e)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := SanitizeIn(io.Discard, bytes.NewReader([]byte("not a tar at all, but long enough to read a header from it............................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................"))); err == nil {
		t.Error("garbage accepted")
	}
}

func TestSanitizeExtensionInMakesInputsReadOnlyAndOnlyOutWritable(t *testing.T) {
	in := mkTar(t,
		entry{h: tar.Header{Typeflag: tar.TypeDir, Name: "extension/", Mode: 0o755}},
		entry{h: tar.Header{Typeflag: tar.TypeReg, Name: "extension/tool", Mode: 0o755}, body: "code"},
		entry{h: tar.Header{Typeflag: tar.TypeDir, Name: "inputs/", Mode: 0o755}},
		entry{h: tar.Header{Typeflag: tar.TypeReg, Name: "inputs/data", Mode: 0o600}, body: "input"},
	)
	var out bytes.Buffer
	if err := SanitizeExtensionIn(&out, in); err != nil {
		t.Fatal(err)
	}
	es := readTar(t, &out)
	if es[0].h.Uid != 0 || es[0].h.Mode&0o222 != 0 {
		t.Fatalf("root is writable: %+v", es[0].h)
	}
	for _, e := range es[1:] {
		if e.h.Name == "extension/" || e.h.Name == "inputs/" {
			if e.h.Typeflag != tar.TypeDir || e.h.Uid != 0 || e.h.Gid != 0 || e.h.Mode&0o777 != 0o555 {
				t.Errorf("package/input directory is not root-owned and searchable read-only: %+v", e.h)
			}
		}
		if e.h.Name == "extension/tool" || e.h.Name == "inputs/data" {
			if e.h.Uid != 0 || e.h.Gid != 0 || e.h.Mode&0o222 != 0 || e.h.Mode&0o444 != 0o444 {
				t.Errorf("input/package writable: %+v", e.h)
			}
		}
	}
	if err := SanitizeExtensionIn(io.Discard, mkTar(t, entry{h: tar.Header{Typeflag: tar.TypeReg, Name: "other/file", Mode: 0o644}, body: "x"})); err == nil {
		t.Fatal("accepted file outside fixed roots")
	}
}

func TestSanitizeOutStripsPrefix(t *testing.T) {
	in := mkTar(t,
		entry{h: tar.Header{Typeflag: tar.TypeDir, Name: "work/", Mode: 0o755, Uid: UID}},
		entry{h: tar.Header{Typeflag: tar.TypeReg, Name: "work/out.txt", Mode: 0o644, Uid: UID, Uname: "build"}, body: "out"},
		entry{h: tar.Header{Typeflag: tar.TypeDir, Name: "work/sub/", Mode: 0o755}},
		entry{h: tar.Header{Typeflag: tar.TypeSymlink, Name: "work/sub/escape", Linkname: "/etc/passwd"}},
		entry{h: tar.Header{Typeflag: tar.TypeLink, Name: "work/again", Linkname: "work/out.txt"}},
	)
	var out bytes.Buffer
	if err := SanitizeOut(&out, in); err != nil {
		t.Fatal(err)
	}
	es := readTar(t, &out)
	names := []string{}
	for _, e := range es {
		names = append(names, e.h.Name)
		if e.h.Uid != 0 || e.h.Uname != "" {
			t.Errorf("%s keeps its owner", e.h.Name)
		}
	}
	want := []string{"out.txt", "sub/", "sub/escape", "again"}
	if len(names) != len(want) {
		t.Fatalf("names %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names %v, want %v", names, want)
		}
	}
	// The symlink is passed through for the diff policy to judge.
	if es[2].h.Linkname != "/etc/passwd" || es[3].h.Linkname != "out.txt" || es[0].body != "out" {
		t.Fatalf("links or body: %+v", es)
	}
}

func TestSanitizeOutRefuses(t *testing.T) {
	for name, e := range map[string]entry{
		"fifo":          {h: tar.Header{Typeflag: tar.TypeFifo, Name: "work/p"}},
		"device":        {h: tar.Header{Typeflag: tar.TypeChar, Name: "work/null"}},
		"outside":       {h: tar.Header{Typeflag: tar.TypeReg, Name: "etc/passwd"}, body: "x"},
		"climbing":      {h: tar.Header{Typeflag: tar.TypeReg, Name: "work/../etc/passwd"}, body: "x"},
		"hard link out": {h: tar.Header{Typeflag: tar.TypeLink, Name: "work/h", Linkname: "etc/shadow"}},
	} {
		if err := SanitizeOut(io.Discard, mkTar(t, e)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
