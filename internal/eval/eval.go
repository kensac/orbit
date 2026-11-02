// Package eval checks saved run results without executing a backend or fetching sources.
package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strconv"
	"strings"
)

// Suite keeps reference inputs and deterministic expectations together.
type Suite struct {
	Version int    `json:"version"`
	Cases   []Case `json:"cases"`
}

// Case names a saved run and the properties its result must retain.
type Case struct {
	Provenance string          `json:"provenance"`
	Name       string          `json:"name"`
	Inputs     json.RawMessage `json:"inputs"`
	Checks     []Check         `json:"checks"`
	MaxCostUSD *json.Number    `json:"max_cost_usd,omitempty"`
}

// Check applies one operation to an RFC 6901 pointer in the saved output.
// Supported operations are equals, contains (string), and count (array).
type Check struct {
	Pointer string          `json:"pointer"`
	Op      string          `json:"op"`
	Value   json.RawMessage `json:"value"`
}

// Result is one captured output; cost is optional when the backend did not report it.
type Result struct {
	Name    string          `json:"name"`
	Output  json.RawMessage `json:"output"`
	CostUSD *json.Number    `json:"cost_usd,omitempty"`
}

// Report contains every check failure and the exact sum of reported costs.
type Report struct {
	Passed       bool         `json:"passed"`
	Cases        []CaseReport `json:"cases"`
	KnownCostUSD string       `json:"known_cost_usd"`
	MissingCosts int          `json:"missing_costs"`
}

// CaseReport records one run's regressions without copying potentially sensitive output.
type CaseReport struct {
	Name     string   `json:"name"`
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures"`
}

