package jsonstrict

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateJSON(t *testing.T) {
	for _, data := range []string{`null`, `{"a":{"n":1.00},"b":[true,"x"]}   `} {
		if err := ValidateJSON([]byte(data), 1024); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string]string{
		"duplicate":     `{"a":1,"a":2}`,
		"escaped":       `{"name":1,"n\u0061me":2}`,
		"nested":        `{"opaque":{"nested":{"x":1,"x":2}}}`,
		"trailing":      `{} {}`,
		"trailing byte": `{}x`,
		"truncated":     `{"a":`,
		"utf8":          string([]byte{255}),
		"depth":         strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65),
		"bytes":         strings.Repeat(" ", 1025),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateJSON([]byte(data), 1024); err == nil {
				t.Fatal("accepted invalid JSON")
			}
		})
	}
}

func TestParseNumber(t *testing.T) {
	r, ok := ParseNumber(json.Number("9007199254740993.01"))
	if !ok || r.RatString() != "900719925474099301/100" {
		t.Fatalf("rounded decimal: %v %v", r, ok)
	}
	for _, s := range []string{"1e309", "1e-309", strings.Repeat("1", 129)} {
		if _, ok := ParseNumber(json.Number(s)); ok {
			t.Fatalf("accepted %q", s)
		}
	}
	for _, s := range []string{"1e308", "1e-308", "1.0"} {
		if _, ok := ParseNumber(json.Number(s)); !ok {
			t.Fatalf("rejected %q", s)
		}
	}
}
