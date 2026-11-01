// Package harness is orbit code the model's own process runs: the `orbit
// mcp` tool server and the `orbit hook` command. Both start inside the CLI's
// process tree, so they get its environment (runner.childEnv, no orbit
// secret) and read only under the run's workdir, which is all orbit wrote
// for them. Every result is a function of the inputs alone: the same files
// and arguments give the same bytes.
package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxQueryFile bounds a file the query tool reads: the largest source
// orbit fetches (spec's default MaxBytes, 8 MB), with room.
const maxQueryFile = 16 << 20

// maxQueryRows bounds the rows a query returns, so one call cannot flood
// the model's context; group or filter to see more.
const maxQueryRows = 200

// Query is the query tool's input.
type Query struct {
	// File is a JSON file under the workdir, as the prompt lists it.
	File string `json:"file"`
	// Items is a dot path to the array of records (for example
	// "transactions"); empty means the document is the array.
	Items string `json:"items,omitempty"`
	Where []Cond `json:"where,omitempty"`
	// GroupBy lists fields to group on; empty gives one group of all
	// matching records.
	GroupBy []string `json:"group_by,omitempty"`
	// Aggregates default to count.
	Aggregates []Agg `json:"aggregates,omitempty"`
	// Select returns these fields of each matching record instead of
	// aggregating (at most maxQueryRows).
	Select []string `json:"select,omitempty"`
	Limit  int      `json:"limit,omitempty"`
}

// Cond is one filter: field op value. Numbers compare exactly as numbers,
// anything else as text.
type Cond struct {
	Field string          `json:"field"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value"`
}

// Agg is count, sum, min or max of a numeric field (count needs none).
type Agg struct {
	Op    string `json:"op"`
	Field string `json:"field,omitempty"`
}

// QuerySchema is the tool's input schema, shown to the model.
var QuerySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"file":  map[string]any{"type": "string", "description": "A JSON input file, as the prompt lists it."},
		"items": map[string]any{"type": "string", "description": "Dot path to the array of records, e.g. \"transactions\". Omit when the file is the array."},
		"where": map[string]any{"type": "array", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"field": map[string]any{"type": "string", "description": "Dot path within a record."},
				"op":    map[string]any{"type": "string", "enum": []string{"eq", "ne", "lt", "lte", "gt", "gte", "contains", "in", "exists"}},
				"value": map[string]any{"description": "A number, a string, or for in a list."},
			},
			"required": []string{"field", "op"},
		}},
		"group_by": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"aggregates": map[string]any{"type": "array", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":    map[string]any{"type": "string", "enum": []string{"count", "sum", "min", "max"}},
				"field": map[string]any{"type": "string"},
			},
			"required": []string{"op"},
		}},
		"select": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": maxQueryRows},
	},
	"required": []string{"file"},
}

