package claude

import (
	"encoding"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

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
	case numberText:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

// numberText is a JSON number kept as its literal text: an encoding/json
// (v1) Number a caller put in a generic value, or a jsonNumber read by
// decodeJSONObject.
type numberText interface {
	Int64() (int64, error)
	Float64() (float64, error)
	String() string
}

// jsonNumber is the literal text of a JSON number. It encodes as that text,
// so a number re-serializes unchanged.
type jsonNumber string

func (n jsonNumber) String() string { return string(n) }

func (n jsonNumber) Int64() (int64, error) { return strconv.ParseInt(string(n), 10, 64) }

func (n jsonNumber) Float64() (float64, error) { return strconv.ParseFloat(string(n), 64) }

func (n jsonNumber) MarshalJSON() ([]byte, error) { return []byte(n), nil }

// stringItems keeps the string elements of a JSON array.
func stringItems(list []any) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
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

// marshalOpts holds the options for encoding, chosen so the SDK writes what
// it wrote with encoding/json v1: map keys sorted, a nil slice or map as null,
// invalid UTF-8 in strings replaced, and raw values with repeated member
// names (passed through from the CLI) accepted.
var marshalOpts = json.JoinOptions(
	json.Deterministic(true),
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
	jsonx.Foreign,
)

// lenient holds the options for lenient reads of foreign input: as tolerant
// as jsonx.Foreign, and, like encoding/json v1, decoding the rest of a value
// after a member of the wrong JSON type, which is then reported as a
// *jsonv1.UnmarshalTypeError.
var lenient = json.JoinOptions(jsonx.Foreign, jsonv1.ReportErrorsWithLegacySemantics(true))

// decodeLenient fills v from data the way the TypeScript SDK reads frames:
// without validation. A field whose JSON type does not match is left at its
// zero value (nil for a pointer); the rest is still decoded.
func decodeLenient(data []byte, v any) {
	_ = unmarshalLenient(data, v)
}

// unmarshalLenient is decodeLenient for custom UnmarshalJSON methods: it
// returns the errors worse than a type mismatch (see fatalDecodeErr).
func unmarshalLenient(data []byte, v any) error {
	err := json.Unmarshal(data, v, lenient)
	if !isTypeMismatch(err) {
		return err
	}
	var src any
	if json.Unmarshal(data, &src, jsonx.Foreign) == nil {
		clearMismatched(reflect.ValueOf(v), src)
	}
	return nil
}

// isTypeMismatch reports whether err is a field type mismatch, the only
// decode error lenient reads tolerate.
func isTypeMismatch(err error) bool {
	var te *jsonv1.UnmarshalTypeError
	return errors.As(err, &te)
}

// fatalDecodeErr reports whether err is worse than a field type mismatch.
// Custom UnmarshalJSON methods of inbound types ignore mismatches, which
// the lenient options report only after decoding the rest: returning one would
// abort the decode of any enclosing value.
func fatalDecodeErr(err error) bool {
	return err != nil && !isTypeMismatch(err)
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
	b, err := json.Marshal(v, marshalOpts)
	if err != nil {
		return nil, err
	}
	return jsonx.MarshalWithExtra(jsontext.Value(b), extra, nil)
}

// toWireMap renders v as the generic JSON object it marshals to, so typed
// values can be merged into wire maps. A value that marshals to null yields a
// nil map. what names the value in errors.
func toWireMap(v any, what string) (map[string]any, error) {
	b, err := json.Marshal(v, marshalOpts)
	if err != nil {
		return nil, fmt.Errorf("claude: encoding %s: %w", what, err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out, jsonx.Foreign); err != nil {
		return nil, fmt.Errorf("claude: encoding %s: %w", what, err)
	}
	return out, nil
}

// decodeValue fills out from the generic JSON value v, leniently as
// decodeLenient does.
func decodeValue(v any, out any) {
	if err := remarshal(v, out); isTypeMismatch(err) {
		clearMismatched(reflect.ValueOf(out), v)
	}
}

// decodeResponse converts a control response payload into a typed value. A
// member of an unexpected JSON type is an error.
func decodeResponse(data map[string]any, out any) error {
	return remarshal(data, out)
}

// remarshal decodes the generic JSON value v into out through its encoding.
// It decodes with the lenient options, so a type mismatch leaves the rest
// decoded, and errors wrap the decoder's, so a mismatch is still recognizable.
func remarshal(v any, out any) error {
	b, err := json.Marshal(v, marshalOpts)
	if err != nil {
		return fmt.Errorf("claude: re-encoding control response: %w", err)
	}
	if err := json.Unmarshal(b, out, lenient); err != nil {
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
// is left at its zero value (nil for a pointer), and Raw still has it.
func decodeControl[T any](data map[string]any, err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	out := new(T)
	if err := remarshal(data, out); fatalDecodeErr(err) {
		return nil, err
	} else if err != nil {
		clearMismatched(reflect.ValueOf(out), data)
	}
	if h, ok := any(out).(rawHolder); ok {
		h.setRaw(data)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Repairing lenient decodes
// ---------------------------------------------------------------------------

// clearMismatched repairs a lenient decode that hit a type mismatch.
// encoding/json allocates a pointer field before it finds that the member's
// JSON type does not fit, so a wrong-typed optional member reads as a pointer
// to the zero value instead of nil. Given the decoded target v and the generic
// JSON value src it was decoded from, clearMismatched sets each pointer field
// whose member has an incompatible JSON kind back to nil, descending into
// struct-valued fields.
func clearMismatched(v reflect.Value, src any) {
	obj, ok := src.(map[string]any)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if !ok || v.Kind() != reflect.Struct {
		return
	}
	for _, f := range lenientFieldsOf(v.Type()) {
		member, present := obj[f.name]
		if !present || member == nil {
			continue
		}
		fv := v.FieldByIndex(f.index)
		if fv.Kind() == reflect.Pointer && !fv.IsNil() && !f.want.accepts(jsonKindOf(member)) {
			fv.SetZero()
			continue
		}
		if f.nested {
			clearMismatched(fv, member)
		}
	}
}

// jsonKind classifies JSON values for clearMismatched.
type jsonKind uint8

const (
	kindAny jsonKind = iota // every kind fits, or the kind is unknown
	kindString
	kindNumber
	kindBool
	kindObject
	kindArray
)

func (want jsonKind) accepts(got jsonKind) bool {
	return want == kindAny || got == kindAny || want == got
}

// jsonKindOf classifies a generic JSON value.
func jsonKindOf(v any) jsonKind {
	switch v.(type) {
	case string:
		return kindString
	case float64, numberText, int, int64:
		return kindNumber
	case bool:
		return kindBool
	case map[string]any:
		return kindObject
	case []any:
		return kindArray
	}
	return kindAny
}

var (
	jsonUnmarshalerType     = reflect.TypeFor[json.Unmarshaler]()
	jsonUnmarshalerFromType = reflect.TypeFor[json.UnmarshalerFrom]()
	textUnmarshalerType     = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// wantedKind is the JSON kind encoding/json decodes into t without a type
// mismatch.
func wantedKind(t reflect.Type) jsonKind {
	pt := reflect.PointerTo(t)
	switch {
	case t.Implements(jsonUnmarshalerType) || pt.Implements(jsonUnmarshalerType),
		t.Implements(jsonUnmarshalerFromType) || pt.Implements(jsonUnmarshalerFromType),
		t.Kind() == reflect.String && t.Implements(reflect.TypeFor[numberText]()):
		return kindAny
	case t.Implements(textUnmarshalerType) || pt.Implements(textUnmarshalerType):
		return kindString
	}
	switch t.Kind() {
	case reflect.String:
		return kindString
	case reflect.Bool:
		return kindBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return kindNumber
	case reflect.Struct, reflect.Map:
		return kindObject
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return kindString // base64
		}
		return kindArray
	case reflect.Array:
		return kindArray
	case reflect.Pointer:
		return wantedKind(t.Elem())
	}
	return kindAny
}

// lenientField is a field clearMismatched inspects.
type lenientField struct {
	name  string
	index []int
	// want is the JSON kind the member of a pointer field must have.
	want jsonKind
	// nested marks a struct or pointer-to-struct field to descend into.
	nested bool
}

// lenientFields caches lenientFieldsOf by type.
var lenientFields sync.Map // reflect.Type -> []lenientField

// lenientFieldsOf returns the pointer and struct-valued fields of struct type
// t, by JSON member name, including those promoted from embedded structs.
func lenientFieldsOf(t reflect.Type) []lenientField {
	if cached, ok := lenientFields.Load(t); ok {
		return cached.([]lenientField)
	}
	fields := appendLenientFields(nil, t, nil)
	lenientFields.Store(t, fields)
	return fields
}

func appendLenientFields(fields []lenientField, t reflect.Type, prefix []int) []lenientField {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		index := append(slices.Clone(prefix), i)
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			fields = appendLenientFields(fields, f.Type, index)
			continue
		}
		if !f.IsExported() || strings.Contains(opts, "string") {
			continue
		}
		if name == "" {
			name = f.Name
		}
		ft := f.Type
		isPtr := ft.Kind() == reflect.Pointer
		if isPtr {
			ft = ft.Elem()
		}
		// A struct with its own decoder is not descended into.
		nested := ft.Kind() == reflect.Struct && wantedKind(ft) == kindObject
		if !isPtr && !nested {
			continue
		}
		lf := lenientField{name: name, index: index, nested: nested}
		if isPtr {
			lf.want = wantedKind(ft)
		}
		fields = append(fields, lf)
	}
	return fields
}
