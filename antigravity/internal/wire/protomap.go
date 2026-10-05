package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// ProtoMap returns msg as the generic map Python's
// json_format.MessageToDict(msg, preserving_proto_field_name=True) produces:
// object keys are the original proto field names, only set fields appear,
// 64-bit integers are decimal strings, enums are value names and bytes are
// base64 text. Other numbers decode as float64, the encoding/json default.
// map-typed fields keep their keys. A nil msg yields nil.
func ProtoMap(msg any) (map[string]any, error) {
	if msg == nil {
		return nil, nil
	}
	rv := reflect.ValueOf(msg)
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil, nil
	}
	b, err := Marshal(msg)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	m, ok := tree.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("wire: %T does not encode as an object", msg)
	}
	renameToProto(m, rv.Type())
	return m, nil
}

// renameToProto rewrites, in place, the JSON member names of tree to the
// proto field names of t.
func renameToProto(tree any, t reflect.Type) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := tree.(map[string]any)
		if !ok {
			return
		}
		for _, f := range structFields(t).fields {
			v, ok := obj[f.jsonName]
			if !ok {
				continue
			}
			if f.protoName != f.jsonName {
				delete(obj, f.jsonName)
				obj[f.protoName] = v
			}
			renameToProto(v, f.typ)
		}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return
		}
		if arr, ok := tree.([]any); ok {
			for _, v := range arr {
				renameToProto(v, t.Elem())
			}
		}
	case reflect.Map:
		if obj, ok := tree.(map[string]any); ok {
			for _, v := range obj {
				renameToProto(v, t.Elem())
			}
		}
	}
}
