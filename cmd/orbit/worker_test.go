package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kensac/orbit/internal/spec"
)

func TestAWorkerNamesTheCredentialsItLacks(t *testing.T) {
	cfg, err := spec.Load("../../examples")
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(have ...string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			for _, h := range have {
				if h == k {
					return "x", true
				}
			}
			return "", false
		}
	}
	bare, _ := spec.ParseCapabilities("")
	// finance needs sure: a bare worker never claims it, so its key is not this worker's concern.
	if got := missingEnv(cfg, bare, lookup("CLAUDE_CODE_OAUTH_TOKEN")); len(got) != 0 {
		t.Fatalf("bare worker misses %v", got)
	}
	sure, _ := spec.ParseCapabilities("sure,docker-network:hetzner")
	if got := missingEnv(cfg, sure, lookup("CLAUDE_CODE_OAUTH_TOKEN")); !reflect.DeepEqual(got, []string{"SURE_API_KEY"}) {
		t.Fatalf("missing = %v, want the Sure key", got)
	}
}

func TestTLSCertIsWrittenOnce(t *testing.T) {
	dir := t.TempDir()
	if err := tlsCertCmd([]string{"--dir", dir}); err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(filepath.Join(dir, "server.key"))
	if err := tlsCertCmd([]string{"--dir", dir}); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(dir, "server.key"))
	if len(key) == 0 || string(key) != string(again) {
		t.Fatal("the key must be kept, or every restart changes the certificate")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "server.crt"))
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatal("no certificate")
	}
	if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "server.key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v: postgres refuses a key others can read", fi.Mode().Perm())
	}
}
