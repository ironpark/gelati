package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"strings"

	"github.com/ironpark/gelati/internal/jsonx"
)

// Generic JSON helpers shared by the parsers and codecs. Inbound CLI data is
// read leniently, like the TypeScript SDK reads it: a member of an unexpected
// JSON type is left at its zero value, while the raw payload, kept in Data or
// Raw fields, still has everything.

// ---------------------------------------------------------------------------
// Reading decoded JSON values
// ---------------------------------------------------------------------------

// str returns v when it is a string, and "" otherwise.
func str(v any) string {
	s, _ := v.(string)
	return s
}

// firstString returns the first non-empty string among m's keys.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// toInt is toInt64 converted to int.
func toInt(v any) (int, bool) {
	n, ok := toInt64(v)
	return int(n), ok
}

// toInt64 reads a JSON number, truncating a fractional one. NaN and the
// infinities are not numbers here.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

// objectItems returns the object items of a JSON array.
func objectItems(items []any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Lenient decoding
// ---------------------------------------------------------------------------

// decodeLenient fills v from data the way the TypeScript SDK reads frames:
// without validation. A field whose JSON type does not match is left at its
// zero value; the rest is still decoded.
func decodeLenient(data []byte, v any) {
	_ = json.Unmarshal(data, v)
}

// fatalDecodeErr reports whether err is worse than a field type mismatch.
// Custom UnmarshalJSON methods of inbound types ignore mismatches, which
// encoding/json reports only after decoding the rest: returning one would
// abort the decode of any enclosing value.
func fatalDecodeErr(err error) bool {
	var te *json.UnmarshalTypeError
	return err != nil && !errors.As(err, &te)
}

// skipValue is a decode target that discards its JSON value. A wrapper struct
// shadows a field with it to leave a subtree undecoded, typically one taken
// from the already-decoded frame instead.
type skipValue struct{}

func (*skipValue) UnmarshalJSON([]byte) error { return nil }

// jsonMemberNames returns the JSON member names of the fields of struct type
// t, as encoding/json names them, so the keys a type models are read off its
// tags instead of a hand-kept list.
func jsonMemberNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				maps.Copy(names, jsonMemberNames(ft))
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		names[name] = true
	}
	return names
}

// ---------------------------------------------------------------------------
// Typed values and wire maps
// ---------------------------------------------------------------------------

// marshalWithExtra encodes v, a struct, followed by the members of extra its
// own encoding does not have, in sorted key order.
func marshalWithExtra(v any, extra map[string]any) ([]byte, error) {
	return jsonx.MarshalWithExtra(v, extra, nil)
}

// toWireMap renders v as the generic JSON object it marshals to, so typed
// values can be merged into wire maps. A value that marshals to null yields a
// nil map. what names the value in errors.
func toWireMap(v any, what string) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("claude: encoding %s: %w", what, err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("claude: encoding %s: %w", what, err)
	}
	return out, nil
}

// decodeValue fills out from the generic JSON value v, leniently as
// decodeLenient does.
func decodeValue(v any, out any) {
	if b, err := json.Marshal(v); err == nil {
		decodeLenient(b, out)
	}
}

// decodeResponse converts a control response payload into a typed value. A
// member of an unexpected JSON type is an error.
func decodeResponse(data map[string]any, out any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("claude: re-encoding control response: %w", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("claude: decoding control response: %w", err)
	}
	return nil
}

// rawHolder is implemented by response types that keep the full payload in a
// Raw field.
type rawHolder interface{ setRaw(map[string]any) }

// decodeControl decodes a control response into a new T, filling its Raw field
// when it has one. Its parameters match a control call's results, so it can
// wrap one directly: decodeControl[T](c.call(ctx, subtype, fields)). Like
// other inbound data it is read leniently: a member of an unexpected JSON type
// is left at its zero value, and Raw still has it.
func decodeControl[T any](data map[string]any, err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("claude: re-encoding control response: %w", err)
	}
	out := new(T)
	if err := json.Unmarshal(b, out); fatalDecodeErr(err) {
		return nil, fmt.Errorf("claude: decoding control response: %w", err)
	}
	if h, ok := any(out).(rawHolder); ok {
		h.setRaw(data)
	}
	return out, nil
}
