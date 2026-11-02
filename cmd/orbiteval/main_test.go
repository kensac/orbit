package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestCommandRejectsUnsupportedNumbersWithoutRegressionReport(t *testing.T) {
	for _, expected := range []string{"1e1001", "1"} {
		dir := t.TempDir()
		suite := filepath.Join(dir, "suite.json")
		results := filepath.Join(dir, "results.json")
		if err := os.WriteFile(suite, []byte(`{"version":1,"cases":[{"name":"same-number","provenance":"synthetic exact comparison","inputs":{},"checks":[{"pointer":"/n","op":"equals","value":`+expected+`}]}]}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(results, []byte(`[{"name":"same-number","output":{"n":1e1001},"cost_usd":0}]`), 0600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := evaluateCmd([]string{"--suite", suite, "--results", results}, &out)
		if err == nil || !strings.Contains(err.Error(), "number exceeds exact comparison bounds") || out.Len() != 0 {
			t.Fatalf("wanted invalid input without a report, got %q, %v", out.String(), err)
		}
	}
}
