package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Marshal encodes a wire message as protobuf JSON. HTML characters are left
// unescaped, as Python's json_format leaves them.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Unmarshal decodes protobuf JSON into the wire message v. Unknown fields are
// ignored, and members may use either the JSON name (lowerCamelCase or a
// json_name override) or the original proto field name, as protobuf JSON
// parsers accept both.
func Unmarshal(data []byte, v any) error {
	if !hasUnderscoreKey(data) || !json.Valid(data) {
		// Invalid input takes this path too, for the decoder's error.
		return json.Unmarshal(data, v)
	}
	var r keyRewriter
	r.data, r.out = data, make([]byte, 0, len(data))
	r.value(reflect.TypeOf(v))
	return json.Unmarshal(r.out, v)
}

// hasUnderscoreKey reports whether data may contain an object key with an
// underscore, which is the only case where proto field names need rewriting.
// It errs on the side of true; a false positive only costs the slow path.
func hasUnderscoreKey(data []byte) bool {
	inString, escaped, underscore := false, false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '_':
				underscore = true
			case c == '"':
				inString = false
				if underscore && nextIsColon(data[i+1:]) {
					return true
				}
			}
			continue
		}
		if c == '"' {
			inString, underscore = true, false
		}
	}
	return false
}

func nextIsColon(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return c == ':'
	}
	return false
}

// structInfos caches structInfo by struct type.
var structInfos sync.Map // reflect.Type -> *structInfo

// structInfo describes the JSON-tagged fields of a struct type.
type structInfo struct {
	fields []fieldInfo
	byName map[string]*fieldInfo // keyed by both JSON name and proto name
}

type fieldInfo struct {
	jsonName  string
	protoName string
	typ       reflect.Type
}

// structFields returns the cached structInfo of the struct type t.
func structFields(t reflect.Type) *structInfo {
	if info, ok := structInfos.Load(t); ok {
		return info.(*structInfo)
	}
	info := &structInfo{byName: make(map[string]*fieldInfo)}
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		protoName := f.Tag.Get("proto")
		if protoName == "" {
			protoName = camelToSnake(name)
		}
		info.fields = append(info.fields, fieldInfo{jsonName: name, protoName: protoName, typ: f.Type})
	}
	for i := range info.fields {
		f := &info.fields[i]
		info.byName[f.jsonName] = f
		info.byName[f.protoName] = f
	}
	actual, _ := structInfos.LoadOrStore(t, info)
	return actual.(*structInfo)
}

// keyRewriter copies valid JSON from data to out, rewriting object keys that
// use proto field names to the JSON names of the target type. Map-typed
// fields keep their keys, and members with no known type are copied as is.
// When an object holds both names of one field, the JSON name wins.
type keyRewriter struct {
	data []byte
	pos  int
	out  []byte
}

// member records where an object member starts in out, for dropping it.
type member struct {
	name    string // the key as written to out
	start   int    // offset in out, before any separating comma
	renamed bool
}

// value rewrites the value at pos as type t; nil t copies it unchanged.
func (r *keyRewriter) value(t reflect.Type) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	r.skipSpace()
	switch r.data[r.pos] {
	case '{':
		r.object(t)
	case '[':
		var elem reflect.Type
		if t != nil && t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
			elem = t.Elem()
		}
		r.out = append(r.out, '[')
		r.pos++
		for r.skipSpace(); r.data[r.pos] != ']'; r.skipSpace() {
			if r.data[r.pos] == ',' {
				r.out = append(r.out, ',')
				r.pos++
			}
			r.value(elem)
		}
		r.out = append(r.out, ']')
		r.pos++
	case '"':
		start := r.pos
		r.skipString()
		r.out = append(r.out, r.data[start:r.pos]...)
	default: // number or literal
		start := r.pos
		for r.pos < len(r.data) && !strings.ContainsRune(",]} \t\r\n", rune(r.data[r.pos])) {
			r.pos++
		}
		r.out = append(r.out, r.data[start:r.pos]...)
	}
}

func (r *keyRewriter) object(t reflect.Type) {
	var info *structInfo
	var elem reflect.Type // the member type for maps
	if t != nil {
		switch t.Kind() {
		case reflect.Struct:
			info = structFields(t)
		case reflect.Map:
			elem = t.Elem()
		}
	}
	objStart := len(r.out)
	r.out = append(r.out, '{')
	r.pos++
	var members []member
	var renamed, direct bool // whether any member is renamed, or not
	for r.skipSpace(); r.data[r.pos] != '}'; r.skipSpace() {
		if r.data[r.pos] == ',' {
			r.pos++
			r.skipSpace()
		}
		m := member{start: len(r.out)}
		if len(members) > 0 {
			r.out = append(r.out, ',')
		}
		keyStart := r.pos
		r.skipString()
		rawKey := r.data[keyStart:r.pos]
		typ := elem
		if info != nil {
			m.name, typ = keyName(rawKey), nil
			if f := info.byName[m.name]; f != nil {
				typ = f.typ
				m.renamed = m.name != f.jsonName
				m.name = f.jsonName
			}
		}
		if m.renamed {
			r.out = append(r.out, '"')
			r.out = append(r.out, m.name...)
			r.out = append(r.out, '"')
		} else {
			r.out = append(r.out, rawKey...)
		}
		r.skipSpace()
		r.pos++ // ':'
		r.out = append(r.out, ':')
		r.value(typ)
		renamed = renamed || m.renamed
		direct = direct || !m.renamed
		members = append(members, m)
	}
	r.pos++
	if renamed && direct {
		r.dropShadowed(objStart+1, members)
	}
	r.out = append(r.out, '}')
}

