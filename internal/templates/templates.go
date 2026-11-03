// Package templates is an agent template: an agent spec with {{ name }} slots and the typed parameters that fill them (docs/api.md, Templates). Validating a template and rendering one are code here, with no store and no model, so the same template and values always give the same spec.
package templates

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/forms"
	"github.com/kensac/orbit/internal/spec"
)

// Parameter modes: who gives a value, and when.
const (
	// Required is a value the creator must give.
	Required = "required"
	// Optional takes its default, or renders empty, when none is given.
	Optional = "optional"
	// UserPrompt is asked of the person at creation: a question with a form in a conversation, the form a client shows through the API.
	UserPrompt = "user_prompt"
)

// Modes are the parameter modes, in the order the docs list them.
var Modes = []string{Required, Optional, UserPrompt}

// Bounds on a template. A spec is a few kilobytes; a file a parameter inlines (a style guide, a checklist) is a document, not a dataset.
const (
	MaxBytes       = 64 << 10
	MaxDescription = 400
	MaxFileBytes   = 64 << 10
)

// NameRE is a template's name, a spec name as agents' and skills' are.
var NameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// slotRE is a slot: {{ name }}, the name a parameter's.
var slotRE = regexp.MustCompile(`\{\{\s*([a-z][a-z0-9_]{0,62})\s*\}\}`)

// Parameter is one typed input of a template: a form field (forms.Field without required, which Mode decides) with who gives it.
type Parameter struct {
	Name    string          `json:"name"`
	Label   string          `json:"label,omitempty"`
	Type    string          `json:"type"`
	Options []string        `json:"options,omitempty"`
	Default json.RawMessage `json:"default,omitempty"`
	// Mode is required, optional or user_prompt.
	Mode string `json:"mode"`
	// Description says what the value is for, shown beside the field.
	Description string `json:"description,omitempty"`
}

// Field is the parameter as a form field: required unless optional.
func (p Parameter) Field() forms.Field {
	return forms.Field{Name: p.Name, Label: p.Label, Type: p.Type, Options: p.Options, Default: p.Default, Required: p.Mode != Optional}
}

// Template is one version's document.
type Template struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  []Parameter `json:"parameters"`
	// Spec is the agent with slots: exactly an agent file's fields as JSON (docs/agents/configuration.md), any string of which may hold {{ name }}.
	Spec json.RawMessage `json:"spec"`
}

