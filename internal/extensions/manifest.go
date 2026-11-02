// Package extensions validates extension packages without executing or installing them.
package extensions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/extensionjson"
)

// Limits bound validation before an extension can be considered for use.
const (
	MaxManifestBytes = 256 << 10
	MaxPackageBytes  = 32 << 20
	MaxFiles         = 256
	MaxOperations    = 32
	MaxDepth         = 16
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Manifest is the validation-only v1 package contract. It does not grant authority.
type Manifest struct {
	Protocol   string      `json:"protocol"`
	ID         string      `json:"id"`
	Version    string      `json:"version"`
	Runtime    string      `json:"runtime"`
	Entrypoint string      `json:"entrypoint"`
	Operations []Operation `json:"operations"`
}

// Operation declares a pure operation's schemas and logical binding slots.
type Operation struct {
	Name         string         `json:"name"`
	Kind         string         `json:"kind"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema"`
	Inputs       []string       `json:"inputs,omitempty"`
	Endpoint     string         `json:"endpoint,omitempty"`
	Limits       Limits         `json:"limits"`
}

// Limits are mandatory per-operation ceilings; deployments may only lower them.
type Limits struct {
	TimeoutMS   int   `json:"timeout_ms"`
	MemoryBytes int64 `json:"memory_bytes"`
	InputBytes  int64 `json:"input_bytes"`
	OutputBytes int64 `json:"output_bytes"`
}

// Parse validates one bounded manifest. Duplicate keys are rejected at every depth.
func Parse(data []byte) (*Manifest, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("manifest must be UTF-8")
	}
	if len(data) > MaxManifestBytes {
		return nil, errors.New("manifest exceeds 256 KiB")
	}
	if err := extensionjson.Validate(data, MaxManifestBytes); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var raw map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&raw); err != nil {
		return nil, err
	}
	if err := manifestKeys(raw); err != nil {
		return nil, err
	}
	var m Manifest
	d = json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Protocol != "orbit.extension.v1" {
		return nil, errors.New("unsupported protocol")
	}
	if !namePattern.MatchString(m.ID) || !versionPattern.MatchString(m.Version) || len(m.Version) > 64 {
		return nil, errors.New("invalid id or version (expected major.minor.patch)")
	}
	if m.Runtime != "native" {
		return nil, errors.New("unsupported runtime: only native validation is available")
	}
	if !safePath(m.Entrypoint) {
		return nil, errors.New("invalid entrypoint path")
	}
	if len(m.Operations) == 0 || len(m.Operations) > MaxOperations {
		return nil, errors.New("operations must contain 1..32 entries")
	}
	seen := map[string]bool{}
	for _, o := range m.Operations {
		if !namePattern.MatchString(o.Name) || seen[o.Name] {
			return nil, fmt.Errorf("invalid or duplicate operation %q", o.Name)
		}
		seen[o.Name] = true
		if o.Kind != "source" && o.Kind != "tool" && o.Kind != "action" {
			return nil, fmt.Errorf("%s: unsupported kind", o.Name)
		}
		if strings.TrimSpace(o.Description) == "" || len(o.Description) > 1024 {
			return nil, fmt.Errorf("%s: description must contain 1..1024 bytes", o.Name)
		}
		slots := map[string]bool{}
		if len(o.Inputs) > 32 {
			return nil, fmt.Errorf("%s: too many input slots", o.Name)
		}
		for _, s := range o.Inputs {
			if !namePattern.MatchString(s) || slots[s] {
				return nil, fmt.Errorf("%s: invalid or duplicate input slot", o.Name)
			}
			slots[s] = true
		}
		if (o.Kind == "action" && !namePattern.MatchString(o.Endpoint)) || (o.Kind != "action" && o.Endpoint != "") {
			return nil, fmt.Errorf("%s: endpoint required only for actions", o.Name)
		}
		l := o.Limits
		if l.TimeoutMS < 1 || l.TimeoutMS > 30000 || l.MemoryBytes < 1 || l.MemoryBytes > 256<<20 || l.InputBytes < 1 || l.InputBytes > 16<<20 || l.OutputBytes < 1 || l.OutputBytes > 16<<20 {
			return nil, fmt.Errorf("%s: limits outside v1 ceilings", o.Name)
		}
		for _, s := range []map[string]any{o.InputSchema, o.OutputSchema} {
			if err := schema(s, 0); err != nil {
				return nil, fmt.Errorf("%s: schema: %w", o.Name, err)
			}
		}
	}
	return &m, nil
}

func safePath(s string) bool {
	return s != "" && len(s) <= 256 && !path.IsAbs(s) && s != ".." && !strings.HasPrefix(s, "../") && path.Clean(s) == s && !strings.ContainsAny(s, "\\\x00")
}