// dropShadowed removes from the object body that starts at out[start] the
// renamed members whose JSON name also appears as a key.
func (r *keyRewriter) dropShadowed(start int, members []member) {
	direct := make(map[string]bool)
	for _, m := range members {
		if !m.renamed {
			direct[m.name] = true
		}
	}
	if !slices.ContainsFunc(members, func(m member) bool { return m.renamed && direct[m.name] }) {
		return
	}
	body := slices.Clone(r.out[start:])
	r.out = r.out[:start]
	for i, m := range members {
		if m.renamed && direct[m.name] {
			continue
		}
		end := len(body)
		if i+1 < len(members) {
			end = members[i+1].start - start
		}
		seg := body[m.start-start : end]
		if i > 0 {
			seg = seg[1:] // the separating comma
		}
		if len(r.out) > start {
			r.out = append(r.out, ',')
		}
		r.out = append(r.out, seg...)
	}
}

// keyName returns the text of the quoted JSON string raw.
func keyName(raw []byte) string {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1 : len(raw)-1])
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func (r *keyRewriter) skipSpace() {
	for r.pos < len(r.data) {
		switch r.data[r.pos] {
		case ' ', '\t', '\r', '\n':
			r.pos++
			continue
		}
		return
	}
}

// skipString advances past the string starting at pos.
func (r *keyRewriter) skipString() {
	for r.pos++; r.data[r.pos] != '"'; r.pos++ {
		if r.data[r.pos] == '\\' {
			r.pos++
		}
	}
	r.pos++
}

func camelToSnake(s string) string {
	var b strings.Builder
	for _, r := range s {
		if 'A' <= r && r <= 'Z' {
			b.WriteByte('_')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Int64 is an int64 field. Protobuf JSON encodes it as a decimal string; a
// bare JSON number is accepted on decode.
type Int64 int64

// MarshalJSON encodes i as a quoted decimal string.
func (i Int64) MarshalJSON() ([]byte, error) {
	return strconv.AppendQuote(nil, strconv.FormatInt(int64(i), 10)), nil
}

// UnmarshalJSON accepts a quoted or bare decimal integer.
func (i *Int64) UnmarshalJSON(b []byte) error {
	return unmarshalInteger(b, i, "int64", func(s string) (Int64, error) {
		n, err := strconv.ParseInt(s, 10, 64)
		return Int64(n), err
	})
}

// Uint64 is a uint64 field. Protobuf JSON encodes it as a decimal string; a
// bare JSON number is accepted on decode.
type Uint64 uint64

// MarshalJSON encodes u as a quoted decimal string.
func (u Uint64) MarshalJSON() ([]byte, error) {
	return strconv.AppendQuote(nil, strconv.FormatUint(uint64(u), 10)), nil
}

// UnmarshalJSON accepts a quoted or bare decimal integer.
func (u *Uint64) UnmarshalJSON(b []byte) error {
	return unmarshalInteger(b, u, "uint64", func(s string) (Uint64, error) {
		n, err := strconv.ParseUint(s, 10, 64)
		return Uint64(n), err
	})
}

// unmarshalInteger decodes a quoted or bare integer into dst with parse,
// also accepting an integral float such as 1e3. Null leaves dst unchanged.
func unmarshalInteger[T ~int64 | ~uint64](b []byte, dst *T, kind string, parse func(string) (T, error)) error {
	s, err := numberText(b)
	if err != nil || s == "" {
		return err
	}
	n, err := parse(s)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		n = T(f)
		if ferr != nil || float64(n) != f || (f < 0) != (n < 0) {
			return fmt.Errorf("wire: invalid %s %s", kind, b)
		}
	}
	*dst = n
	return nil
}

// numberText returns the text of a JSON number or numeric string, or "" for
// null.
func numberText(b []byte) (string, error) {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		return "", nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	return string(b), nil
}

// marshalEnum encodes an enum value name. Values that hold a bare number
// (an open-enum value this package does not know) encode as that number.
func marshalEnum(name string) ([]byte, error) {
	if _, err := strconv.ParseInt(name, 10, 32); err == nil {
		return []byte(name), nil
	}
	return json.Marshal(name)
}

// unmarshalEnum decodes an enum given by name or number. A known number is
// mapped to its name; an unknown one is kept as its decimal text.
func unmarshalEnum(b []byte, dst *string, names map[int32]string) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, dst)
	}
	n, err := strconv.ParseInt(string(b), 10, 32)
	if err != nil {
		return fmt.Errorf("wire: invalid enum value %s", b)
	}
	if name, ok := names[int32(n)]; ok {
		*dst = name
	} else {
		*dst = strconv.FormatInt(n, 10)
	}
	return nil
}
