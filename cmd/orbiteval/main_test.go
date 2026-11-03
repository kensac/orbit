package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/client"
	"github.com/kensac/orbit/internal/eval"
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

func TestReplayRejectsInvalidSuiteBeforeDeviceLogin(t *testing.T) {
	suite := filepath.Join(t.TempDir(), "suite.json")
	if err := os.WriteFile(suite, []byte(`{"version":1,"cases":[{"name":"bad","provenance":"test","inputs":{},"checks":[{"pointer":"/x","op":"unknown","value":null}]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := evaluateCmd([]string{"--suite", suite, "--replay", "--url", "https://example.test", "--agent", "finance"}, &out)
	if err == nil || !strings.Contains(err.Error(), "unknown operation") || out.Len() != 0 {
		t.Fatalf("invalid suite replay = %q, %v", out.String(), err)
	}
}

func TestReplayCasesUsesOneCurrentAgentRequestPerCase(t *testing.T) {
	var calls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/agents/finance/replay" || r.Header.Get("Authorization") != "Bearer current-token" {
			t.Errorf("request = %s %s, authorization %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		body, readErr := io.ReadAll(r.Body)
		var request struct {
			Cases []struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"cases"`
		}
		if err := json.Unmarshal(body, &request); readErr != nil || err != nil || len(request.Cases) != 2 || request.Cases[1].Name != "second" {
			t.Errorf("request body = %s, read err %v, decode err %v", body, readErr, err)
		}
		_, _ = fmt.Fprint(w, `{"results":[{"name":"first","output":{"summary":"ok"},"cost_usd":0.1},{"name":"second","output":{"summary":"ok"},"cost_usd":0.125}]}`)
	}))
	defer server.Close()
	cases := eval.Suite{Version: 1, Cases: []eval.Case{
		{Name: "first", Inputs: json.RawMessage(`{"n":1}`)},
		{Name: "second", Inputs: json.RawMessage(`{"n":2}`)},
	}}
	results, err := replayCases(context.Background(), client.Client{BaseURL: server.URL, Token: "current-token", HTTP: server.Client()}, cases, "finance")
	if err != nil || len(results) != 2 || calls != 1 || results[1].Name != "second" || results[1].CostUSD == nil || results[1].CostUSD.String() != "0.125" {
		t.Fatalf("replay results = %+v, calls %d, err %v", results, calls, err)
	}
}

func TestGradeCasesSendsOnlyRubricAndCandidateToSeparateAgent(t *testing.T) {
	var calls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/agents/evaluator/grade" || r.Header.Get("Authorization") != "Bearer current-token" {
			t.Errorf("request = %s %s, authorization %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		body, readErr := io.ReadAll(r.Body)
		var request struct {
			Target string `json:"target_agent"`
			Cases  []struct {
				Name   string          `json:"name"`
				Rubric string          `json:"rubric"`
				Output json.RawMessage `json:"output"`
			} `json:"cases"`
		}
		if err := json.Unmarshal(body, &request); readErr != nil || err != nil || request.Target != "finance" || len(request.Cases) != 1 || request.Cases[0].Rubric != "mentions total" || string(request.Cases[0].Output) != `{"summary":"$4"}` {
			t.Errorf("grade request = %s, read err %v, decode err %v", body, readErr, err)
		}
		_, _ = fmt.Fprint(w, `{"grades":[{"name":"one","passed":true,"reason":"total present","cost_usd":0.125}]}`)
	}))
	defer server.Close()
	suite := eval.Suite{Version: 1, Cases: []eval.Case{{Name: "one", Grade: "mentions total"}}}
	results := []eval.Result{{Name: "one", Output: json.RawMessage(`{"summary":"$4"}`)}}
	grades, err := gradeCases(context.Background(), client.Client{BaseURL: server.URL, Token: "current-token", HTTP: server.Client()}, suite, results, "evaluator", "finance")
	if err != nil || len(grades) != 1 || grades[0].Name != "one" || !grades[0].Passed || grades[0].CostUSD == nil || grades[0].CostUSD.String() != "0.125" || calls != 1 {
		t.Fatalf("grades = %+v, calls %d, err %v", grades, calls, err)
	}
}
