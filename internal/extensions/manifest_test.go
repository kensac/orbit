package extensions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/reducer/manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func changed(t *testing.T, edit func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if e := json.Unmarshal(fixture(t), &m); e != nil {
		t.Fatal(e)
	}
	edit(m)
	b, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func operation(m map[string]any) map[string]any { return m["operations"].([]any)[0].(map[string]any) }

func TestManifestRejections(t *testing.T) {
	tests := map[string][]byte{
		"duplicate top key":          bytes.Replace(fixture(t), []byte(`"id": "daily-total"`), []byte(`"id":"one","id":"two"`), 1),
		"duplicate nested key":       bytes.Replace(fixture(t), []byte(`"timeout_ms":1000`), []byte(`"timeout_ms":1000,"timeout_ms":2000`), 1),
		"trailing document":          append(fixture(t), []byte(` {}`)...),
		"case aliased key":           bytes.Replace(fixture(t), []byte(`"id": "daily-total"`), []byte(`"id":"daily-total","ID":"other"`), 1),
		"unknown field":              changed(t, func(m map[string]any) { m["credential_env"] = "SECRET" }),
		"unsupported protocol":       changed(t, func(m map[string]any) { m["protocol"] = "orbit.extension.v2" }),
		"wasm reserved":              changed(t, func(m map[string]any) { m["runtime"] = "wasm" }),
		"bad version":                changed(t, func(m map[string]any) { m["version"] = "latest" }),
		"traversal":                  changed(t, func(m map[string]any) { m["entrypoint"] = "../elsewhere" }),
		"backslash":                  changed(t, func(m map[string]any) { m["entrypoint"] = `dir\reducer` }),
		"absolute":                   changed(t, func(m map[string]any) { m["entrypoint"] = "/bin/sh" }),
		"duplicate operations":       changed(t, func(m map[string]any) { m["operations"] = []any{operation(m), operation(m)} }),
		"duplicate inputs":           changed(t, func(m map[string]any) { operation(m)["inputs"] = []string{"same", "same"} }),
		"write through tool":         changed(t, func(m map[string]any) { o := operation(m); o["kind"] = "tool"; o["endpoint"] = "write" }),
		"action without destination": changed(t, func(m map[string]any) { operation(m)["kind"] = "action" }),
		"unknown limit":              changed(t, func(m map[string]any) { operation(m)["limits"].(map[string]any)["network"] = true }),
		"unbounded resources":        changed(t, func(m map[string]any) { operation(m)["limits"].(map[string]any)["timeout_ms"] = 30001 }),
		"oversize":                   bytes.Repeat([]byte(" "), MaxManifestBytes+1),
	}
	for name, b := range tests {
		t.Run(name, func(t *testing.T) {
			if _, e := Parse(b); e == nil {
				t.Fatal("accepted unsafe manifest")
			}
		})
	}
}

func TestSchemaResourceAndConsistency(t *testing.T) {
	schemas := map[string]string{
		"remote ref":               `{"$ref":"https://outside/schema","type":"string","maxLength":8}`,
		"recursive ref":            `{"$ref":"#","type":"string","maxLength":8}`,
		"unknown keyword":          `{"type":"string","maxLength":8,"pattern":".*"}`,
		"unbounded string":         `{"type":"string"}`,
		"unbounded array":          `{"type":"array","items":{"type":"boolean"}}`,
		"open object":              `{"type":"object","properties":{},"additionalProperties":true}`,
		"negative length":          `{"type":"string","maxLength":-1}`,
		"reversed bounds":          `{"type":"number","minimum":2,"maximum":1}`,
		"fractional integer":       `{"type":"integer","minimum":0,"maximum":1.5}`,
		"exponent allocation":      `{"type":"number","minimum":0,"maximum":1e999999999}`,
		"tiny exponent allocation": `{"type":"number","minimum":0,"maximum":1e-999999999}`,
		"enum bounds":              `{"type":"integer","minimum":0,"maximum":1,"enum":[2]}`,
		"enum string bounds":       `{"type":"string","maxLength":1,"enum":["two"]}`,
		"numeric duplicate":        `{"type":"number","minimum":0,"maximum":2,"enum":[1,1.0]}`,
		"enum wrong type":          `{"type":"boolean","enum":["true"]}`,
		"required undeclared":      `{"type":"object","properties":{},"required":["secret"],"additionalProperties":false}`,
	}
	deep := `{"type":"boolean"}`
	for range 18 {
		deep = `{"type":"array","maxItems":1,"items":` + deep + `}`
	}
	schemas["deep schema"] = deep
	for name, s := range schemas {
		t.Run(name, func(t *testing.T) {
			b := changed(t, func(m map[string]any) { operation(m)["input_schema"] = json.RawMessage(s) })
			if _, e := Parse(b); e == nil {
				t.Fatal("accepted unsafe schema")
			}
		})
	}
	b := changed(t, func(m map[string]any) {
		operation(m)["input_schema"] = json.RawMessage(`{"type":"number","minimum":0,"maximum":0.3,"enum":[0.1,0.2]}`)
	})
	if _, e := Parse(b); e != nil {
		t.Fatal(e)
	}
}

func packageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"manifest.json", "reducer"} {
		b, e := os.ReadFile(filepath.Join("testdata/reducer", name))
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(dir, name), b, 0755); e != nil {
			t.Fatal(e)
		}
	}
	return dir
}

