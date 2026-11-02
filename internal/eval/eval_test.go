package eval

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixture(t *testing.T) (Suite, []Result) {
	t.Helper()
	var s Suite
	var r []Result
	if err := Decode(strings.NewReader(`{"version":1,"cases":[{"name":"daily","provenance":"run reference-1, charter abc","inputs":{"records":[1,2]},"max_cost_usd":0.3,"checks":[{"pointer":"/report","op":"contains","value":"Total"},{"pointer":"/actions","op":"count","value":0},{"pointer":"/totals/a~1b/~0","op":"equals","value":9007199254740993}]}]}`), &s); err != nil {
		t.Fatal(err)
	}
	if err := Decode(strings.NewReader(`[{"name":"daily","cost_usd":0.1,"output":{"report":"Total 3","actions":[],"totals":{"a/b":{"~":9007199254740993}}}}]`), &r); err != nil {
		t.Fatal(err)
	}
	return s, r
}
func TestRegressionAndExactNumbers(t *testing.T) {
	s, r := fixture(t)
	report, err := Evaluate(s, r)
	if err != nil || !report.Passed || report.KnownCostUSD != "0.1" {
		t.Fatalf("%+v %v", report, err)
	}
	r[0].Output = json.RawMessage(`{"report":"Wrong","actions":[{}],"totals":{"a/b":{"~":9007199254740992}}}`)
	report, err = Evaluate(s, r)
	if err != nil || report.Passed || len(report.Cases[0].Failures) != 3 {
		t.Fatalf("%+v %v", report, err)
	}
}
func TestMissingNullAndArrayPointers(t *testing.T) {
	s, r := fixture(t)
	s.Cases[0].Checks = []Check{{Pointer: "/missing", Op: "equals", Value: json.RawMessage(`null`)}}
	report, err := Evaluate(s, r)
	if err != nil || report.Passed {
		t.Fatalf("missing null passed: %+v %v", report, err)
	}
	for _, p := range []string{"/x/00", "/x/-1", "/x/+0", "/x/1"} {
		_, ok := pointer(map[string]any{"x": []any{nil}}, p)
		if ok {
			t.Errorf("invalid array pointer %s", p)
		}
	}
}
func TestCostAccounting(t *testing.T) {
	s, r := fixture(t)
	other := s.Cases[0]
	other.Name = "second"
	s.Cases = append(s.Cases, other)
	r = append(r, r[0])
	r[1].Name = "second"
	v := json.Number("0.2")
	r[1].CostUSD = &v
	report, err := Evaluate(s, r)
	if err != nil || !report.Passed || report.KnownCostUSD != "0.3" {
		t.Fatalf("%+v %v", report, err)
	}
	r[1].CostUSD = nil
	report, err = Evaluate(s, r)
	if err != nil || report.Passed || report.MissingCosts != 1 {
		t.Fatalf("%+v %v", report, err)
	}
	v = json.Number("0.31")
	r[1].CostUSD = &v
	report, err = Evaluate(s, r)
	if err != nil || report.Passed {
		t.Fatalf("%+v %v", report, err)
	}
}
func TestRejectInvalidFixtures(t *testing.T) {
	for _, change := range []func(*Suite, *[]Result){
		func(s *Suite, r *[]Result) { s.Version = 2 }, func(s *Suite, r *[]Result) { s.Cases[0].Provenance = "" },
		func(s *Suite, r *[]Result) { s.Cases[0].Checks[0].Op = "bogus" }, func(s *Suite, r *[]Result) { s.Cases[0].Checks[0].Pointer = "/~2" },
		func(s *Suite, r *[]Result) { *r = append(*r, (*r)[0]) }, func(s *Suite, r *[]Result) { (*r)[0].Name = "unknown" },
		func(s *Suite, r *[]Result) { s.Cases[0].Checks[0].Value = json.RawMessage(`3`) },
	} {
		s, r := fixture(t)
		change(&s, &r)
		if _, err := Evaluate(s, r); err == nil {
			t.Fatal("accepted invalid suite")
		}
	}
}
func TestDecodeStrict(t *testing.T) {
	for _, s := range []string{`{"version":1,"typo":true}`, `{"version":1} {}`, `{"version":1} broken`} {
		var v Suite
		if err := Decode(strings.NewReader(s), &v); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}

func TestCostsBoundArithmeticAndMissingResultReportsZero(t *testing.T) {
	for _, value := range []string{"1e1000000000", "1e-1000000000", "-0.1", strings.Repeat("1", 1001)} {
		s, r := fixture(t)
		cost := json.Number(value)
		r[0].CostUSD = &cost
		if _, err := Evaluate(s, r); err == nil {
			t.Errorf("accepted unsafe cost %q", value)
		}
	}
	s, _ := fixture(t)
	report, err := Evaluate(s, nil)
	if err != nil || report.Passed || report.KnownCostUSD != "0" || report.MissingCosts != 1 {
		t.Fatalf("missing result: %+v %v", report, err)
	}
}
