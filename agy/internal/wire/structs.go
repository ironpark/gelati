package wire

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"google.golang.org/protobuf/types/known/structpb"
)

// StructOf builds a genai Struct from Go values: nil, bool, numbers, string,
// []any, map[string]any, and the Struct, ListValue, Value and Content message
// types. Other values are converted through their JSON encoding. Map members
// are ordered by key.
func StructOf(m map[string]any) (*Struct, error) {
	fields := make([]*Field, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		v, err := ValueOf(m[k])
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", k, err)
		}
		fields = append(fields, Field_builder{Name: new(k), Value: v}.Build())
	}
	return Struct_builder{Fields: fields}.Build(), nil
}

// ValueOf converts a Go value to a genai Value; see StructOf for the
// supported types. Other values go through their JSON encoding, so an
// encoding/json Number becomes a number value; a number beyond float64's
// range in that encoding becomes its text. A value that cannot be encoded
// (an invalid Number, or one json.Marshal rejects) becomes its fmt
// representation, as upstream falls back to str(), so callers need not
// probe values first; the error is currently always nil.
func ValueOf(v any) (*Value, error) {
	switch v := v.(type) {
	case nil:
		return Value_builder{NullValue: new(structpb.NullValue_NULL_VALUE)}.Build(), nil
	case bool:
		return Value_builder{BoolValue: &v}.Build(), nil
	case string:
		return Value_builder{StringValue: &v}.Build(), nil
	case float64:
		return numberValue(v), nil
	case float32:
		return numberValue(float64(v)), nil
	case int:
		return numberValue(float64(v)), nil
	case int32:
		return numberValue(float64(v)), nil
	case int64:
		return numberValue(float64(v)), nil
	case uint32:
		return numberValue(float64(v)), nil
	case uint64:
		return numberValue(float64(v)), nil
	case []any:
		values := make([]*Value, 0, len(v))
		for i, e := range v {
			ev, err := ValueOf(e)
			if err != nil {
				return nil, fmt.Errorf("index %d: %w", i, err)
			}
			values = append(values, ev)
		}
		return Value_builder{ListValue: ListValue_builder{Values: values}.Build()}.Build(), nil
	case map[string]any:
		s, err := StructOf(v)
		if err != nil {
			return nil, err
		}
		return Value_builder{StructValue: s}.Build(), nil
	case *Value:
		return v, nil
	case *Struct:
		return Value_builder{StructValue: v}.Build(), nil
	case *ListValue:
		return Value_builder{ListValue: v}.Build(), nil
	case *Content:
		return Value_builder{ContentValue: v}.Build(), nil
	}
	// Anything else (structs, typed slices and maps) goes through its JSON
	// encoding, the Go analogue of upstream's model_dump/dataclass handling.
	b, err := json.Marshal(v, jsontext.AllowInvalidUTF8(true))
	if err != nil {
		return Value_builder{StringValue: new(fmt.Sprint(v))}.Build(), nil
	}
	val, err := valueOfJSON(jsontext.NewDecoder(bytes.NewReader(b)))
	if err != nil {
		return Value_builder{StringValue: new(fmt.Sprint(v))}.Build(), nil
	}
	return val, nil
}

// valueOfJSON reads the next JSON value from dec as a Value. Object members
// are ordered by name, as StructOf orders them. Each number is converted on
// its own, so one out of float64's range keeps its text rather than failing
// the whole value.
func valueOfJSON(dec *jsontext.Decoder) (*Value, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	switch tok.Kind() {
	case 'n':
		return ValueOf(nil)
	case 't', 'f':
		return ValueOf(tok.Bool())
	case '"':
		return ValueOf(tok.String())
	case '0':
		f, err := strconv.ParseFloat(tok.String(), 64)
		if err != nil {
			return Value_builder{StringValue: new(tok.String())}.Build(), nil
		}
		return numberValue(f), nil
	case '[':
		var values []*Value
		for dec.PeekKind() != ']' {
			ev, err := valueOfJSON(dec)
			if err != nil {
				return nil, err
			}
			values = append(values, ev)
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		return Value_builder{ListValue: ListValue_builder{Values: values}.Build()}.Build(), nil
	case '{':
		members := map[string]*Value{}
		for dec.PeekKind() != '}' {
			nameTok, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			name := nameTok.String()
			mv, err := valueOfJSON(dec)
			if err != nil {
				return nil, err
			}
			members[name] = mv
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		fields := make([]*Field, 0, len(members))
		for _, k := range slices.Sorted(maps.Keys(members)) {
			fields = append(fields, Field_builder{Name: new(k), Value: members[k]}.Build())
		}
		return Value_builder{StructValue: Struct_builder{Fields: fields}.Build()}.Build(), nil
	}
	return nil, errors.New("wire: unexpected JSON token")
}

func numberValue(f float64) *Value {
	return Value_builder{NumberValue: &f}.Build()
}

// AsMap returns the plain Go form of s: field names to the AsAny form of
// their values. A nil s yields nil.
func (x *Struct) AsMap() map[string]any {
	if x == nil {
		return nil
	}
	m := make(map[string]any, len(x.GetFields()))
	for _, f := range x.GetFields() {
		m[f.GetName()] = f.GetValue().AsAny()
	}
	return m
}

// AsAny returns the plain Go form of the value: nil, bool, float64, string,
// []any or map[string]any. Content and Function values are returned as their
// message pointers. An unset value yields nil.
func (x *Value) AsAny() any {
	switch x.WhichKind() {
	case Value_NumberValue_case:
		return x.GetNumberValue()
	case Value_StringValue_case:
		return x.GetStringValue()
	case Value_BoolValue_case:
		return x.GetBoolValue()
	case Value_StructValue_case:
		return x.GetStructValue().AsMap()
	case Value_ListValue_case:
		values := x.GetListValue().GetValues()
		out := make([]any, len(values))
		for i, e := range values {
			out[i] = e.AsAny()
		}
		return out
	case Value_ContentValue_case:
		return x.GetContentValue()
	case Value_FunctionValue_case:
		return x.GetFunctionValue()
	}
	return nil
}
