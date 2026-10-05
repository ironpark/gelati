package jsonschema

import (
	"reflect"
	"testing"

	"github.com/ironpark/gelati/internal/jsonx"
)

type custom struct{}

func (custom) JSONSchema() map[string]any { return map[string]any{"type": "string"} }

// sharedEnum has spare capacity holding a sentinel, which an append that
// does not copy it would overwrite.
var sharedEnum = []any{"x", "y", "sentinel"}[:2]

type point struct{}

func (point) JSONSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"x":    map[string]any{"type": "number"},
		"kind": map[string]any{"type": "string", "enum": sharedEnum},
	}, "required": []any{"x"}}
}

func TestStrict(t *testing.T) {
	type inner struct {
		Note string `json:"note,omitempty"`
	}
	type answer struct {
		City   string   `json:"city" description:"The city."`
		Units  string   `json:"units,omitempty" enum:"c,f"`
		Days   *int     `json:"days"`
		Inner  inner    `json:"inner"`
		Tags   []string `json:"tags,omitzero"`
		Any    any      `json:"any,omitempty"`
		Custom custom   `json:"custom,omitempty"`
	}
	got := Strict(reflect.TypeFor[answer]())
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city":  map[string]any{"type": "string", "description": "The city."},
			"units": map[string]any{"type": []any{"string", "null"}, "enum": []any{"c", "f", nil}},
			"days":  map[string]any{"type": []any{"integer", "null"}},
			"inner": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"note": map[string]any{"type": []any{"string", "null"}}},
				"required":             []any{"note"},
				"additionalProperties": false,
			},
			"tags":   map[string]any{"type": []any{"array", "null"}, "items": map[string]any{"type": "string"}},
			"any":    map[string]any{},
			"custom": map[string]any{"type": []any{"string", "null"}},
		},
		"required":             []any{"city", "inner", "any", "custom", "days", "tags", "units"},
		"additionalProperties": false,
	}
	if !reflect.DeepEqual(got, want) {
		b, _ := jsonx.Marshal(got)
		t.Fatalf("schema %s", b)
	}
}

func TestStrictProvidedSchema(t *testing.T) {
	got := Strict(reflect.TypeFor[point]())
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x":    map[string]any{"type": "number"},
			"kind": map[string]any{"type": []any{"string", "null"}, "enum": []any{"x", "y", nil}},
		},
		"required":             []any{"x", "kind"},
		"additionalProperties": false,
	}
	if !reflect.DeepEqual(got, want) {
		b, _ := jsonx.Marshal(got)
		t.Fatalf("schema %s", b)
	}
	if extra := sharedEnum[:3][2]; extra != "sentinel" {
		t.Fatalf("Strict wrote into the provider's enum: %v", extra)
	}
	if s := (point{}).JSONSchema(); s["additionalProperties"] != nil {
		t.Fatalf("provider schema changed: %v", s)
	}
}

func TestForKeepsObjects(t *testing.T) {
	if s := For(reflect.TypeFor[*struct{}]()); s["type"] != "object" {
		t.Fatalf("pointer to struct = %v", s)
	}
	if s := For(reflect.TypeFor[any]()); len(s) != 0 {
		t.Fatalf("interface = %v", s)
	}
	if s := For(reflect.TypeFor[struct{ A int }]()); s["additionalProperties"] != nil {
		t.Fatalf("For set additionalProperties: %v", s)
	}
}
