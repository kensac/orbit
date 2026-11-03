// Package forms is the one definition of a typed field a person fills in: a question's form (docs/api.md, A conversation's thread) and a template's parameters (docs/api.md, Templates) share it. Fields and answers are checked here, in code, so a model can describe a form but never decide what a valid answer is.
package forms

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/jsonstrict"
)

// Field types.
const (
	String  = "string"
	Number  = "number"
	Boolean = "boolean"
	Date    = "date"
	Select  = "select"
	File    = "file"
)

// Types are the field types, in the order the docs list them.
var Types = []string{String, Number, Boolean, Date, Select, File}

// Bounds on a form. A person answers a few fields, not a survey; the option and length bounds keep one form within a notification and a phone screen.
const (
	MaxFields    = 12
	MaxOptions   = 50
	MaxOptionLen = 200
	MaxLabelLen  = 200
	// MaxStringLen bounds a string answer: a paragraph, not a document (a file field carries one).
	MaxStringLen = 4000
)

// NamePattern is a field's name: what an answer is keyed by and a template's slot names.
const NamePattern = `^[a-z][a-z0-9_]{0,62}$`

var nameRE = regexp.MustCompile(NamePattern)

// DateLayout is a date answer: a calendar day, without a zone.
const DateLayout = "2006-01-02"

// Field is one typed input.
type Field struct {
	Name string `json:"name"`
	// Label is what the person is shown; empty shows Name.
	Label string `json:"label,omitempty"`
	Type  string `json:"type"`
	// Options are a select's choices, in order.
	Options []string `json:"options,omitempty"`
	// Default is the value a form starts with, and the answer when the person leaves the field out; absent or null is none.
	Default json.RawMessage `json:"default,omitempty"`
	// Required means the answer cannot be left empty (a default counts as an answer).
	Required bool `json:"required"`
}

// Title is the label a person sees.
func (f Field) Title() string {
	if f.Label != "" {
		return f.Label
	}
	return f.Name
}

// HasDefault reports whether the field has a default value.
func (f Field) HasDefault() bool {
	d := bytes.TrimSpace(f.Default)
	return len(d) > 0 && !bytes.Equal(d, []byte("null"))
}

// Validate checks a list of fields: names unique and well formed, a known type, options only (and always) on a select, and a default that is a valid answer.
func Validate(fields []Field) error {
	if len(fields) > MaxFields {
		return fmt.Errorf("at most %d fields", MaxFields)
	}
	seen := map[string]bool{}
	for i, f := range fields {
		if err := validField(f); err != nil {
			return fmt.Errorf("field %d (%s): %w", i, f.Name, err)
		}
		if seen[f.Name] {
			return fmt.Errorf("field %d: name %q appears twice", i, f.Name)
		}
		seen[f.Name] = true
	}
	return nil
}

