package transforms

import (
	"encoding/json"
	"strings"
	"testing"
)

const txns = `{"transactions": [
  {"id": 1, "date": "2026-10-03", "amount": "-$1,250.00", "classification": "income", "category": {"name": "Income"}},
  {"id": 2, "date": "2026-10-04", "amount_cents": 1999, "classification": "expense", "category": {"name": "Dining"}},
  {"id": 3, "date": "2026-10-09", "amount": 10.01, "classification": "expense", "category": {"name": "Dining"}},
  {"id": 4, "date": "2026-10-10", "amount": "$500.00", "classification": "expense", "category": null, "transfer": {"other_account": {"account_type": "credit_card"}}},
  {"id": 5, "date": "2026-10-11", "amount": "$42.10", "classification": "expense"},
  {"id": 6, "date": "2026-09-30", "amount": "$0.10", "classification": "expense", "category": {"name": "Fees"}, "transfer": false}
]}`

func apply(t *testing.T, kind, params, in string) string {
	t.Helper()
	out, err := Apply(kind, json.RawMessage(params), []byte(in))
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
	return string(out)
}

func TestSureMonthly(t *testing.T) {
	got := apply(t, "sure-monthly", `{"items": "transactions"}`, txns)
	want := `{"months":[` +
		`{"month":"2026-09","transactions":1,"transfers":0,"income":"0.00","spending":"0.10","net":"-0.10",` +
		`"categories":[{"category":"Fees","spending":"0.10","income":"0.00","count":1}]},` +
		`{"month":"2026-10","transactions":4,"transfers":1,"income":"1250.00","spending":"72.10","net":"1177.90",` +
		`"categories":[{"category":"Uncategorized","spending":"42.10","income":"0.00","count":1},` +
		`{"category":"Dining","spending":"30.00","income":"0.00","count":2},` +
		`{"category":"Income","spending":"0.00","income":"1250.00","count":1}]}],"transactions":6}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Deterministic: the same bytes give the same output.
	if again := apply(t, "sure-monthly", `{"items": "transactions"}`, txns); again != got {
		t.Fatal("output changed between runs")
	}
	if _, err := Apply("sure-monthly", nil, []byte(`[{"date": "2026-10-01", "amount": "1.005"}]`)); err == nil || !strings.Contains(err.Error(), "whole cents") {
		t.Fatalf("fractional cents = %v", err)
	}
	if _, err := Apply("sure-monthly", nil, []byte(`[{"date": "Oct 1", "amount": 1}]`)); err == nil {
		t.Fatal("a bad date must fail, not be dropped")
	}
}

func TestJSONFilterProjectGroupSum(t *testing.T) {
	where := `"where": [{"field": "classification", "op": "eq", "value": "expense"}]`
	got := apply(t, "json-filter", `{"items": "transactions", `+where+`}`, txns)
	var recs []map[string]any
	_ = json.Unmarshal([]byte(got), &recs)
	if len(recs) != 5 {
		t.Fatalf("filter kept %d, want 5: %s", len(recs), got)
	}
	got = apply(t, "json-project", `{"items": "transactions", `+where+`, "fields": ["id", "category.name"]}`, txns)
	if !strings.HasPrefix(got, `[{"category.name":"Dining","id":2},{"category.name":"Dining","id":3},{"id":4}`) {
		t.Fatalf("project = %s", got)
	}
	got = apply(t, "json-group-sum", `{"items": "transactions", "where": [{"field": "amount_cents", "op": "exists"}], "group_by": ["category.name"], "sum": ["amount_cents"]}`, txns)
	if got != `{"matched":1,"records":6,"rows":[{"count":1,"group":{"category.name":"Dining"},"sum(amount_cents)":"1999"}]}` {
		t.Fatalf("group-sum = %s", got)
	}
}

func TestValidateRefusesBadParams(t *testing.T) {
	for kind, params := range map[string]string{
		"json-filter":    `{}`,
		"json-project":   `{"where": [{"field": "x", "op": "eq", "value": 1}]}`,
		"json-group-sum": `{"sum": ["x"]}`,
		"sure-monthly":   `{"fields": ["x"]}`,
		"nope":           `{}`,
	} {
		if err := Validate(kind, json.RawMessage(params)); err == nil {
			t.Errorf("%s %s: want an error", kind, params)
		}
	}
	if err := Validate("json-filter", json.RawMessage(`{"where": [{"field": "x", "op": "like"}]}`)); err == nil {
		t.Error("an unknown op must fail")
	}
	if err := Validate("json-filter", json.RawMessage(`{"where": [{"field": "x", "op": "eq"}], "typo": 1}`)); err == nil {
		t.Error("an unknown parameter must fail")
	}
	if err := Validate("sure-monthly", nil); err != nil {
		t.Errorf("sure-monthly without params = %v", err)
	}
}
