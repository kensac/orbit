package store

import (
	"slices"
	"testing"
)

func TestParseSearchTakesOutOperators(t *testing.T) {
	p, err := parseSearch(`Agent:dev "parse config" or loadConfig -old_name kind:report,error kind:report path:"my dir/x.go" repo:Kensac/Orbit https://x.io`)
	if err != nil {
		t.Fatal(err)
	}
	if p.agent != "dev" || !slices.Equal(p.kinds, []string{"report", "error"}) || p.path != "my dir/x.go" || p.repo != "kensac/orbit" {
		t.Fatalf("operators = %+v", p)
	}
	if got := p.text(); got != `"parse config" or loadConfig -old_name https://x.io` {
		t.Errorf("text = %q", got)
	}
	if got := p.codeText(nil); got != `"parse config" or "load Config" -"old name" "https x io"` {
		t.Errorf("code text = %q", got)
	}
	if got := p.negated().text(); got != "old_name" {
		t.Errorf("negated = %q", got)
	}
	if got := len(p.ops); got != 5 {
		t.Errorf("ops = %q", p.ops)
	}
	for _, q := range []string{"x agent:", "x agent:a agent:b", "x repo:a repo:b"} {
		if _, err := parseSearch(q); err == nil {
			t.Errorf("%q parsed", q)
		}
	}
	if p, _ := parseSearch("kind:report"); p.hasWords() {
		t.Error("operators alone have words")
	}
	if p, _ := parseSearch(`the "unclosed phrase`); p.text() != `the "unclosed phrase"` {
		t.Errorf("unclosed = %q", p.text())
	}
	if p, _ := parseSearch("the cat"); p.codeText(map[string]bool{"the": true}) != "cat" {
		t.Errorf("stop words kept: %q", p.codeText(map[string]bool{"the": true}))
	}
}