func validField(f Field) error {
	if !nameRE.MatchString(f.Name) {
		return fmt.Errorf("name must match %s", NamePattern)
	}
	if len(f.Label) > MaxLabelLen || !utf8.ValidString(f.Label) || strings.ContainsAny(f.Label, "\n\r") {
		return fmt.Errorf("label is one line of at most %d bytes", MaxLabelLen)
	}
	if !slices.Contains(Types, f.Type) {
		return fmt.Errorf("type must be one of %s", strings.Join(Types, ", "))
	}
	if f.Type == Select {
		if len(f.Options) == 0 || len(f.Options) > MaxOptions {
			return fmt.Errorf("a select has 1 to %d options", MaxOptions)
		}
		seen := map[string]bool{}
		for _, o := range f.Options {
			if strings.TrimSpace(o) == "" || len(o) > MaxOptionLen || !utf8.ValidString(o) || strings.ContainsAny(o, "\n\r") {
				return fmt.Errorf("an option is one non-empty line of at most %d bytes", MaxOptionLen)
			}
			if seen[o] {
				return fmt.Errorf("option %q appears twice", o)
			}
			seen[o] = true
		}
	} else if len(f.Options) > 0 {
		return errors.New("only a select has options")
	}
	if f.HasDefault() {
		if f.Type == File {
			return errors.New("a file field has no default")
		}
		if _, err := Value(f, f.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	return nil
}

// Value checks one answer against its field and returns it normalized: a string, a json.Number, a bool, a date as YYYY-MM-DD, a select's option, or a file's upload id. A number keeps its exact decimal text.
func Value(f Field, raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("not a JSON value: %w", err)
	}
	if dec.More() {
		return nil, errors.New("one JSON value, please")
	}
	switch f.Type {
	case String:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("want a string")
		}
		if len(s) > MaxStringLen || !utf8.ValidString(s) {
			return nil, fmt.Errorf("a string answer is UTF-8 of at most %d bytes", MaxStringLen)
		}
		return s, nil
	case Number:
		n, ok := v.(json.Number)
		if !ok {
			return nil, errors.New("want a number")
		}
		if _, ok := jsonstrict.ParseNumber(n); !ok {
			return nil, errors.New("the number is out of range")
		}
		return n, nil
	case Boolean:
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("want true or false")
		}
		return b, nil
	case Date:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("want a date, YYYY-MM-DD")
		}
		if _, err := time.Parse(DateLayout, s); err != nil {
			return nil, errors.New("want a date, YYYY-MM-DD")
		}
		return s, nil
	case Select:
		s, ok := v.(string)
		if !ok || !slices.Contains(f.Options, s) {
			return nil, fmt.Errorf("want one of %s", strings.Join(f.Options, ", "))
		}
		return s, nil
	case File:
		s, ok := v.(string)
		if !ok || !uuidRE.MatchString(s) {
			return nil, errors.New("want an upload id")
		}
		return s, nil
	}
	return nil, fmt.Errorf("unknown type %q", f.Type)
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// empty reports whether a normalized answer says nothing: an empty string, which a required field refuses.
func empty(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// Answers checks a set of answers against the fields: every name known, every value valid, every required field answered. A field left out takes its default; one with neither is absent from the result. The result is keyed by field name.
func Answers(fields []Field, answers map[string]json.RawMessage) (map[string]any, error) {
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for name := range answers {
		if _, ok := byName[name]; !ok {
			return nil, fmt.Errorf("answers: no field named %q", name)
		}
	}
	out := map[string]any{}
	for _, f := range fields {
		raw, given := answers[f.Name]
		if given && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			given = false
		}
		if !given && f.HasDefault() {
			raw, given = f.Default, true
		}
		if !given {
			if f.Required {
				return nil, fmt.Errorf("answers: %s is required", f.Name)
			}
			continue
		}
		v, err := Value(f, raw)
		if err != nil {
			return nil, fmt.Errorf("answers: %s: %w", f.Name, err)
		}
		if f.Required && empty(v) {
			return nil, fmt.Errorf("answers: %s is required", f.Name)
		}
		out[f.Name] = v
	}
	return out, nil
}

// Text renders answers as a numbered list, in the fields' order, as a person's reply reads to an agent: "1. Label: value". A field without an answer reads "(no answer)". files names each file field's upload by id (a name, as the agent's workdir has it); an id it lacks is shown as is.
func Text(fields []Field, answers map[string]any, files map[string]string) string {
	var b strings.Builder
	for i, f := range fields {
		v, ok := answers[f.Name]
		s := "(no answer)"
		if ok {
			s = fmt.Sprint(v)
			if f.Type == File {
				if name := files[s]; name != "" {
					s = name
				}
			}
			if f.Type == Boolean {
				s = map[bool]string{true: "yes", false: "no"}[v.(bool)]
			}
		}
		fmt.Fprintf(&b, "%d. %s: %s\n", i+1, f.Title(), s)
	}
	return b.String()
}

// Describe renders a form as a numbered list of what each field takes, for a surface that cannot show the form itself (mail, a notification): "1. Label (one of: a, b; default: a; required)".
func Describe(fields []Field) string {
	var b strings.Builder
	for i, f := range fields {
		var parts []string
		switch f.Type {
		case Select:
			parts = append(parts, "one of: "+strings.Join(f.Options, ", "))
		case Date:
			parts = append(parts, "a date, YYYY-MM-DD")
		case Boolean:
			parts = append(parts, "yes or no")
		case File:
			parts = append(parts, "attach a file")
		case Number:
			parts = append(parts, "a number")
		}
		if f.HasDefault() {
			var d any
			_ = json.Unmarshal(f.Default, &d)
			parts = append(parts, fmt.Sprintf("default: %v", d))
		}
		if f.Required {
			parts = append(parts, "required")
		}
		line := fmt.Sprintf("%d. %s", i+1, f.Title())
		if len(parts) > 0 {
			line += " (" + strings.Join(parts, "; ") + ")"
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// Schema is the JSON Schema of a field list as a model writes one, for a backend's structured output: every property required, as strict schemas need, with an empty options list and a null default meaning none.
func Schema() map[string]any {
	return map[string]any{
		"type": "array", "maxItems": MaxFields,
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":     map[string]any{"type": "string", "pattern": NamePattern},
				"label":    map[string]any{"type": "string", "maxLength": MaxLabelLen},
				"type":     map[string]any{"type": "string", "enum": Types},
				"options":  map[string]any{"type": "array", "maxItems": MaxOptions, "items": map[string]any{"type": "string", "maxLength": MaxOptionLen}},
				"default":  map[string]any{"type": []string{"string", "number", "boolean", "null"}},
				"required": map[string]any{"type": "boolean"},
			},
			"required":             []string{"name", "label", "type", "options", "default", "required"},
			"additionalProperties": false,
		},
	}
}