func schema(s map[string]any, depth int) error {
	if s == nil || depth > MaxDepth {
		return errors.New("missing schema or depth exceeds 16")
	}
	typ, ok := s["type"].(string)
	if !ok {
		return errors.New("type must be a single string")
	}
	allowed := map[string]bool{"type": true, "enum": true}
	switch typ {
	case "object":
		allowed["properties"] = true
		allowed["required"] = true
		allowed["additionalProperties"] = true
		if v, ok := s["additionalProperties"].(bool); !ok || v {
			return errors.New("object requires additionalProperties false")
		}
		props, ok := s["properties"].(map[string]any)
		if !ok || len(props) > 128 {
			return errors.New("object needs at most 128 properties")
		}
		for k, v := range props {
			if k == "" || len(k) > 128 {
				return errors.New("invalid property name")
			}
			child, ok := v.(map[string]any)
			if !ok {
				return errors.New("property must be a schema")
			}
			if err := schema(child, depth+1); err != nil {
				return err
			}
		}
		if req, present := s["required"]; present {
			arr, ok := req.([]any)
			if !ok {
				return errors.New("required must be array")
			}
			seen := map[string]bool{}
			for _, v := range arr {
				k, ok := v.(string)
				if !ok || seen[k] {
					return errors.New("duplicate or invalid required property")
				}
				if _, ok := props[k]; !ok {
					return errors.New("required property is not declared")
				}
				seen[k] = true
			}
		}
	case "array":
		allowed["items"] = true
		allowed["minItems"] = true
		allowed["maxItems"] = true
		if err := bounds(s, "minItems", "maxItems", true, 10000); err != nil {
			return err
		}
		child, ok := s["items"].(map[string]any)
		if !ok {
			return errors.New("array needs items schema")
		}
		if err := schema(child, depth+1); err != nil {
			return err
		}
	case "string":
		allowed["minLength"] = true
		allowed["maxLength"] = true
		if err := bounds(s, "minLength", "maxLength", true, 1<<20); err != nil {
			return err
		}
	case "integer", "number":
		allowed["minimum"] = true
		allowed["maximum"] = true
		if err := bounds(s, "minimum", "maximum", typ == "integer", 0); err != nil {
			return err
		}
	case "boolean", "null":
	default:
		return fmt.Errorf("unsupported type %q", typ)
	}
	for k := range s {
		if !allowed[k] {
			return fmt.Errorf("unsupported keyword %q", k)
		}
	}
	if raw, present := s["enum"]; present {
		a, ok := raw.([]any)
		if !ok || len(a) == 0 || len(a) > 128 {
			return errors.New("enum must contain 1..128 values")
		}
		seen := map[string]bool{}
		for _, v := range a {
			if !scalarMatches(v, typ) {
				return errors.New("enum value has wrong type or is not scalar")
			}
			if err := enumBounds(v, typ, s); err != nil {
				return err
			}
			b, _ := json.Marshal(v)
			if n, ok := v.(json.Number); ok {
				r, _ := extensionjson.Number(n)
				b = []byte(r.RatString())
			}
			if seen[string(b)] {
				return errors.New("duplicate enum value")
			}
			seen[string(b)] = true
		}
	}
	return nil
}

func scalarMatches(v any, t string) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number", "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		r, ok := extensionjson.Number(n)
		return ok && (t != "integer" || r.IsInt())
	}
	return false
}

func bounds(s map[string]any, lo, hi string, integer bool, ceiling int64) error {
	get := func(k string) (*big.Rat, error) {
		n, ok := s[k].(json.Number)
		if !ok {
			return nil, fmt.Errorf("%s must be a number", k)
		}
		r, ok := extensionjson.Number(n)
		if !ok || (integer && !r.IsInt()) {
			return nil, fmt.Errorf("invalid %s", k)
		}
		return r, nil
	}
	upper, err := get(hi)
	if err != nil {
		return err
	}
	lower := new(big.Rat)
	if _, present := s[lo]; present {
		lower, err = get(lo)
		if err != nil {
			return err
		}
	} else if ceiling == 0 {
		return fmt.Errorf("%s required", lo)
	}
	if lower.Cmp(upper) > 0 {
		return errors.New("minimum exceeds maximum")
	}
	if ceiling > 0 && (lower.Sign() < 0 || upper.Cmp(new(big.Rat).SetInt64(ceiling)) > 0) {
		return errors.New("length or item bound exceeds ceiling")
	}
	return nil
}

func enumBounds(v any, typ string, s map[string]any) error {
	switch typ {
	case "string":
		n := int64(utf8.RuneCountInString(v.(string)))
		for _, k := range []string{"minLength", "maxLength"} {
			if b, ok := s[k].(json.Number); ok {
				r, _ := extensionjson.Number(b)
				cmp := new(big.Rat).SetInt64(n).Cmp(r)
				if (k == "minLength" && cmp < 0) || (k == "maxLength" && cmp > 0) {
					return errors.New("enum string violates length bounds")
				}
			}
		}
	case "integer", "number":
		r, _ := extensionjson.Number(v.(json.Number))
		lo, _ := extensionjson.Number(s["minimum"].(json.Number))
		hi, _ := extensionjson.Number(s["maximum"].(json.Number))
		if r.Cmp(lo) < 0 || r.Cmp(hi) > 0 {
			return errors.New("enum number violates bounds")
		}
	}
	return nil
}

func manifestKeys(m map[string]any) error {
	check := func(m map[string]any, names ...string) error {
		allowed := map[string]bool{}
		for _, n := range names {
			allowed[n] = true
		}
		for k := range m {
			if !allowed[k] {
				return fmt.Errorf("unknown field %q", k)
			}
		}
		return nil
	}
	if err := check(m, "protocol", "id", "version", "runtime", "entrypoint", "operations"); err != nil {
		return err
	}
	if ops, ok := m["operations"].([]any); ok {
		for _, v := range ops {
			if o, ok := v.(map[string]any); ok {
				if err := check(o, "name", "kind", "description", "input_schema", "output_schema", "inputs", "endpoint", "limits"); err != nil {
					return err
				}
				if l, ok := o["limits"].(map[string]any); ok {
					if err := check(l, "timeout_ms", "memory_bytes", "input_bytes", "output_bytes"); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
