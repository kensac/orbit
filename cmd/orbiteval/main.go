package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/kensac/orbit/internal/client"
	"github.com/kensac/orbit/internal/eval"
)

const (
	replayMaxCases      = 32
	replayMaxInputBytes = 8 << 20
	replayMaxTotalBytes = 32 << 20
)

func main() {
	if err := evaluateCmd(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "orbiteval:", err)
		os.Exit(1)
	}
}

func evaluateCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("orbiteval", flag.ContinueOnError)
	suitePath := fs.String("suite", "", "saved reference suite JSON")
	resultsPath := fs.String("results", "", "captured results JSON array")
	replay := fs.Bool("replay", false, "run suite cases through the current agent charter and skills")
	grader := fs.String("grade-with", "", "independent configured agent to grade rubric cases after replay")
	endpoint := fs.String("url", os.Getenv("ORBIT_URL"), "Orbit HTTPS URL (required for --replay)")
	issuer := fs.String("issuer", os.Getenv("ORBIT_OIDC_ISSUER"), "device provider issuer")
	clientID := fs.String("client-id", os.Getenv("ORBIT_OIDC_CLIENT_ID"), "public device provider client ID")
	agent := fs.String("agent", "", "current agent name (required for --replay)")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall replay timeout (at most 30 minutes)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *suitePath == "" || (!*replay && *resultsPath == "") || (*replay && (*agent == "" || *endpoint == "")) || (*grader != "" && !*replay) || (*grader != "" && *grader == *agent) {
		return errors.New("usage: orbiteval --suite suite.json --results results.json | --replay --url https://orbit --agent name")
	}
	var suite eval.Suite
	var report eval.Report
	f, err := os.Open(*suitePath)
	if err != nil {
		return err
	}
	err = eval.Decode(f, &suite)
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", *suitePath, err)
	}
	if closeErr != nil {
		return closeErr
	}
	if *replay {
		if _, err := eval.Evaluate(suite, nil); err != nil {
			return err
		}
		var err error
		report, err = replaySuiteWithGrades(suite, *endpoint, *issuer, *clientID, *agent, *grader, *timeout, os.Stderr)
		if err != nil {
			return err
		}
	} else {
		f, err := os.Open(*resultsPath)
		if err != nil {
			return err
		}
		var results []eval.Result
		err = eval.Decode(f, &results)
		closeErr := f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", *resultsPath, err)
		}
		if closeErr != nil {
			return closeErr
		}
		report, err = eval.Evaluate(suite, results)
		if err != nil {
			return err
		}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return err
	}
	if !report.Passed {
		return errors.New("evaluation regressions found")
	}
	return nil
}

