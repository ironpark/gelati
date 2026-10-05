package agy

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

// SchemaProvider lets a type supply its own JSON schema. NewTool and
// SchemaFor use it instead of reflection when the parameter type (or a
// pointer to it) implements it.
type SchemaProvider interface {
	JSONSchema() map[string]any
}

// SchemaFor returns the JSON schema of T, as NewTool derives it for tool
// parameters. It is useful for Config.ResponseSchema.
func SchemaFor[T any]() map[string]any {
	return schemaForType(reflect.TypeFor[T]())
}

var (
	schemaProviderType = reflect.TypeFor[SchemaProvider]()
	timeType           = reflect.TypeFor[time.Time]()
	rawMessageType     = reflect.TypeFor[json.RawMessage]()
	textMarshalerType  = reflect.TypeFor[encoding.TextMarshaler]()
)

// schemaForType derives a JSON schema for t, the way the Gemini SDK derives
// function declarations from Python signatures:
//
//   - A struct is an object whose properties are its exported fields, named
//     and skipped by their json tags; embedded structs are flattened as
//     encoding/json flattens them. A field is required unless it is a
//     pointer or its json tag has omitempty or omitzero.
//   - A field's `description` tag becomes its description, and an `enum`
//     tag (comma-separated values) its allowed values.
//   - Strings, booleans, integers and floats map to their JSON types;
//     slices and arrays to arrays ([]byte to a base64 string); maps with
//     string keys to objects with additionalProperties; time.Time to a
//     date-time string. Interfaces and json.RawMessage accept anything.
//   - A type implementing SchemaProvider supplies its own schema.
//
// The result is normalized with NormalizeSchema.
func schemaForType(t reflect.Type) map[string]any {
	s := (&schemaBuilder{visiting: map[reflect.Type]bool{}}).build(t)
	if s == nil {
		s = map[string]any{}
	}
	if _, ok := s["type"]; !ok && t.Kind() != reflect.Interface {
		// Keep tool parameter schemas well-formed objects.
		if t.Kind() == reflect.Struct || (t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct) {
			s["type"] = "object"
		}
	}
	return NormalizeSchema(s).(map[string]any)
}

type schemaBuilder struct {
	visiting map[reflect.Type]bool
}

func (b *schemaBuilder) build(t reflect.Type) map[string]any {
	if t == nil {
		return map[string]any{}
	}
	if s, ok := providedSchema(t); ok {
		return s
	}
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == rawMessageType:
		return map[string]any{}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return b.build(t.Elem())
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 && t.Kind() == reflect.Slice {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": "array", "items": b.build(t.Elem())}
	case reflect.Map:
		if t.Key().Kind() != reflect.String && !t.Key().Implements(textMarshalerType) {
			return map[string]any{"type": "object"}
		}
		return map[string]any{"type": "object", "additionalProperties": b.build(t.Elem())}
	case reflect.Struct:
		if b.visiting[t] {
			// Recursive type: accept anything below this point.
			return map[string]any{"type": "object"}
		}
		b.visiting[t] = true
		defer delete(b.visiting, t)
		props := map[string]any{}
		var required []any
		b.addFields(t, props, &required)
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	// Interfaces accept anything; so do channels, functions and other kinds
	// encoding/json cannot encode.
	return map[string]any{}
}

func providedSchema(t reflect.Type) (map[string]any, bool) {
	if t.Kind() == reflect.Interface {
		return nil, false
	}
	var v reflect.Value
	switch {
	case t.Implements(schemaProviderType):
		if t.Kind() == reflect.Pointer {
			v = reflect.New(t.Elem())
		} else {
			v = reflect.Zero(t)
		}
	case reflect.PointerTo(t).Implements(schemaProviderType):
		v = reflect.New(t)
	default:
		return nil, false
	}
	return v.Interface().(SchemaProvider).JSONSchema(), true
}

// addFields adds the JSON-visible fields of struct type t to props.
func (b *schemaBuilder) addFields(t reflect.Type, props map[string]any, required *[]any) {
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				b.addFields(ft, props, required)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		prop := b.build(f.Type)
		if d := f.Tag.Get("description"); d != "" {
			prop["description"] = d
		}
		if e := f.Tag.Get("enum"); e != "" {
			var vals []any
			for v := range strings.SplitSeq(e, ",") {
				vals = append(vals, strings.TrimSpace(v))
			}
			prop["enum"] = vals
		}
		props[name] = prop
		optional := f.Type.Kind() == reflect.Pointer ||
			strings.Contains(","+opts+",", ",omitempty,") ||
			strings.Contains(","+opts+",", ",omitzero,")
		if !optional {
			*required = append(*required, name)
		}
	}
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
