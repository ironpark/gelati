package tools

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"

	"github.com/ironpark/gelati/internal/jsonx"
)

// marshalOpts holds the options for encoding, chosen so values encode as they
// did with encoding/json v1: map keys sorted, a nil slice or map as null, and
// invalid UTF-8 in strings replaced.
var marshalOpts = json.JoinOptions(
	json.Deterministic(true),
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
	jsonx.Foreign,
)

// valueKind is the kind of a JSON value, used by the generated untagged-union
// types to pick an alternative.
type valueKind int

const (
	kindInvalid valueKind = iota
	kindNull
	kindString
	kindNumber
	kindBool
	kindArray
	kindObject
)

func (k valueKind) String() string {
	switch k {
	case kindNull:
		return "null"
	case kindString:
		return "string"
	case kindNumber:
		return "number"
	case kindBool:
		return "boolean"
	case kindArray:
		return "array"
	case kindObject:
		return "object"
	}
	return "invalid value"
}

// jsonValueKind classifies data by its first non-space byte.
func jsonValueKind(data []byte) valueKind {
	data = bytes.TrimLeft(data, " \t\r\n")
	if len(data) == 0 {
		return kindInvalid
	}
	switch c := data[0]; {
	case c == 'n':
		return kindNull
	case c == '"':
		return kindString
	case c == 't' || c == 'f':
		return kindBool
	case c == '[':
		return kindArray
	case c == '{':
		return kindObject
	case c == '-' || c >= '0' && c <= '9':
		return kindNumber
	}
	return kindInvalid
}

// extraFields returns the members of the JSON object data whose names are not
// in known, or nil if there are none.
func extraFields(data []byte, known []string) (map[string]any, error) {
	return jsonx.ExtraFields(data, func(k string) bool { return slices.Contains(known, k) })
}

// marshalWithExtra encodes v (a struct without custom marshaling) and appends
// the members of extra, in sorted key order, that do not collide with known.
func marshalWithExtra(v any, extra map[string]any, known []string) ([]byte, error) {
	b, err := json.Marshal(v, marshalOpts)
	if err != nil {
		return nil, err
	}
	return jsonx.MarshalWithExtra(jsontext.Value(b), extra, func(k string) bool { return slices.Contains(known, k) })
}
