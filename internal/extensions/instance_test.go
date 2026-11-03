package extensions

import (
	"encoding/json"
	"testing"
)

func TestValidateInstance(t *testing.T) {
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"count": map[string]any{"type": "integer", "minimum": jsonNumber("0"), "maximum": jsonNumber("10")},
			"names": map[string]any{"type": "array", "items": map[string]any{"type": "string", "maxLength": jsonNumber("8")}, "minItems": jsonNumber("1"), "maxItems": jsonNumber("3")},
		},
		"required": []any{"count"},
	}
	for _, tc := range []struct {
		name string
		json string
		ok   bool
	}{
		{name: "valid", json: `{"count":2,"names":["orbit"]}`, ok: true},
		{name: "exact decimal integer", json: `{"count":2.0}`, ok: true},
		{name: "fraction rejected", json: `{"count":2.1}`},
		{name: "out of bounds", json: `{"count":11}`},
		{name: "missing required", json: `{}`},
		{name: "unknown property", json: `{"count":1,"other":true}`},
		{name: "array lower bound", json: `{"count":1,"names":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInstance(schema, []byte(tc.json))
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateInstance() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func jsonNumber(s string) json.Number { return json.Number(s) }