// Decode reads a template document strictly: an unknown field is an error, as for a spec.
func Decode(b []byte) (*Template, error) {
	if len(b) > MaxBytes {
		return nil, fmt.Errorf("a template is at most %d bytes", MaxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var t Template
	if err := dec.Decode(&t); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("one JSON object, please")
	}
	if t.Parameters == nil {
		t.Parameters = []Parameter{}
	}
	return &t, nil
}

// Validate checks a template: its name and description, its parameters (forms.Validate, a known mode), every slot naming a parameter, and the spec decoding as an agent once its slots are filled with sample values. Whether the rendered agent loads (its schedule parses, its sources are allowed) depends on the values, so that is checked when it is rendered.
func (t *Template) Validate() error {
	if !NameRE.MatchString(t.Name) {
		return fmt.Errorf("name %q must match %s", t.Name, NameRE)
	}
	if d := strings.TrimSpace(t.Description); d == "" || len(d) > MaxDescription || strings.ContainsAny(d, "\n\r") || !utf8.ValidString(d) {
		return fmt.Errorf("description is required: one line of at most %d bytes", MaxDescription)
	}
	fields := make([]forms.Field, len(t.Parameters))
	for i, p := range t.Parameters {
		if !slices.Contains(Modes, p.Mode) {
			return fmt.Errorf("parameter %d (%s): mode must be one of %s", i, p.Name, strings.Join(Modes, ", "))
		}
		if len(p.Description) > MaxDescription || !utf8.ValidString(p.Description) {
			return fmt.Errorf("parameter %d (%s): description is at most %d bytes", i, p.Name, MaxDescription)
		}
		fields[i] = p.Field()
	}
	if err := forms.Validate(fields); err != nil {
		return fmt.Errorf("parameters: %w", err)
	}
	var tree any
	if err := json.Unmarshal(t.Spec, &tree); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	if _, ok := tree.(map[string]any); !ok {
		return errors.New("spec must be a JSON object: the agent")
	}
	if missing := t.unknownSlots(); len(missing) > 0 {
		return fmt.Errorf("spec: slots name no parameter: %s", strings.Join(missing, ", "))
	}
	sample := map[string]any{}
	for _, p := range t.Parameters {
		sample[p.Name] = sampleValue(p)
	}
	out, err := t.fill(sample, func(string) (string, error) { return "sample", nil })
	if err != nil {
		return err
	}
	if _, err := spec.DecodeAgent(out); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	return nil
}

// sampleValue is a value of p's type, to check that a filled spec decodes.
func sampleValue(p Parameter) any {
	switch p.Type {
	case forms.Number:
		return json.Number("1")
	case forms.Boolean:
		return true
	case forms.Date:
		return "2026-01-01"
	case forms.Select:
		return p.Options[0]
	case forms.File:
		return "00000000-0000-0000-0000-000000000000"
	}
	return "sample"
}

// Slots are the parameter names the spec's strings use, sorted.
func (t *Template) Slots() []string {
	seen := map[string]bool{}
	walkStrings(t.Spec, func(s string) {
		for _, m := range slotRE.FindAllStringSubmatch(s, -1) {
			seen[m[1]] = true
		}
	})
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (t *Template) unknownSlots() []string {
	var out []string
	for _, n := range t.Slots() {
		if !slices.ContainsFunc(t.Parameters, func(p Parameter) bool { return p.Name == n }) {
			out = append(out, n)
		}
	}
	return out
}

// walkStrings calls f with every string value in a JSON document (never a key).
func walkStrings(doc json.RawMessage, f func(string)) {
	var v any
	if json.Unmarshal(doc, &v) != nil {
		return
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			f(x)
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

// Fields are the template's parameters as a form, in order.
func (t *Template) Fields() []forms.Field {
	out := make([]forms.Field, len(t.Parameters))
	for i, p := range t.Parameters {
		out[i] = p.Field()
	}
	return out
}

// Missing are the parameters of mode want that values lacks and that have no default: what must still be asked before rendering.
func (t *Template) Missing(values map[string]json.RawMessage, want ...string) []Parameter {
	var out []Parameter
	for _, p := range t.Parameters {
		raw, given := values[p.Name]
		if given && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		if p.Mode == Optional || p.Field().HasDefault() || !slices.Contains(want, p.Mode) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// FileReader returns the text a file parameter's value (an upload id) inlines, or why it cannot. The caller decides which uploads it may read.
type FileReader func(uploadID string) (string, error)

// Render fills the spec's slots from values (checked by forms.Answers against the parameters, so a required or user_prompt parameter without a value or default is an error) and returns the agent spec as JSON. A string that is exactly one slot becomes the value itself (a number stays a number, a boolean a boolean); a slot inside a longer string becomes the value's text. A file parameter's value is an upload id, and its slot becomes the file's text (read through files, UTF-8, at most MaxFileBytes). Values are never scanned for slots, so a value holding "{{x}}" stays as written.
func (t *Template) Render(values map[string]json.RawMessage, files FileReader) (json.RawMessage, error) {
	vals, err := forms.Answers(t.Fields(), values)
	if err != nil {
		return nil, fmt.Errorf("parameters: %s", strings.TrimPrefix(err.Error(), "answers: "))
	}
	return t.fill(vals, files)
}

func (t *Template) fill(vals map[string]any, files FileReader) (json.RawMessage, error) {
	byName := map[string]Parameter{}
	for _, p := range t.Parameters {
		byName[p.Name] = p
	}
	text := map[string]string{}
	for name, v := range vals {
		p := byName[name]
		switch p.Type {
		case forms.File:
			if files == nil {
				return nil, fmt.Errorf("parameter %s: files cannot be read here", name)
			}
			s, err := files(v.(string))
			if err != nil {
				return nil, fmt.Errorf("parameter %s: %w", name, err)
			}
			if len(s) > MaxFileBytes || !utf8.ValidString(s) {
				return nil, fmt.Errorf("parameter %s: a file is UTF-8 text of at most %d bytes", name, MaxFileBytes)
			}
			text[name] = s
		default:
			text[name] = fmt.Sprint(v)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(t.Spec))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	var fill func(any) any
	fill = func(v any) any {
		switch x := v.(type) {
		case string:
			if m := slotRE.FindStringSubmatch(x); m != nil && m[0] == x {
				val, ok := vals[m[1]]
				if !ok {
					return ""
				}
				p := byName[m[1]]
				if p.Type == forms.Number || p.Type == forms.Boolean {
					return val
				}
				return text[m[1]]
			}
			return slotRE.ReplaceAllStringFunc(x, func(s string) string {
				return text[slotRE.FindStringSubmatch(s)[1]]
			})
		case []any:
			for i, e := range x {
				x[i] = fill(e)
			}
			return x
		case map[string]any:
			for k, e := range x {
				x[k] = fill(e)
			}
			return x
		}
		return v
	}
	return json.Marshal(fill(tree))
}

// Canonical is the template as stored: compact JSON with the spec's keys sorted, so the same template always has the same bytes.
func (t *Template) Canonical() (json.RawMessage, error) {
	var tree any
	dec := json.NewDecoder(bytes.NewReader(t.Spec))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	sp, err := json.Marshal(tree)
	if err != nil {
		return nil, err
	}
	c := *t
	c.Spec = sp
	return json.Marshal(c)
}
