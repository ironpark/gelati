// Package jsonx holds the JSON (encoding/json/v2) policy and helpers the SDK
// packages share.
//
// The policy:
//
//   - Decode with Unmarshal, which applies Foreign: everything the SDKs read
//     is written by another program.
//   - Encode with Marshal, which sorts map keys, so what the SDKs send and
//     store is byte-stable. Pass LegacyEncode where the output must match
//     what encoding/json v1 (and JSON.stringify) wrote, nil slices and maps
//     as null included; the claude package does throughout.
//   - Tag optional fields omitzero when their zero value is what to leave
//     out (bools, numbers, pointers, structs, any), and omitempty when an
//     empty string, slice or map is: v2's omitempty no longer omits false
//     or 0.
//
// For types that carry the members they do not model in an Extra map,
// MarshalWithExtra and ExtraFields merge those members in and split them back
// out; ReadValue decodes a generic value keeping numbers' text.
package jsonx

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
)

// Foreign holds the options for decoding JSON written by another program
// (a CLI, a server, a transcript on disk): as tolerant as encoding/json v1
// was, so a stray invalid UTF-8 sequence, a lone surrogate escape or a
// repeated member does not lose a whole message.
var Foreign = json.JoinOptions(jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))

// LegacyEncode holds the options for encoding exactly as encoding/json v1
// did: map keys sorted, a nil slice or map as null, invalid UTF-8 in strings
// replaced, and raw values with repeated member names passed through.
var LegacyEncode = json.JoinOptions(
	json.Deterministic(true),
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
	Foreign,
)

// stable sorts map keys; see Marshal.
var stable = json.Deterministic(true)

// Unmarshal decodes data into v with Foreign and opts.
func Unmarshal(data []byte, v any, opts ...json.Options) error {
	if len(opts) == 0 {
		return json.Unmarshal(data, v, Foreign)
	}
	return json.Unmarshal(data, v, json.JoinOptions(append([]json.Options{Foreign}, opts...)...))
}

// Marshal encodes v with map keys sorted, then opts.
func Marshal(v any, opts ...json.Options) ([]byte, error) {
	if len(opts) == 0 {
		return json.Marshal(v, stable)
	}
	return json.Marshal(v, json.JoinOptions(append([]json.Options{stable}, opts...)...))
}

// MarshalWithExtra encodes v, which must encode to a JSON object, and appends
// the members of extra in sorted key order, after v's own. A member is left
// out when skip reports its key; a nil skip leaves out the keys v's encoding
// already has, so v's fields win over extra. Both are encoded as Marshal
// does, with opts.
func MarshalWithExtra(v any, extra map[string]any, skip func(key string) bool, opts ...json.Options) ([]byte, error) {
	b, err := Marshal(v, opts...)
	if err != nil || len(extra) == 0 {
		return b, err
	}
	if len(b) < 2 || b[0] != '{' {
		return nil, fmt.Errorf("jsonx: %T does not encode to a JSON object", v)
	}
	if skip == nil {
		var present map[string]jsontext.Value
		if err := Unmarshal(b, &present); err != nil {
			return nil, err
		}
		skip = func(k string) bool { _, ok := present[k]; return ok }
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		if !skip(k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return b, nil
	}
	slices.Sort(keys)
	var buf bytes.Buffer
	buf.Write(b[:len(b)-1]) // drop the closing brace
	first := len(b) == 2    // "{}"
	for _, k := range keys {
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := Marshal(extra[k], opts...)
		if err != nil {
			return nil, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// ExtraFields returns the members of the JSON object data whose keys known
// does not report, or nil if there are none.
func ExtraFields(data []byte, known func(key string) bool) (map[string]any, error) {
	var all map[string]jsontext.Value
	if err := Unmarshal(data, &all); err != nil {
		return nil, err
	}
	var extra map[string]any
	for k, raw := range all {
		if known(k) {
			continue
		}
		var v any
		if err := Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if extra == nil {
			extra = make(map[string]any)
		}
		extra[k] = v
	}
	return extra, nil
}

// Number is the literal text of a JSON number, as ReadValue keeps it. It
// encodes as that text, so a number re-serializes unchanged.
type Number string

func (n Number) String() string { return string(n) }

// Int64 parses the number as an integer.
func (n Number) Int64() (int64, error) { return strconv.ParseInt(string(n), 10, 64) }

// Float64 parses the number as a float.
func (n Number) Float64() (float64, error) { return strconv.ParseFloat(string(n), 64) }

// MarshalJSON writes the number's text.
func (n Number) MarshalJSON() ([]byte, error) { return []byte(n), nil }

// ReadValue reads the next value from dec as a generic JSON value (nil, bool,
// string, Number, []any or map[string]any). A repeated object member keeps
// its last value.
func ReadValue(dec *jsontext.Decoder) (any, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	switch tok.Kind() {
	case 'n':
		return nil, nil
	case 't', 'f':
		return tok.Bool(), nil
	case '"':
		return tok.String(), nil
	case '0':
		return Number(tok.String()), nil
	case '{':
		obj := map[string]any{}
		for dec.PeekKind() != '}' {
			name, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			// The token is only valid until the next read.
			k := name.String()
			v, err := ReadValue(dec)
			if err != nil {
				return nil, err
			}
			obj[k] = v
		}
		_, err := dec.ReadToken()
		return obj, err
	case '[':
		arr := []any{}
		for dec.PeekKind() != ']' {
			v, err := ReadValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err := dec.ReadToken()
		return arr, err
	}
	return nil, fmt.Errorf("jsonx: unexpected JSON token %v", tok)
}
