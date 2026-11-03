package extensions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/jsonstrict"
)

// ValidateInstance checks one JSON value against the bounded manifest schema subset.
func ValidateInstance(schema map[string]any, raw []byte) error {
	if err := jsonstrict.ValidateJSON(raw, 1<<20); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return err
	}
	return validateValue(schema, value, "$", 0)
}

func validateValue(schema map[string]any, value any, at string, depth int) error {
	if depth > MaxDepth {
		return fmt.Errorf("%s: value exceeds schema depth", at)
	}
	typ, _ := schema["type"].(string)
	if !matches(value, typ) {
		return fmt.Errorf("%s: expected %s", at, typ)
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, option := range enum {
			if equalScalar(option, value) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: value is not in enum", at)
		}
	}
	switch typ {
	case "object":
		obj := value.(map[string]any)
		props := schema["properties"].(map[string]any)
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				key := name.(string)
				if _, ok := obj[key]; !ok {
					return fmt.Errorf("%s.%s: required", at, key)
				}
			}
		}
		for key, childValue := range obj {
			child, ok := props[key].(map[string]any)
			if !ok {
				return fmt.Errorf("%s.%s: unknown property", at, key)
			}
			if err := validateValue(child, childValue, at+"."+key, depth+1); err != nil {
				return err
			}
		}
	case "array":
		items := value.([]any)
		if bound, ok := schema["minItems"].(json.Number); ok && int64(len(items)) < intBound(bound) {
			return fmt.Errorf("%s: fewer than minItems", at)
		}
		if int64(len(items)) > intBound(schema["maxItems"].(json.Number)) {
			return fmt.Errorf("%s: more than maxItems", at)
		}
		child := schema["items"].(map[string]any)
		for i, item := range items {
			if err := validateValue(child, item, fmt.Sprintf("%s[%d]", at, i), depth+1); err != nil {
				return err
			}
		}
	case "string":
		n := int64(utf8.RuneCountInString(value.(string)))
		if bound, ok := schema["minLength"].(json.Number); ok && n < intBound(bound) {
			return fmt.Errorf("%s: shorter than minLength", at)
		}
		if n > intBound(schema["maxLength"].(json.Number)) {
			return fmt.Errorf("%s: longer than maxLength", at)
		}
	case "integer", "number":
		n, _ := jsonstrict.ParseNumber(value.(json.Number))
		lo, _ := jsonstrict.ParseNumber(schema["minimum"].(json.Number))
		hi, _ := jsonstrict.ParseNumber(schema["maximum"].(json.Number))
		if n.Cmp(lo) < 0 || n.Cmp(hi) > 0 {
			return fmt.Errorf("%s: number is outside bounds", at)
		}
	}
	return nil
}

func matches(v any, typ string) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "integer", "number":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		r, valid := jsonstrict.ParseNumber(n)
		return valid && (typ != "integer" || r.IsInt())
	}
	return false
}

func equalScalar(a, b any) bool {
	if x, ok := a.(json.Number); ok {
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		rx, okx := jsonstrict.ParseNumber(x)
		ry, oky := jsonstrict.ParseNumber(y)
		return okx && oky && rx.Cmp(ry) == 0
	}
	return fmt.Sprintf("%T:%v", a, a) == fmt.Sprintf("%T:%v", b, b)
}

func intBound(n json.Number) int64 {
	r, ok := jsonstrict.ParseNumber(n)
	if !ok || !r.IsInt() {
		return 0
	}
	i := new(big.Int).Quo(r.Num(), r.Denom())
	return i.Int64()
}
