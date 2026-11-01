package harness

import (
	"fmt"
	"math/big"
	"strings"
)

// calcScale is the decimal places a non-terminating quotient is rounded
// to: past any currency's minor unit and any rate a report quotes.
const calcScale = 10

// maxCalcExpr bounds an expression's length: arithmetic over a few hundred
// figures, not a program.
const maxCalcExpr = 8 << 10

// CalcSchema is the calc tool's input schema.
var CalcSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"expression": map[string]any{"type": "string", "maxLength": maxCalcExpr,
			"description": "Decimal numbers with + - * / and parentheses, e.g. \"(4512 + 6021) / 100\"."},
	},
	"required": []string{"expression"},
}

// Calc evaluates an arithmetic expression exactly, over rationals. The
// result is a decimal string; a quotient that does not terminate is rounded
// to calcScale places and says so.
func Calc(expr string) (map[string]any, error) {
	if len(expr) > maxCalcExpr {
		return nil, fmt.Errorf("expression is over %d bytes", maxCalcExpr)
	}
	p := &calcParser{s: expr}
	v, err := p.expr()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i != len(p.s) {
		return nil, fmt.Errorf("unexpected %q at %d", p.s[p.i:], p.i)
	}
	out := map[string]any{"expression": expr}
	if s, ok := terminating(v); ok {
		out["result"] = s
	} else {
		out["result"], out["rounded"], out["fraction"] = v.FloatString(calcScale), true, v.RatString()
	}
	return out, nil
}

// terminating writes v as an exact decimal when its denominator has no
// prime factor but 2 and 5.
func terminating(v *big.Rat) (string, bool) {
	d := new(big.Int).Set(v.Denom())
	places := 0
	for _, f := range []int64{2, 5} {
		bf := big.NewInt(f)
		n := 0
		for new(big.Int).Mod(d, bf).Sign() == 0 {
			d.Div(d, bf)
			n++
		}
		places = max(places, n)
	}
	if d.Cmp(big.NewInt(1)) != 0 {
		return "", false
	}
	return v.FloatString(places), true
}

type calcParser struct {
	s     string
	i     int
	depth int
}

// maxCalcDepth bounds nested parentheses, so a hostile expression cannot
// exhaust the stack.
const maxCalcDepth = 64

func (p *calcParser) space() {
	for p.i < len(p.s) && strings.ContainsRune(" \t\n", rune(p.s[p.i])) {
		p.i++
	}
}

func (p *calcParser) expr() (*big.Rat, error) {
	v, err := p.term()
	if err != nil {
		return nil, err
	}
	for {
		p.space()
		if p.i >= len(p.s) || (p.s[p.i] != '+' && p.s[p.i] != '-') {
			return v, nil
		}
		op := p.s[p.i]
		p.i++
		w, err := p.term()
		if err != nil {
			return nil, err
		}
		if op == '+' {
			v = new(big.Rat).Add(v, w)
		} else {
			v = new(big.Rat).Sub(v, w)
		}
	}
}

func (p *calcParser) term() (*big.Rat, error) {
	v, err := p.factor()
	if err != nil {
		return nil, err
	}
	for {
		p.space()
		if p.i >= len(p.s) || (p.s[p.i] != '*' && p.s[p.i] != '/') {
			return v, nil
		}
		op := p.s[p.i]
		p.i++
		w, err := p.factor()
		if err != nil {
			return nil, err
		}
		if op == '*' {
			v = new(big.Rat).Mul(v, w)
		} else {
			if w.Sign() == 0 {
				return nil, fmt.Errorf("division by zero at %d", p.i)
			}
			v = new(big.Rat).Quo(v, w)
		}
	}
}

func (p *calcParser) factor() (*big.Rat, error) {
	p.space()
	if p.i >= len(p.s) {
		return nil, fmt.Errorf("expression ends early")
	}
	switch p.s[p.i] {
	case '-':
		p.i++
		v, err := p.factor()
		if err != nil {
			return nil, err
		}
		return new(big.Rat).Neg(v), nil
	case '(':
		p.depth++
		if p.depth > maxCalcDepth {
			return nil, fmt.Errorf("parentheses nest deeper than %d", maxCalcDepth)
		}
		p.i++
		v, err := p.expr()
		if err != nil {
			return nil, err
		}
		p.space()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return nil, fmt.Errorf("missing ) at %d", p.i)
		}
		p.i++
		p.depth--
		return v, nil
	}
	start := p.i
	for p.i < len(p.s) && (p.s[p.i] >= '0' && p.s[p.i] <= '9' || p.s[p.i] == '.' || p.s[p.i] == ',') {
		p.i++
	}
	lit := strings.ReplaceAll(p.s[start:p.i], ",", "")
	if lit == "" {
		return nil, fmt.Errorf("expected a number at %d", start)
	}
	v, ok := new(big.Rat).SetString(lit)
	if !ok || strings.Count(lit, ".") > 1 {
		return nil, fmt.Errorf("%q is not a number", lit)
	}
	return v, nil
}
