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
	endpoint := fs.String("url", os.Getenv("ORBIT_URL"), "Orbit HTTPS URL (required for --replay)")
	issuer := fs.String("issuer", os.Getenv("ORBIT_OIDC_ISSUER"), "device provider issuer")
	clientID := fs.String("client-id", os.Getenv("ORBIT_OIDC_CLIENT_ID"), "public device provider client ID")
	agent := fs.String("agent", "", "current agent name (required for --replay)")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall replay timeout (at most 30 minutes)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *suitePath == "" || (!*replay && *resultsPath == "") || (*replay && (*agent == "" || *endpoint == "")) {
		return errors.New("usage: orbiteval --suite suite.json --results results.json | --replay --url https://orbit --agent name")
	}
	var suite eval.Suite
	var results []eval.Result
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
		results, err = replaySuite(suite, *endpoint, *issuer, *clientID, *agent, *timeout, os.Stderr)
		if err != nil {
			return err
		}
	} else {
		f, err := os.Open(*resultsPath)
		if err != nil {
			return err
		}
		err = eval.Decode(f, &results)
		closeErr := f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", *resultsPath, err)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	report, err := eval.Evaluate(suite, results)
	if err != nil {
		return err
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

func replaySuite(suite eval.Suite, endpoint, issuer, clientID, agent string, timeout time.Duration, notice io.Writer) ([]eval.Result, error) {
	if len(suite.Cases) == 0 || len(suite.Cases) > replayMaxCases {
		return nil, fmt.Errorf("replay supports 1 to %d cases", replayMaxCases)
	}
	totalInput := 0
	for _, c := range suite.Cases {
		if len(c.Inputs) > replayMaxInputBytes {
			return nil, fmt.Errorf("case %q input exceeds %d bytes", c.Name, replayMaxInputBytes)
		}
		totalInput += len(c.Inputs)
		if totalInput > replayMaxTotalBytes {
			return nil, fmt.Errorf("replay inputs exceed %d bytes", replayMaxTotalBytes)
		}
	}
	if err := client.ValidateURL(endpoint); err != nil {
		return nil, err
	}
	segment, err := client.Segment(agent)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 || timeout > 30*time.Minute {
		return nil, errors.New("replay timeout must be positive and at most 30 minutes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	hc := client.HTTPClientWithResponseTimeout(timeout)
	token, err := client.Login(ctx, hc, issuer, clientID, notice)
	if err != nil {
		return nil, err
	}
	c := client.Client{BaseURL: endpoint, Token: token, HTTP: hc}
	return replayCases(ctx, c, suite, segment)
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
