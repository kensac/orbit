package harness

import (
	"bytes"
	"encoding/json"
	"math/big"
	"regexp"
	"strings"
)

// moneyRE finds a money amount as a report writes one: a dollar sign, an
// optional minus, digits with optional thousands commas and decimals.
var moneyRE = regexp.MustCompile(`\$\s?-?\d[\d,]*(?:\.\d+)?`)

// numberRE finds a number in tool output text.
var numberRE = regexp.MustCompile(`-?\d[\d,]*(?:\.\d+)?`)

// Backed is the set of values code produced for a run: every number in
// its input files, the same divided by 100 (sources often count cents),
// and every number in a result of orbit's tools. Values are kept by
// absolute value, since "a refund of $12" reports -1200 cents.
//
// A figure also counts as backed when it is a backed value rounded (half
// away from zero) to the decimals the figure shows: "$40.21" for 40.205.
type Backed struct {
	set  map[string]bool
	vals []*big.Rat
}

// NewBacked returns an empty set.
func NewBacked() *Backed { return &Backed{set: map[string]bool{}} }

// rounds reports whether some backed value rounds to f at d decimals.
func (b *Backed) rounds(f *big.Rat, d int) bool {
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d)), nil))
	half := big.NewRat(1, 2)
	for _, v := range b.vals {
		x := new(big.Rat).Add(new(big.Rat).Mul(v, scale), half)
		n := new(big.Int).Quo(x.Num(), x.Denom()) // floor: x is not negative
		if new(big.Rat).Quo(new(big.Rat).SetInt(n), scale).Cmp(f) == 0 {
			return true
		}
	}
	return false
}

var hundred = big.NewRat(100, 1)

func (b *Backed) add(r *big.Rat, cents bool) {
	a := new(big.Rat).Abs(r)
	for _, v := range []*big.Rat{a, new(big.Rat).Quo(a, hundred)} {
		if !cents && v != a {
			continue
		}
		if k := v.RatString(); !b.set[k] {
			b.set[k] = true
			b.vals = append(b.vals, v)
		}
	}
}

// AddJSON adds every number in a JSON document, and every string that is
// a number. Content that is not JSON is read as text.
func (b *Backed) AddJSON(data []byte) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		b.AddText(string(data))
		return
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case json.Number:
			if r, ok := new(big.Rat).SetString(x.String()); ok {
				b.add(r, true)
			}
		case string:
			if r, ok := parseNumber(x); ok {
				b.add(r, true)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
}

// AddText adds every number written in s.
func (b *Backed) AddText(s string) {
	for _, m := range numberRE.FindAllString(s, -1) {
		if r, ok := parseNumber(m); ok {
			b.add(r, true)
		}
	}
}

func parseNumber(s string) (*big.Rat, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || strings.ContainsAny(s, "eE/") {
		return nil, false
	}
	return new(big.Rat).SetString(s)
}

// Unbacked lists, in order of first appearance and without repeats, the
// money amounts in text that are not in the set.
func (b *Backed) Unbacked(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range moneyRE.FindAllString(text, -1) {
		r, ok := parseNumber(strings.TrimLeft(m, "$ "))
		if !ok || seen[m] {
			continue
		}
		seen[m] = true
		abs := new(big.Rat).Abs(r)
		if !b.set[abs.RatString()] && !b.rounds(abs, decimals(strings.ReplaceAll(strings.TrimLeft(m, "$ -"), ",", ""))) {
			out = append(out, m)
		}
	}
	return out
}

// OrbitToolResults returns the text of every result of an orbit tool in a
// stream of Claude Code lines (the stream-json output, or a session's
// transcript, which share the message shape): tool_use blocks name the
// tool, tool_result blocks carry its output under the same id.
func OrbitToolResults(lines [][]byte) []string {
	names := map[string]string{}
	type result struct{ id, text string }
	var results []result
	for _, line := range lines {
		var l struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &l) != nil || (l.Type != "assistant" && l.Type != "user") {
			continue
		}
		var blocks []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			continue
		}
		for _, bl := range blocks {
			switch bl.Type {
			case "tool_use":
				names[bl.ID] = bl.Name
			case "tool_result":
				results = append(results, result{bl.ToolUseID, contentText(bl.Content)})
			}
		}
	}
	var out []string
	for _, r := range results {
		if strings.HasPrefix(names[r.id], "mcp__"+MCPServer+"__") {
			out = append(out, r.text)
		}
	}
	return out
}

// contentText is a tool_result's content: a string, or text blocks.
func contentText(c json.RawMessage) string {
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(c, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ReportText is the text of an output whose figures are checked: its
// summary, notification title and report.
func ReportText(output json.RawMessage) string {
	var o struct {
		Summary string `json:"summary"`
		Report  string `json:"report"`
		Notify  struct {
			Title string `json:"title"`
		} `json:"notify"`
	}
	_ = json.Unmarshal(output, &o)
	return strings.Join([]string{o.Summary, o.Notify.Title, o.Report}, "\n")
}
