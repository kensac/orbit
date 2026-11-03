package forms

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateRefusesWhatAFormCannotBe(t *testing.T) {
	ok := []Field{
		{Name: "city", Label: "City", Type: String, Required: true},
		{Name: "budget", Type: Number, Default: json.RawMessage(`1500.50`)},
		{Name: "pets", Type: Boolean, Default: json.RawMessage(`false`)},
		{Name: "move_in", Type: Date, Default: json.RawMessage(`"2026-12-01"`)},
		{Name: "size", Type: Select, Options: []string{"studio", "1br"}, Default: json.RawMessage(`"1br"`)},
		{Name: "letter", Type: File, Default: json.RawMessage(`null`)},
	}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]Field{
		"name":              {Name: "City", Type: String},
		"type":              {Name: "x", Type: "color"},
		"select no options": {Name: "x", Type: Select},
		"options on string": {Name: "x", Type: String, Options: []string{"a"}},
		"duplicate option":  {Name: "x", Type: Select, Options: []string{"a", "a"}},
		"bad default":       {Name: "x", Type: Number, Default: json.RawMessage(`"ten"`)},
		"default not option": {Name: "x", Type: Select, Options: []string{"a"},
			Default: json.RawMessage(`"b"`)},
		"file default": {Name: "x", Type: File, Default: json.RawMessage(`"00000000-0000-0000-0000-000000000000"`)},
		"bad date":     {Name: "x", Type: Date, Default: json.RawMessage(`"2026-13-01"`)},
		"label lines":  {Name: "x", Type: String, Label: "a\nb"},
	} {
		if err := Validate([]Field{f}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := Validate([]Field{{Name: "a", Type: String}, {Name: "a", Type: Number}}); err == nil {
		t.Error("a repeated name was accepted")
	}
	many := make([]Field, MaxFields+1)
	for i := range many {
		many[i] = Field{Name: "f" + strings.Repeat("x", i), Type: String}
	}
	if err := Validate(many); err == nil {
		t.Error("too many fields were accepted")
	}
}

func TestAnswersAreCheckedAndNormalized(t *testing.T) {
	fields := []Field{
		{Name: "city", Label: "City", Type: String, Required: true},
		{Name: "budget", Label: "Budget", Type: Number, Default: json.RawMessage(`1500`)},
		{Name: "pets", Label: "Pets", Type: Boolean},
		{Name: "size", Label: "Size", Type: Select, Options: []string{"studio", "1br"}, Required: true},
	}
	got, err := Answers(fields, map[string]json.RawMessage{
		"city": json.RawMessage(`"Queens"`), "pets": json.RawMessage(`true`), "size": json.RawMessage(`"1br"`)})
	if err != nil {
		t.Fatal(err)
	}
	if got["city"] != "Queens" || got["budget"] != json.Number("1500") || got["pets"] != true || got["size"] != "1br" {
		t.Fatalf("answers: %#v", got)
	}
	want := "1. City: Queens\n2. Budget: 1500\n3. Pets: yes\n4. Size: 1br\n"
	if s := Text(fields, got, nil); s != want {
		t.Fatalf("text:\n%s\nwant:\n%s", s, want)
	}
	for name, a := range map[string]map[string]json.RawMessage{
		"unknown":     {"city": json.RawMessage(`"x"`), "size": json.RawMessage(`"1br"`), "zip": json.RawMessage(`"1"`)},
		"missing":     {"size": json.RawMessage(`"1br"`)},
		"blank":       {"city": json.RawMessage(`"  "`), "size": json.RawMessage(`"1br"`)},
		"not option":  {"city": json.RawMessage(`"x"`), "size": json.RawMessage(`"2br"`)},
		"wrong type":  {"city": json.RawMessage(`"x"`), "size": json.RawMessage(`"1br"`), "pets": json.RawMessage(`"yes"`)},
		"huge number": {"city": json.RawMessage(`"x"`), "size": json.RawMessage(`"1br"`), "budget": json.RawMessage(`1e400`)},
	} {
		if _, err := Answers(fields, a); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDescribeIsANumberedList(t *testing.T) {
	got := Describe([]Field{
		{Name: "size", Label: "Size", Type: Select, Options: []string{"studio", "1br"}, Default: json.RawMessage(`"1br"`), Required: true},
		{Name: "move_in", Type: Date},
	})
	want := "1. Size (one of: studio, 1br; default: 1br; required)\n2. move_in (a date, YYYY-MM-DD)\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
