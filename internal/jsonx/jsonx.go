// Package jsonx holds the JSON helpers the SDK packages share: the decoding
// options for input from other programs, and, for types that carry the
// members they do not model in an Extra map, encoding a struct with extra
// members merged in and splitting those members back out of an object.
package jsonx

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
)

// Foreign holds the options for decoding JSON written by another program
// (a CLI, a server, a transcript on disk): as tolerant as encoding/json v1
// was, so a stray invalid UTF-8 sequence, a lone surrogate escape or a
// repeated member does not lose a whole message.
var Foreign = json.JoinOptions(jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))

// MarshalWithExtra encodes v, which must encode to a JSON object, and appends
// the members of extra in sorted key order, after v's own. A member is left
// out when skip reports its key; a nil skip leaves out the keys v's encoding
// already has, so v's fields win over extra.
func MarshalWithExtra(v any, extra map[string]any, skip func(key string) bool) ([]byte, error) {
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil || len(extra) == 0 {
		return b, err
	}
	if len(b) < 2 || b[0] != '{' {
		return nil, fmt.Errorf("jsonx: %T does not encode to a JSON object", v)
	}
	if skip == nil {
		var present map[string]jsontext.Value
		if err := json.Unmarshal(b, &present); err != nil {
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
		vb, err := json.Marshal(extra[k], json.Deterministic(true))
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
	if err := json.Unmarshal(data, &all, Foreign); err != nil {
		return nil, err
	}
	var extra map[string]any
	for k, raw := range all {
		if known(k) {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v, Foreign); err != nil {
			return nil, err
		}
		if extra == nil {
			extra = make(map[string]any)
		}
		extra[k] = v
	}
	return extra, nil
}