func replaySuiteWithGrades(suite eval.Suite, endpoint, issuer, clientID, agent, grader string, timeout time.Duration, notice io.Writer) (eval.Report, error) {
	if len(suite.Cases) == 0 || len(suite.Cases) > replayMaxCases {
		return eval.Report{}, fmt.Errorf("replay supports 1 to %d cases", replayMaxCases)
	}
	totalInput := 0
	for _, c := range suite.Cases {
		if len(c.Inputs) > replayMaxInputBytes {
			return eval.Report{}, fmt.Errorf("case %q input exceeds %d bytes", c.Name, replayMaxInputBytes)
		}
		totalInput += len(c.Inputs)
		if totalInput > replayMaxTotalBytes {
			return eval.Report{}, fmt.Errorf("replay inputs exceed %d bytes", replayMaxTotalBytes)
		}
	}
	if err := client.ValidateURL(endpoint); err != nil {
		return eval.Report{}, err
	}
	segment, err := client.Segment(agent)
	if err != nil {
		return eval.Report{}, err
	}
	graderSegment := ""
	if grader != "" {
		graderSegment, err = client.Segment(grader)
		if err != nil {
			return eval.Report{}, err
		}
		if grader == agent {
			return eval.Report{}, errors.New("grader must be a different configured agent than the replay target")
		}
		hasRubric := false
		for _, c := range suite.Cases {
			hasRubric = hasRubric || c.Grade != ""
		}
		if !hasRubric {
			return eval.Report{}, errors.New("--grade-with requires at least one case with a grade rubric")
		}
	}
	if timeout <= 0 || timeout > 30*time.Minute {
		return eval.Report{}, errors.New("replay timeout must be positive and at most 30 minutes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	hc := client.HTTPClientWithResponseTimeout(timeout)
	token, err := client.Login(ctx, hc, issuer, clientID, notice)
	if err != nil {
		return eval.Report{}, err
	}
	c := client.Client{BaseURL: endpoint, Token: token, HTTP: hc}
	results, err := replayCases(ctx, c, suite, segment)
	if err != nil {
		return eval.Report{}, err
	}
	report, err := eval.Evaluate(suite, results)
	if err != nil || graderSegment == "" {
		return report, err
	}
	grades, err := gradeCases(ctx, c, suite, results, graderSegment, agent)
	if err != nil {
		return eval.Report{}, err
	}
	if err := eval.ApplyGrades(suite, &report, grades); err != nil {
		return eval.Report{}, err
	}
	return report, nil
}

func gradeCases(ctx context.Context, c client.Client, suite eval.Suite, results []eval.Result, grader, target string) ([]eval.GradeResult, error) {
	byName := make(map[string]eval.Result, len(results))
	for _, result := range results {
		byName[result.Name] = result
	}
	type gradeCase struct {
		Name   string          `json:"name"`
		Rubric string          `json:"rubric"`
		Output json.RawMessage `json:"output"`
	}
	cases := make([]gradeCase, 0)
	for _, test := range suite.Cases {
		if test.Grade == "" {
			continue
		}
		result, ok := byName[test.Name]
		if !ok {
			return nil, fmt.Errorf("missing replay result for graded case %q", test.Name)
		}
		cases = append(cases, gradeCase{Name: test.Name, Rubric: test.Grade, Output: result.Output})
	}
	body, err := json.Marshal(struct {
		Target string      `json:"target_agent"`
		Cases  []gradeCase `json:"cases"`
	}{Target: target, Cases: cases})
	if err != nil {
		return nil, err
	}
	var response struct {
		Grades []struct {
			Name    string   `json:"name"`
			Passed  bool     `json:"passed"`
			Reason  string   `json:"reason"`
			CostUSD *float64 `json:"cost_usd"`
		} `json:"grades"`
	}
	var raw bytes.Buffer
	if err := c.Request(ctx, http.MethodPost, "agents/"+grader+"/grade", bytes.NewReader(body), &raw); err != nil {
		return nil, err
	}
	if err := eval.Decode(&raw, &response); err != nil {
		return nil, fmt.Errorf("grade response: %w", err)
	}
	if len(response.Grades) != len(cases) {
		return nil, errors.New("grade response is incomplete")
	}
	grades := make([]eval.GradeResult, 0, len(response.Grades))
	for i, g := range response.Grades {
		if g.Name != cases[i].Name {
			return nil, fmt.Errorf("grade result %d names %q, want %q", i+1, g.Name, cases[i].Name)
		}
		var cost *json.Number
		if g.CostUSD != nil {
			n := json.Number(strconv.FormatFloat(*g.CostUSD, 'f', -1, 64))
			cost = &n
		}
		grades = append(grades, eval.GradeResult{Name: g.Name, Passed: g.Passed, Reason: g.Reason, CostUSD: cost})
	}
	return grades, nil
}

func replayCases(ctx context.Context, c client.Client, suite eval.Suite, agent string) ([]eval.Result, error) {
	type replayCase struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	cases := make([]replayCase, 0, len(suite.Cases))
	for _, test := range suite.Cases {
		cases = append(cases, replayCase{Name: test.Name, Input: test.Inputs})
	}
	body, err := json.Marshal(struct {
		Cases []replayCase `json:"cases"`
	}{Cases: cases})
	if err != nil {
		return nil, err
	}
	var response struct {
		Results []eval.Result `json:"results"`
	}
	var raw bytes.Buffer
	path := "agents/" + agent + "/replay"
	if err := c.Request(ctx, http.MethodPost, path, bytes.NewReader(body), &raw); err != nil {
		return nil, err
	}
	if err := eval.Decode(&raw, &response); err != nil {
		return nil, fmt.Errorf("replay response: %w", err)
	}
	if len(response.Results) != len(suite.Cases) {
		return nil, fmt.Errorf("replay returned %d results for %d cases", len(response.Results), len(suite.Cases))
	}
	for i := range response.Results {
		if response.Results[i].Name != suite.Cases[i].Name {
			return nil, fmt.Errorf("replay result %d names %q, want %q", i+1, response.Results[i].Name, suite.Cases[i].Name)
		}
	}
	results := response.Results
	return results, nil
}
