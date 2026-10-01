package tools

import (
	"bytes"
	"encoding/json"
	"slices"
	"sort"
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
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	var extra map[string]any
	for k, raw := range all {
		if slices.Contains(known, k) {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if extra == nil {
			extra = make(map[string]any)
		}
		extra[k] = v
	}
	return extra, nil
}

// marshalWithExtra encodes v (a struct without custom marshaling) and appends
// the members of extra, in sorted key order, that do not collide with known.
func marshalWithExtra(v any, extra map[string]any, known []string) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || len(extra) == 0 {
		return b, err
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		if !slices.Contains(known, k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return b, nil
	}
	var buf bytes.Buffer
	buf.Write(b[:len(b)-1]) // drop the closing brace
	first := len(b) == 2    // "{}"
	for _, k := range keys {
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := json.Marshal(extra[k])
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
