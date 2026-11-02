package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kensac/orbit/internal/eval"
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *suitePath == "" || *resultsPath == "" {
		return errors.New("usage: orbiteval --suite suite.json --results results.json")
	}
	var suite eval.Suite
	var results []eval.Result
	for _, file := range []struct {
		path  string
		value any
	}{{*suitePath, &suite}, {*resultsPath, &results}} {
		f, err := os.Open(file.path)
		if err != nil {
			return err
		}
		err = eval.Decode(f, file.value)
		closeErr := f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", file.path, err)
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
