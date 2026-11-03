// Package share is a templates bundle: the JSON one orbit gives another (docs/api.md, Sharing templates), and what code does to one before it is imported. An import is text someone else wrote, so it is scanned for characters that hide text from the person reading it, and lowered so that it grants nothing: every action rule arrives as approve, never auto, and no connector arrives at all.
package share

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/templates"
)

// Format names a bundle's shape; a bundle of another format is refused rather than guessed at.
const Format = "orbit.templates/1"

// Bounds on a bundle: as many templates as one import takes, each within its own bound.
const (
	MaxTemplates = 100
	MaxBytes     = 1 << 20
)

// Bundle is what is shared: template documents, each as templates.Canonical writes it.
type Bundle struct {
	Format    string            `json:"format"`
	Templates []json.RawMessage `json:"templates"`
}

// New makes a bundle of documents.
func New(docs []*templates.Template) (*Bundle, error) {
	b := &Bundle{Format: Format}
	for _, d := range docs {
		c, err := d.Canonical()
		if err != nil {
			return nil, err
		}
		b.Templates = append(b.Templates, c)
	}
	if b.Templates == nil {
		b.Templates = []json.RawMessage{}
	}
	return b, nil
}

// Hidden reports whether r is a character that hides or reorders text a person reads: a Unicode tag character (U+E0000 to U+E007F, invisible, and readable by a model as ASCII) or a bidirectional control (U+061C, U+200E, U+200F, U+202A to U+202E, U+2066 to U+2069), which can make a spec read differently from what it does.
func Hidden(r rune) bool {
	switch {
	case r >= 0xE0000 && r <= 0xE007F:
		return true
	case r == 0x061C, r == 0x200E, r == 0x200F:
		return true
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// Scan refuses a document with a hidden character (Hidden) in any key or string, after JSON escapes are decoded (so the escape for U+202E is caught as the character it is), naming where.
func Scan(doc []byte) error {
	if !utf8.Valid(doc) {
		return errors.New("not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	if err := scanJSON(dec, ""); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func scanJSON(dec *json.Decoder, at string) error {
	check := func(s, where string) error {
		for i, r := range s {
			if Hidden(r) {
				return fmt.Errorf("%s: a hidden character U+%04X at byte %d (a Unicode tag or a bidirectional control) is refused", where, r, i)
			}
		}
		return nil
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	switch x := token.(type) {
	case string:
		return check(x, orRoot(at))
	case json.Delim:
		switch x {
		case '{':
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				k, ok := key.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if err := check(k, at+"/"+k+" (the key)"); err != nil {
					return err
				}
				if err := scanJSON(dec, at+"/"+k); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim('}') {
				if err != nil {
					return err
				}
				return errors.New("malformed JSON object")
			}
		case '[':
			for i := 0; dec.More(); i++ {
				if err := scanJSON(dec, at+"/"+strconv.Itoa(i)); err != nil {
					return err
				}
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim(']') {
				if err != nil {
					return err
				}
				return errors.New("malformed JSON array")
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
	}
	return nil
}

func orRoot(at string) string {
	if at == "" {
		return "/"
	}
	return at
}

// Lowered is one template of an import, as it will be written, with what lowering changed.
type Lowered struct {
	Template *templates.Template
	// Changes say, one line each, what was taken away: a rule that arrived auto (or as a slot) and is now approve, a connector removed.
	Changes []string
}

// Read reads a bundle for import: the scan first, over the whole bundle, then the format, then each template decoded strictly, lowered and validated. One bad template refuses the bundle, naming its index.
func Read(raw []byte) ([]Lowered, error) {
	if len(raw) > MaxBytes {
		return nil, fmt.Errorf("a bundle is at most %d bytes", MaxBytes)
	}
	if err := Scan(raw); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b Bundle
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	if b.Format != Format {
		return nil, fmt.Errorf("bundle: format %q, want %q", b.Format, Format)
	}
	if len(b.Templates) == 0 || len(b.Templates) > MaxTemplates {
		return nil, fmt.Errorf("bundle: 1 to %d templates", MaxTemplates)
	}
	out := make([]Lowered, 0, len(b.Templates))
	for i, doc := range b.Templates {
		t, err := templates.Decode(doc)
		if err != nil {
			return nil, fmt.Errorf("bundle: template %d: %w", i, err)
		}
		changes, err := Lower(t)
		if err != nil {
			return nil, fmt.Errorf("bundle: template %d (%s): %w", i, t.Name, err)
		}
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("bundle: template %d (%s): %w", i, t.Name, err)
		}
		out = append(out, Lowered{Template: t, Changes: changes})
	}
	return out, nil
}

// grantFields are spec fields an import never carries: each would give the agent a credential-holding reach the receiving orbit's person did not choose. connectors are MCP servers orbit calls with credentials it holds.
var grantFields = []string{"connectors"}

// Lower takes from a template what an import must not grant: every action rule that is not deny becomes approve (an auto, or a slot that could render auto), and every grant field is removed. It returns what it changed, sorted.
func Lower(t *templates.Template) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(t.Spec))
	dec.UseNumber()
	var sp map[string]any
	if err := dec.Decode(&sp); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	if sp == nil {
		return nil, errors.New("spec must be a JSON object: the agent")
	}
	var changes []string
	if acts, ok := sp["actions"].(map[string]any); ok {
		for typ, rule := range acts {
			if rule == "deny" || rule == "approve" {
				continue
			}
			changes = append(changes, fmt.Sprintf("actions.%s: %v arrives as approve", typ, rule))
			acts[typ] = "approve"
		}
	} else if a, has := sp["actions"]; has && a != nil {
		return nil, errors.New("spec.actions must be a map of action type to rule")
	}
	for _, f := range grantFields {
		if _, has := sp[f]; has {
			delete(sp, f)
			changes = append(changes, f+": removed; an import never grants one")
		}
	}
	slices.Sort(changes)
	b, err := json.Marshal(sp)
	if err != nil {
		return nil, err
	}
	t.Spec = b
	return changes, nil
}
