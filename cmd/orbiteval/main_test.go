package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCommandReportsAndReturnsRegressions(t *testing.T) {
	args := []string{"--suite", "testdata/suite.json", "--results", "testdata/results.json"}
	var out bytes.Buffer
	if err := evaluateCmd(args, &out); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Passed bool `json:"passed"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || !report.Passed {
		t.Fatalf("%s %v", out.String(), err)
	}
	bad := filepath.Join(t.TempDir(), "results.json")
	if err := os.WriteFile(bad, []byte(`[{"name":"quiet-day","output":{"actions":[{}]},"cost_usd":0.2}]`), 0600); err != nil {
		t.Fatal(err)
	}
	args[3] = bad
	out.Reset()
	if err := evaluateCmd(args, &out); err == nil {
		t.Fatal("regression returned success")
	}
	if !json.Valid(out.Bytes()) {
		t.Fatalf("missing regression report: %s", out.String())
	}
}
