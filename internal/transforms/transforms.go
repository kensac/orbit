// Package transforms is code that runs over a source's bytes before the
// model sees them (docs/ARCHITECTURE.md, Code first, model last): a named,
// tested function from a source to a smaller JSON document, stored as its
// own artifact and handed to the model as an input. Every transform is
// deterministic: the same bytes and parameters give the same output, and
// sums are exact decimal arithmetic, never floats.
package transforms

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/kensac/orbit/internal/harness"
)

// Kinds lists every transform kind, for validation.
var Kinds = []string{"json-filter", "json-project", "json-group-sum", "sure-monthly"}

// maxGroupRows bounds a group-sum's rows. A transform exists to make an
// input smaller; more groups than this is a grouping that did not.
const maxGroupRows = 10000

// Validate checks a kind's parameters, so a bad transform fails the load,
// not the run.
func Validate(kind string, params json.RawMessage) error {
	_, err := parse(kind, params)
	return err
}

// Apply runs kind with params over a source's bytes and returns the output
// document.
func Apply(kind string, params json.RawMessage, in []byte) ([]byte, error) {
	p, err := parse(kind, params)
	if err != nil {
		return nil, err
	}
	doc, err := harness.Decode(in)
	if err != nil {
		return nil, fmt.Errorf("the source is not JSON: %w", err)
	}
	var out any
	switch kind {
	case "json-filter":
		out, err = filter(doc, p)
	case "json-project":
		out, err = project(doc, p)
	case "json-group-sum":
		out, err = groupSum(doc, p)
	case "sure-monthly":
		out, err = sureMonthly(doc, p)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// Params are every kind's parameters; each kind requires and allows a
// subset (parse).
type Params struct {
	// Items is the dot path to the array of records; empty means the
	// document is the array (a paginated http source is).
	Items string         `json:"items,omitempty"`
	Where []harness.Cond `json:"where,omitempty"`
	// Fields are the dot paths json-project keeps.
	Fields []string `json:"fields,omitempty"`
	// GroupBy and Sum are json-group-sum's groups and summed fields.
	GroupBy []string `json:"group_by,omitempty"`
	Sum     []string `json:"sum,omitempty"`
}

func parse(kind string, raw json.RawMessage) (Params, error) {
	var p Params
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return p, fmt.Errorf("params: %w", err)
		}
	}
	for _, c := range p.Where {
		if c.Field == "" || !slices.Contains(harness.CondOps, c.Op) {
			return p, fmt.Errorf("params.where: each condition needs a field and an op in %v", harness.CondOps)
		}
	}
	switch kind {
	case "json-filter":
		if len(p.Where) == 0 {
			return p, errors.New("json-filter needs params.where")
		}
		if len(p.Fields)+len(p.GroupBy)+len(p.Sum) > 0 {
			return p, errors.New("json-filter takes only items and where")
		}
	case "json-project":
		if len(p.Fields) == 0 {
			return p, errors.New("json-project needs params.fields")
		}
		if len(p.GroupBy)+len(p.Sum) > 0 {
			return p, errors.New("json-project takes only items, where and fields")
		}
	case "json-group-sum":
		if len(p.GroupBy) == 0 {
			return p, errors.New("json-group-sum needs params.group_by")
		}
		if len(p.Fields) > 0 {
			return p, errors.New("json-group-sum takes only items, where, group_by and sum")
		}
	case "sure-monthly":
		if len(p.Where)+len(p.Fields)+len(p.GroupBy)+len(p.Sum) > 0 {
			return p, errors.New("sure-monthly takes only items")
		}
	default:
		return p, fmt.Errorf("unknown transform kind %q (want one of %v)", kind, Kinds)
	}
	return p, nil
}

// matching returns the records at p.Items that pass p.Where.
func matching(doc any, p Params) ([]any, error) {
	records, err := harness.Records("the source", doc, p.Items)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range records {
		ok, err := harness.Match(r, p.Where)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// filter keeps the records that pass every condition, whole.
func filter(doc any, p Params) (any, error) { return matching(doc, p) }

// project keeps the named fields of each matching record, keyed by their
// dot path; a record without a field has no key for it.
func project(doc any, p Params) (any, error) {
	records, err := matching(doc, p)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(records))
	for _, r := range records {
		row := map[string]any{}
		for _, f := range p.Fields {
			if v, ok := harness.Lookup(r, f); ok {
				row[f] = v
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// groupSum is the query tool's group and sum, run before the model:
// {"records", "matched", "rows": [{"group", "count", "sum(<field>)"}]}.
func groupSum(doc any, p Params) (any, error) {
	aggs := []harness.Agg{{Op: "count"}}
	for _, f := range p.Sum {
		aggs = append(aggs, harness.Agg{Op: "sum", Field: f})
	}
	res, err := harness.QueryDoc("the source", doc, harness.Query{Items: p.Items, Where: p.Where, GroupBy: p.GroupBy, Aggregates: aggs}, maxGroupRows)
	if err != nil {
		return nil, err
	}
	if res["truncated"] == true {
		return nil, fmt.Errorf("more than %d groups: group by fewer fields", maxGroupRows)
	}
	delete(res, "truncated")
	return res, nil
}
