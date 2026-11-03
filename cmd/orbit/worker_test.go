package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	if got := missingEnv(cfg, sure, func(k string) (string, bool) {
		if k == "CLAUDE_CODE_OAUTH_TOKEN" {
			return "token", true
		}
		if k == "SURE_API_KEY" {
			return "", true
		}
		return "", false
	}); !reflect.DeepEqual(got, []string{"SURE_API_KEY"}) {
		t.Fatalf("empty variable is missing: %v", got)
	}
	// The backend credential is either variable, and a worker with neither is told both names. The fixture is the test's own: one claude-code agent that needs nothing, so a bare worker covers it.
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte("defaults: {runner: r, channels: [c]}\nrunners: {r: {kind: claude-code}}\nchannels: {c: {kind: log}}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "a.yaml"), []byte("name: a\ncharter: x\n"), 0o644)
	own, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := missingEnv(own, bare, lookup("ANTHROPIC_API_KEY")); len(got) != 0 {
		t.Fatalf("an API key alone misses %v", got)
	}
	if got := missingEnv(own, bare, lookup()); !reflect.DeepEqual(got, []string{"CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY"}) {
		t.Fatalf("missing = %v, want both backend names", got)
	}
}

func TestWorkerSettingsDefaultsAndRejectsInvalidValues(t *testing.T) {
	lookup := func(values map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			v, ok := values[k]
			return v, ok
		}
	}
	workers, caps, err := workerSettings(lookup(nil))
	if err != nil || workers != 2 || caps.Any || len(caps.List) != 0 {
		t.Fatalf("defaults = %d, %+v, %v", workers, caps, err)
	}
	if _, _, err := workerSettings(lookup(map[string]string{"ORBIT_WORKERS": "0"})); err == nil {
		t.Fatal("zero workers accepted")
	}
	if _, _, err := workerSettings(lookup(map[string]string{"ORBIT_CAPABILITIES": "INVALID"})); err == nil {
		t.Fatal("invalid capability accepted")
	}
}

func TestValidateNamesBothBackendCredentials(t *testing.T) {
	cfg, err := spec.Load("../../examples")
	if err != nil {
		t.Fatal(err)
	}
	env := referencedEnv(cfg)
	if !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY") {
		t.Fatalf("environment = %v, want the claude-code runner's two alternatives", env)
	}
	if !slices.IsSorted(env) {
		t.Fatalf("environment = %v, not sorted", env)
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
