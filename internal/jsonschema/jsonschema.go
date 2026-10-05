// Package jsonschema derives JSON schemas from Go types, for tool parameters
// and structured output.
package jsonschema

import (
	"encoding"
	"encoding/json/jsontext"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Provider lets a type supply its own schema. agy exports it as
// SchemaProvider; claude and codex accept any type with the method.
type Provider interface {
	JSONSchema() map[string]any
}

var (
	providerType      = reflect.TypeFor[Provider]()
	timeType          = reflect.TypeFor[time.Time]()
	rawMessageType    = reflect.TypeFor[jsontext.Value]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// For derives the JSON schema of t, the way the Gemini SDK derives function
// declarations from Python signatures:
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
//   - A type implementing Provider supplies its own schema.
//
// A struct, or a pointer to one, always yields an object schema.
func For(t reflect.Type) map[string]any {
	s := (&builder{visiting: map[reflect.Type]bool{}}).build(t)
	if _, ok := s["type"]; !ok && t != nil {
		// Keep tool parameter schemas well-formed objects, also when a
		// struct's own schema leaves the type out.
		if t.Kind() == reflect.Struct || (t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct) {
			s["type"] = "object"
		}
	}
	return s
}

// Strict is For in the strict form OpenAI's structured outputs require: every
// object with properties lists them all as required and sets
// additionalProperties to false, and a property For leaves optional accepts
// null instead. It applies to schemas types supply themselves too. Maps and
// interface fields have no strict form; they are left as For derives them,
// and the API rejects them.
func Strict(t reflect.Type) map[string]any {
	return strict(For(t))
}

// strict returns a strict copy of schema s; see Strict.
func strict(s map[string]any) map[string]any {
	out := maps.Clone(s)
	if items, ok := s["items"].(map[string]any); ok {
		out["items"] = strict(items)
	}
	if extra, ok := s["additionalProperties"].(map[string]any); ok {
		out["additionalProperties"] = strict(extra)
	}
	props, ok := s["properties"].(map[string]any)
	if !ok {
		return out
	}
	req, _ := s["required"].([]any)
	required := slices.Clone(req)
	newProps := make(map[string]any, len(props))
	// Optional properties join the required list in name order, after the
	// ones For already requires.
	for _, name := range slices.Sorted(maps.Keys(props)) {
		prop, _ := props[name].(map[string]any)
		prop = strict(prop)
		if !slices.Contains(req, any(name)) {
			nullable(prop)
			required = append(required, name)
		}
		newProps[name] = prop
	}
	out["properties"] = newProps
	out["required"] = required
	out["additionalProperties"] = false
	return out
}

type builder struct {
	visiting map[reflect.Type]bool
}

func (b *builder) build(t reflect.Type) map[string]any {
	if t == nil {
		return map[string]any{}
	}
	if s, ok := provided(t); ok {
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

// provided returns the schema t supplies through Provider, copied so that
// the caller may add to it.
func provided(t reflect.Type) (map[string]any, bool) {
	if t.Kind() == reflect.Interface {
		return nil, false
	}
	var v reflect.Value
	switch {
	case t.Implements(providerType):
		if t.Kind() == reflect.Pointer {
			v = reflect.New(t.Elem())
		} else {
			v = reflect.Zero(t)
		}
	case reflect.PointerTo(t).Implements(providerType):
		v = reflect.New(t)
	default:
		return nil, false
	}
	s := maps.Clone(v.Interface().(Provider).JSONSchema())
	if s == nil {
		s = map[string]any{}
	}
	return s, true
}

// addFields adds the JSON-visible fields of struct type t to props.
func (b *builder) addFields(t reflect.Type, props map[string]any, required *[]any) {
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		hasOpt := func(o string) bool { return strings.Contains(","+opts+",", ","+o+",") }
		if hasOpt("unknown") {
			// Holds the members no other field takes; not a property.
			continue
		}
		if (f.Anonymous && name == "") || hasOpt("inline") {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				b.addFields(ft, props, required)
				continue
			}
			if hasOpt("inline") {
				// An inlined map holds members of any name; not a property.
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
		optional := f.Type.Kind() == reflect.Pointer || hasOpt("omitempty") || hasOpt("omitzero")
		if !optional {
			*required = append(*required, name)
		}
	}
}

// nullable lets schema s, a copy it may change, also accept null. A schema
// without a type already accepts it.
func nullable(s map[string]any) {
	t, ok := s["type"].(string)
	if !ok {
		return
	}
	s["type"] = []any{t, "null"}
	if e, ok := s["enum"].([]any); ok {
		s["enum"] = append(slices.Clip(e), nil)
	}
}