// RunQuery answers q over a file under root. Sums are exact: numbers are
// read as the decimal literals they are, never as floats.
func RunQuery(root string, q Query) (map[string]any, error) {
	path, err := within(root, q.File)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxQueryFile {
		return nil, fmt.Errorf("%s is %d bytes, over the %d the query tool reads", q.File, info.Size(), maxQueryFile)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s is not JSON: %w", q.File, err)
	}
	if q.Items != "" {
		v, ok := lookup(doc, q.Items)
		if !ok {
			return nil, fmt.Errorf("%s has no %q", q.File, q.Items)
		}
		doc = v
	}
	records, ok := doc.([]any)
	if !ok {
		return nil, fmt.Errorf("%s%s is not an array of records; name the array with items", q.File, suffix(q.Items))
	}
	var matched []any
	for _, r := range records {
		keep := true
		for _, c := range q.Where {
			ok, err := c.match(r)
			if err != nil {
				return nil, err
			}
			if !ok {
				keep = false
				break
			}
		}
		if keep {
			matched = append(matched, r)
		}
	}
	limit := q.Limit
	if limit <= 0 || limit > maxQueryRows {
		limit = maxQueryRows
	}
	out := map[string]any{"records": len(records), "matched": len(matched)}
	if len(q.Select) > 0 {
		var rows []map[string]any
		for _, r := range matched {
			if len(rows) == limit {
				break
			}
			row := map[string]any{}
			for _, f := range q.Select {
				if v, ok := lookup(r, f); ok {
					row[f] = v
				}
			}
			rows = append(rows, row)
		}
		out["rows"], out["truncated"] = rows, len(matched) > len(rows)
		return out, nil
	}
	aggs := q.Aggregates
	if len(aggs) == 0 {
		aggs = []Agg{{Op: "count"}}
	}
	groups := map[string]*group{}
	var order []string
	for _, r := range matched {
		key, vals := groupKey(r, q.GroupBy)
		g := groups[key]
		if g == nil {
			g = &group{by: vals, acc: make([]*acc, len(aggs))}
			for i := range aggs {
				g.acc[i] = &acc{}
			}
			groups[key] = g
			order = append(order, key)
		}
		for i, a := range aggs {
			if err := g.acc[i].add(a, r); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(order)
	var rows []map[string]any
	for _, k := range order {
		if len(rows) == limit {
			break
		}
		g := groups[k]
		row := map[string]any{}
		if len(q.GroupBy) > 0 {
			by := map[string]any{}
			for i, f := range q.GroupBy {
				by[f] = g.by[i]
			}
			row["group"] = by
		}
		for i, a := range aggs {
			if a.Field != "" && g.acc[i].n == 0 && a.Op != "count" {
				// A total of nothing is a misspelt field far more often
				// than a real zero; saying so beats a plausible "0".
				return nil, fmt.Errorf("%s: no matching record has a number at %q", a.name(), a.Field)
			}
			row[a.name()] = g.acc[i].result(a)
		}
		rows = append(rows, row)
	}
	out["rows"], out["truncated"] = rows, len(order) > len(rows)
	return out, nil
}

func suffix(items string) string {
	if items == "" {
		return ""
	}
	return " at " + items
}

type group struct {
	by  []any
	acc []*acc
}

type acc struct {
	n     int
	sum   *big.Rat
	scale int
	best  *big.Rat
	bestS string
}

func (a Agg) name() string {
	if a.Field == "" {
		return a.Op
	}
	return a.Op + "(" + a.Field + ")"
}

func (x *acc) add(a Agg, r any) error {
	if a.Op == "count" && a.Field == "" {
		x.n++
		return nil
	}
	v, ok := lookup(r, a.Field)
	if !ok || v == nil {
		return nil // a record without the field does not count toward it
	}
	if a.Op == "count" {
		x.n++
		return nil
	}
	n, ok := v.(json.Number)
	if !ok {
		return fmt.Errorf("%s of %q: value %v is not a number", a.Op, a.Field, v)
	}
	rat, ok := new(big.Rat).SetString(n.String())
	if !ok {
		return fmt.Errorf("%s of %q: cannot read %s", a.Op, a.Field, n)
	}
	x.n++
	x.scale = max(x.scale, decimals(n.String()))
	switch a.Op {
	case "sum":
		if x.sum == nil {
			x.sum = new(big.Rat)
		}
		x.sum.Add(x.sum, rat)
	case "min", "max":
		if x.best == nil || (a.Op == "min" && rat.Cmp(x.best) < 0) || (a.Op == "max" && rat.Cmp(x.best) > 0) {
			x.best, x.bestS = rat, n.String()
		}
	default:
		return fmt.Errorf("unknown aggregate %q (want count, sum, min or max)", a.Op)
	}
	return nil
}

func (x *acc) result(a Agg) any {
	switch a.Op {
	case "count":
		return x.n
	case "sum":
		if x.sum == nil {
			return "0"
		}
		return x.sum.FloatString(x.scale)
	default:
		if x.best == nil {
			return nil
		}
		return x.bestS
	}
}

// decimals counts the digits after the point in a JSON number literal
// (an exponent makes it 0: such a literal is not a money amount).
func decimals(s string) int {
	if strings.ContainsAny(s, "eE") {
		return 0
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}

func groupKey(r any, fields []string) (string, []any) {
	vals := make([]any, len(fields))
	parts := make([]string, len(fields))
	for i, f := range fields {
		v, _ := lookup(r, f)
		vals[i] = v
		b, _ := json.Marshal(v)
		parts[i] = string(b)
	}
	return strings.Join(parts, "\x00"), vals
}

func (c Cond) match(r any) (bool, error) {
	v, ok := lookup(r, c.Field)
	if c.Op == "exists" {
		return ok && v != nil, nil
	}
	if !ok {
		return c.Op == "ne", nil
	}
	dec := json.NewDecoder(bytes.NewReader(c.Value))
	dec.UseNumber()
	var want any
	if len(c.Value) > 0 {
		if err := dec.Decode(&want); err != nil {
			return false, fmt.Errorf("where %s: value is not JSON: %w", c.Field, err)
		}
	}
	switch c.Op {
	case "eq":
		return compare(v, want) == 0, nil
	case "ne":
		return compare(v, want) != 0, nil
	case "lt", "lte", "gt", "gte":
		cmp := compare(v, want)
		return map[string]bool{"lt": cmp < 0, "lte": cmp <= 0, "gt": cmp > 0, "gte": cmp >= 0}[c.Op], nil
	case "contains":
		return strings.Contains(strings.ToLower(text(v)), strings.ToLower(text(want))), nil
	case "in":
		list, ok := want.([]any)
		if !ok {
			return false, fmt.Errorf("where %s in: value must be a list", c.Field)
		}
		for _, w := range list {
			if compare(v, w) == 0 {
				return true, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("where %s: unknown op %q", c.Field, c.Op)
}

// compare orders two values: both numbers exactly, otherwise as text.
func compare(a, b any) int {
	if x, ok := num(a); ok {
		if y, ok := num(b); ok {
			return x.Cmp(y)
		}
	}
	return strings.Compare(text(a), text(b))
}

func num(v any) (*big.Rat, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return nil, false
	}
	return new(big.Rat).SetString(n.String())
}

func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// lookup follows a dot path through objects.
func lookup(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[k]; !ok {
			return nil, false
		}
	}
	return v, true
}

// within resolves rel under root, refusing anything that leaves it, by
// path or by symlink.
func within(root, rel string) (string, error) {
	if rel == "" {
		return "", errors.New("file is required")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(realRoot, p)
	}
	if !inside(realRoot, filepath.Clean(p)) {
		return "", fmt.Errorf("%s is outside the working directory", rel)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("%s: %w", rel, errors.Unwrap(err))
	}
	if !inside(realRoot, real) {
		return "", fmt.Errorf("%s is outside the working directory", rel)
	}
	return real, nil
}

func inside(root, p string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