func TestPackageDigestAndNoExecution(t *testing.T) {
	dir := packageDir(t)
	first, e := ValidateDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	second, e := ValidateDir(dir)
	if e != nil || first.Digest != second.Digest {
		t.Fatalf("unstable digest: %v", e)
	}
	if first.Files != 2 || len(first.ManifestDigest) != 64 {
		t.Fatalf("bad metadata: %+v", first)
	}
	if e = os.WriteFile(filepath.Join(dir, "input.json"), []byte(`{"amount":"0.10"}`), 0600); e != nil {
		t.Fatal(e)
	}
	third, e := ValidateDir(dir)
	if e != nil || first.Digest == third.Digest || first.ManifestDigest != third.ManifestDigest {
		t.Fatalf("content identity wrong: %v", e)
	}
	if e = os.Chmod(filepath.Join(dir, "input.json"), 0644); e != nil {
		t.Fatal(e)
	}
	fourth, e := ValidateDir(dir)
	if e != nil || third.Digest == fourth.Digest {
		t.Fatalf("mode missing from identity: %v", e)
	}
}

func TestPackageRejectsEscapesAndResources(t *testing.T) {
	tests := map[string]func(string) error{
		"symlink":           func(d string) error { return os.Symlink("/etc/passwd", filepath.Join(d, "leak")) },
		"directory symlink": func(d string) error { return os.Symlink(t.TempDir(), filepath.Join(d, "other")) },
		"hardlink":          func(d string) error { return os.Link(filepath.Join(d, "reducer"), filepath.Join(d, "alias")) },
		"fifo":              func(d string) error { return syscall.Mkfifo(filepath.Join(d, "pipe"), 0600) },
		"not executable":    func(d string) error { return os.Chmod(filepath.Join(d, "reducer"), 0600) },
		"script instead of ELF": func(d string) error {
			return os.WriteFile(filepath.Join(d, "reducer"), []byte("#!/bin/sh\ntouch SHOULD_NOT_EXIST\n"), 0755)
		},
		"missing manifest": func(d string) error { return os.Remove(filepath.Join(d, "manifest.json")) },
		"too many files": func(d string) error {
			for i := range MaxFiles {
				if e := os.WriteFile(filepath.Join(d, fmt.Sprintf("file-%04d", i)), nil, 0600); e != nil {
					return e
				}
			}
			return nil
		},
		"oversize file": func(d string) error {
			f, e := os.Create(filepath.Join(d, "huge"))
			if e != nil {
				return e
			}
			defer f.Close()
			return f.Truncate(MaxPackageBytes + 1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			dir := packageDir(t)
			if e := mutate(dir); e != nil {
				t.Fatal(e)
			}
			if _, e := ValidateDir(dir); e == nil {
				t.Fatal("accepted unsafe package")
			}
			if _, e := os.Stat(filepath.Join(dir, "SHOULD_NOT_EXIST")); e == nil {
				t.Fatal("executed package")
			}
		})
	}
	dir := packageDir(t)
	link := filepath.Join(t.TempDir(), "root")
	if e := os.Symlink(dir, link); e != nil {
		t.Fatal(e)
	}
	if _, e := ValidateDir(link); e == nil {
		t.Fatal("accepted linked root")
	}
}

func TestPackageDirectoryBoundsAndOrdering(t *testing.T) {
	t.Run("directory count", func(t *testing.T) {
		dir := packageDir(t)
		for i := range 513 {
			if e := os.Mkdir(filepath.Join(dir, fmt.Sprintf("dir-%04d", i)), 0700); e != nil {
				t.Fatal(e)
			}
		}
		if _, e := ValidateDir(dir); e == nil {
			t.Fatal("accepted excessive directory entries")
		}
	})
	t.Run("path depth", func(t *testing.T) {
		dir := packageDir(t)
		p := dir
		for range 18 {
			p = filepath.Join(p, "deep")
			if e := os.Mkdir(p, 0700); e != nil {
				t.Fatal(e)
			}
		}
		if _, e := ValidateDir(dir); e == nil {
			t.Fatal("accepted excessive nesting")
		}
	})
	t.Run("creation order", func(t *testing.T) {
		var digests []string
		for _, order := range [][]string{{"a/x", "a.z"}, {"a.z", "a/x"}} {
			dir := packageDir(t)
			if e := os.Mkdir(filepath.Join(dir, "a"), 0700); e != nil {
				t.Fatal(e)
			}
			for _, p := range order {
				if e := os.WriteFile(filepath.Join(dir, p), []byte(p), 0600); e != nil {
					t.Fatal(e)
				}
			}
			p, e := ValidateDir(dir)
			if e != nil {
				t.Fatal(e)
			}
			digests = append(digests, p.Digest)
		}
		if digests[0] != digests[1] {
			t.Fatal("digest depends on filesystem creation order")
		}
	})
}
