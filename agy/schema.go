package agy

import (
	"reflect"
	"strings"

	"github.com/ironpark/gelati/internal/jsonschema"
)

// SchemaProvider lets a type supply its own JSON schema. NewTool and
// SchemaFor use it instead of reflection when the parameter type (or a
// pointer to it) implements it.
type SchemaProvider interface {
	JSONSchema() map[string]any
}

// SchemaFor returns the JSON schema of T, as NewTool derives it for tool
// parameters. It is useful for Options.ResponseSchema. The schema follows
// the Gemini SDK's derivation from Python signatures:
//
//   - A struct is an object whose properties are its exported fields, named
//     and skipped by their json tags; embedded structs and fields tagged
//     inline are flattened as encoding/json flattens them, and a field
//     tagged unknown is left out. A field is required unless it is a
//     pointer or its json tag has omitempty or omitzero.
//   - A field's `description` tag becomes its description, and an `enum`
//     tag (comma-separated values) its allowed values.
//   - Strings, booleans, integers and floats map to their JSON types;
//     slices and arrays to arrays ([]byte to a base64 string); maps with
//     string keys to objects with additionalProperties; time.Time to a
//     date-time string. Interfaces and jsontext.Value accept anything.
//   - A type implementing SchemaProvider supplies its own schema.
//
// The result is normalized with NormalizeSchema.
func SchemaFor[T any]() map[string]any {
	return schemaForType(reflect.TypeFor[T]())
}

// schemaForType is SchemaFor for a reflect.Type.
func schemaForType(t reflect.Type) map[string]any {
	return NormalizeSchema(jsonschema.For(t)).(map[string]any)
}

// schemaKeywordMap maps snake_case JSON Schema keywords (as produced by
// Python tooling) to their camelCase spelling.
var schemaKeywordMap = map[string]string{
	"any_of": "anyOf", "one_of": "oneOf", "all_of": "allOf",
	"additional_properties": "additionalProperties", "pattern_properties": "patternProperties",
	"min_items": "minItems", "max_items": "maxItems", "min_length": "minLength",
	"max_length": "maxLength", "min_properties": "minProperties", "max_properties": "maxProperties",
	"unique_items": "uniqueItems", "multiple_of": "multipleOf",
	"exclusive_minimum": "exclusiveMinimum", "exclusive_maximum": "exclusiveMaximum",
	"prefix_items": "prefixItems", "property_names": "propertyNames",
	"dependent_required": "dependentRequired", "dependent_schemas": "dependentSchemas",
	"unevaluated_properties": "unevaluatedProperties", "unevaluated_items": "unevaluatedItems",
}

var (
	uppercaseSchemaTypes  = map[string]bool{"STRING": true, "NUMBER": true, "INTEGER": true, "BOOLEAN": true, "ARRAY": true, "OBJECT": true, "NULL": true}
	subschemaDictKeywords = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true}
	literalSchemaKeywords = map[string]bool{"enum": true, "const": true, "default": true, "example": true, "examples": true, "dependentRequired": true}
)

// NormalizeSchema normalizes a JSON schema for model compatibility (upstream
// schema_utils.normalize_schema): uppercase type names ("STRING") become
// lowercase, snake_case keywords ("any_of") become camelCase ("anyOf"), and
// literal values (enum, const, default, example, examples,
// dependentRequired) are left untouched. It returns a new value; maps and
// slices are copied.
func NormalizeSchema(schema any) any {
	switch s := schema.(type) {
	case map[string]any:
		out := make(map[string]any, len(s))
		for k, v := range s {
			if m, ok := schemaKeywordMap[k]; ok {
				k = m
			}
			switch {
			case k == "type":
				switch tv := v.(type) {
				case string:
					out[k] = strings.ToLower(tv)
				case []any:
					out[k] = normalizeEach(tv)
				case []string:
					out[k] = normalizeEach(tv)
				default:
					out[k] = NormalizeSchema(v)
				}
			case subschemaDictKeywords[k]:
				if m, ok := v.(map[string]any); ok {
					sub := make(map[string]any, len(m))
					for pk, pv := range m {
						sub[pk] = NormalizeSchema(pv)
					}
					out[k] = sub
				} else {
					out[k] = NormalizeSchema(v)
				}
			case literalSchemaKeywords[k]:
				out[k] = v
			default:
				out[k] = NormalizeSchema(v)
			}
		}
		return out
	case []any:
		return normalizeEach(s)
	case []map[string]any:
		return normalizeEach(s)
	case string:
		if uppercaseSchemaTypes[s] {
			return strings.ToLower(s)
		}
		return s
	}
	return schema
}

// normalizeEach normalizes every item of s into a new []any.
func normalizeEach[T any](s []T) []any {
	out := make([]any, len(s))
	for i, item := range s {
		out[i] = NormalizeSchema(item)
	}
	return out
}