// Decode reads exactly one strict JSON document. Unknown fields are errors.
func Decode(r io.Reader, v any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// Evaluate compares a complete result set to its suite. Invalid fixtures are errors, not passing evaluations.
func Evaluate(s Suite, results []Result) (Report, error) {
	report := Report{Passed: true, Cases: []CaseReport{}}
	if s.Version != 1 {
		return report, fmt.Errorf("unsupported suite version %d", s.Version)
	}
	if len(s.Cases) == 0 {
		return report, errors.New("suite has no cases")
	}
	byName := map[string]Result{}
	for _, r := range results {
		if _, ok := byName[r.Name]; ok {
			return report, fmt.Errorf("duplicate result %q", r.Name)
		}
		byName[r.Name] = r
	}
	names := map[string]bool{}
	total := new(big.Rat)
	for _, c := range s.Cases {
		if c.Name == "" || names[c.Name] {
			return report, fmt.Errorf("empty or duplicate case name %q", c.Name)
		}
		names[c.Name] = true
		if strings.TrimSpace(c.Provenance) == "" {
			return report, fmt.Errorf("case %q: provenance is required", c.Name)
		}
		if len(c.Inputs) == 0 || !json.Valid(c.Inputs) {
			return report, fmt.Errorf("case %q: inputs must be JSON", c.Name)
		}
		if len(c.Checks) == 0 {
			return report, fmt.Errorf("case %q: no checks", c.Name)
		}
		var maxCost *big.Rat
		if c.MaxCostUSD != nil {
			var err error
			maxCost, err = money(*c.MaxCostUSD)
			if err != nil {
				return report, fmt.Errorf("case %q max_cost_usd: %w", c.Name, err)
			}
		}
		// Validate all checks even if the candidate is missing.
		expected := make([]any, len(c.Checks))
		for i, ch := range c.Checks {
			var err error
			expected[i], err = validate(ch)
			if err != nil {
				return report, fmt.Errorf("case %q check %d: %w", c.Name, i+1, err)
			}
		}
		cr := CaseReport{Name: c.Name, Passed: true, Failures: []string{}}
		fail := func(s string) { cr.Passed = false; report.Passed = false; cr.Failures = append(cr.Failures, s) }
		r, ok := byName[c.Name]
		if !ok {
			fail("missing result")
			report.MissingCosts++
			report.Cases = append(report.Cases, cr)
			continue
		}
		var output any
		if err := Decode(bytes.NewReader(r.Output), &output); err != nil {
			return report, fmt.Errorf("result %q output: %w", c.Name, err)
		}
		if r.CostUSD == nil {
			report.MissingCosts++
			if maxCost != nil {
				fail("cost unavailable for budget check")
			}
		} else {
			cost, err := money(*r.CostUSD)
			if err != nil {
				return report, fmt.Errorf("result %q cost_usd: %w", c.Name, err)
			}
			total.Add(total, cost)
			if maxCost != nil && cost.Cmp(maxCost) > 0 {
				fail("cost exceeds max_cost_usd")
			}
		}
		for i, ch := range c.Checks {
			actual, exists := pointer(output, ch.Pointer)
			passed := exists
			if exists {
				switch ch.Op {
				case "equals":
					passed = equal(actual, expected[i])
				case "contains":
					v, ok := actual.(string)
					passed = ok && strings.Contains(v, expected[i].(string))
				case "count":
					v, ok := actual.([]any)
					passed = ok && equal(json.Number(strconv.Itoa(len(v))), expected[i])
				}
			}
			if !passed {
				fail(fmt.Sprintf("check %d: %s %s failed", i+1, ch.Pointer, ch.Op))
			}
		}
		report.Cases = append(report.Cases, cr)
	}
	for name := range byName {
		if !names[name] {
			return report, fmt.Errorf("unexpected result %q", name)
		}
	}
	report.KnownCostUSD = decimal(total)
	return report, nil
}

func money(n json.Number) (*big.Rat, error) {
	// JSON decimals have finite precision, so their exact sum also has finite precision.
	if !json.Valid([]byte(n.String())) {
		return nil, errors.New("invalid number")
	}
	v, ok := number(n.String())
	if !ok || v.Sign() < 0 {
		return nil, errors.New("must be a nonnegative decimal with at most 1000 digits and an exponent between -1000 and 1000")
	}
	return v, nil
}
func decimal(r *big.Rat) string {
	d := new(big.Int).Set(r.Denom())
	two, five := 0, 0
	rem := new(big.Int)
	for {
		q := new(big.Int)
		q.QuoRem(d, big.NewInt(2), rem)
		if rem.Sign() != 0 {
			break
		}
		d = q
		two++
	}
	for {
		q := new(big.Int)
		q.QuoRem(d, big.NewInt(5), rem)
		if rem.Sign() != 0 {
			break
		}
		d = q
		five++
	}
	n := max(two, five)
	s := r.FloatString(n)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}
func validate(c Check) (any, error) {
	if c.Pointer != "" && !strings.HasPrefix(c.Pointer, "/") {
		return nil, errors.New("pointer must be empty or begin with /")
	}
	for i := 0; i < len(c.Pointer); i++ {
		if c.Pointer[i] == '~' {
			if i+1 >= len(c.Pointer) || (c.Pointer[i+1] != '0' && c.Pointer[i+1] != '1') {
				return nil, errors.New("invalid pointer escape")
			}
			i++
		}
	}
	var v any
	if err := Decode(bytes.NewReader(c.Value), &v); err != nil {
		return nil, fmt.Errorf("value: %w", err)
	}
	switch c.Op {
	case "equals":
	case "contains":
		if _, ok := v.(string); !ok {
			return nil, errors.New("contains needs a string value")
		}
	case "count":
		n, ok := v.(json.Number)
		if !ok {
			return nil, errors.New("count needs a nonnegative integer")
		}
		i, err := strconv.ParseUint(n.String(), 10, 64)
		if err != nil || i > uint64(^uint(0)>>1) {
			return nil, errors.New("count needs a nonnegative integer")
		}
	default:
		return nil, fmt.Errorf("unknown operation %q", c.Op)
	}
	return v, nil
}
func equal(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		xr, xok := number(x.String())
		yr, yok := number(y.String())
		return xok && yok && xr.Cmp(yr) == 0
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !equal(v, w) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}
func pointer(v any, p string) (any, bool) {
	if p == "" {
		return v, true
	}
	for _, part := range strings.Split(p[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = x[part]
			if !ok {
				return nil, false
			}
		case []any:
			if part == "" || strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 || (len(part) > 1 && part[0] == '0') {
				return nil, false
			}
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// number bounds exact arithmetic to fixture-sized decimals, including scientific notation.
func number(s string) (*big.Rat, bool) {
	// A thousand decimal places covers reported costs without unbounded big integer allocations.
	const maxPlaces = 1000
	if len(s) > maxPlaces {
		return nil, false
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(s[i+1:])
		if err != nil || exponent < -maxPlaces || exponent > maxPlaces {
			return nil, false
		}
	}
	return new(big.Rat).SetString(s)
}
