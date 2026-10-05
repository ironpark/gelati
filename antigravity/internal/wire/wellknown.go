package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Duration mirrors google.protobuf.Duration. Its JSON form is a decimal
// number of seconds with an "s" suffix, such as "1.5s".
type Duration struct {
	Seconds int64
	Nanos   int32
}

// NewDuration returns d as a Duration.
func NewDuration(d time.Duration) *Duration {
	return &Duration{Seconds: int64(d / time.Second), Nanos: int32(d % time.Second)}
}

// AsDuration returns x as a time.Duration, saturating on overflow. A nil x is
// zero.
func (x *Duration) AsDuration() time.Duration {
	if x == nil {
		return 0
	}
	const maxSec = math.MaxInt64 / int64(time.Second)
	switch {
	case x.Seconds > maxSec:
		return math.MaxInt64
	case x.Seconds < -maxSec:
		return math.MinInt64
	}
	return time.Duration(x.Seconds)*time.Second + time.Duration(x.Nanos)
}

// MarshalJSON encodes x as protobuf JSON, using 0, 3, 6 or 9 fractional
// digits like the reference implementations.
func (x Duration) MarshalJSON() ([]byte, error) {
	if x.Nanos <= -1e9 || x.Nanos >= 1e9 || (x.Seconds > 0 && x.Nanos < 0) || (x.Seconds < 0 && x.Nanos > 0) {
		return nil, fmt.Errorf("wire: invalid duration %d s %d ns", x.Seconds, x.Nanos)
	}
	sec, nanos := x.Seconds, x.Nanos
	var b strings.Builder
	b.WriteByte('"')
	if sec < 0 || nanos < 0 {
		b.WriteByte('-')
		sec, nanos = -sec, -nanos
	}
	b.WriteString(strconv.FormatInt(sec, 10))
	if nanos != 0 {
		frac := fmt.Sprintf("%09d", nanos)
		switch {
		case nanos%1e6 == 0:
			frac = frac[:3]
		case nanos%1e3 == 0:
			frac = frac[:6]
		}
		b.WriteByte('.')
		b.WriteString(frac)
	}
	b.WriteString(`s"`)
	return []byte(b.String()), nil
}

// UnmarshalJSON decodes the protobuf JSON form of a Duration.
func (x *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("wire: duration must be a string: %w", err)
	}
	num, ok := strings.CutSuffix(s, "s")
	if !ok || num == "" {
		return fmt.Errorf("wire: invalid duration %q", s)
	}
	neg := strings.HasPrefix(num, "-")
	num = strings.TrimPrefix(num, "-")
	whole, frac, _ := strings.Cut(num, ".")
	if whole == "" || len(frac) > 9 {
		return fmt.Errorf("wire: invalid duration %q", s)
	}
	sec, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return fmt.Errorf("wire: invalid duration %q", s)
	}
	var nanos int64
	if frac != "" {
		n, err := strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 32)
		if err != nil || n < 0 {
			return fmt.Errorf("wire: invalid duration %q", s)
		}
		nanos = n
	}
	if neg {
		sec, nanos = -sec, -nanos
	}
	*x = Duration{Seconds: sec, Nanos: int32(nanos)}
	return nil
}

// NullValue mirrors the google.protobuf.NullValue enum, whose only value
// encodes as JSON null.
type NullValue string

// NullValueNull is the single NullValue value.
const NullValueNull NullValue = "NULL_VALUE"

// MarshalJSON encodes the value as JSON null.
func (NullValue) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// UnmarshalJSON accepts null, "NULL_VALUE" or 0.
func (n *NullValue) UnmarshalJSON(b []byte) error {
	switch string(bytes.TrimSpace(b)) {
	case "null", `"NULL_VALUE"`, "0":
		*n = NullValueNull
		return nil
	}
	return fmt.Errorf("wire: invalid NullValue %s", b)
}

// UnmarshalJSON decodes a Value. It exists because a null_value member is
// written as JSON null, which the standard decoder would otherwise treat as
// an absent pointer.
func (x *Value) UnmarshalJSON(b []byte) error {
	type plain Value
	// The outer NullValue shadows the embedded one and, being a RawMessage,
	// captures a literal null instead of dropping it.
	var v struct {
		plain
		NullValue json.RawMessage `json:"nullValue"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v.NullValue != nil {
		var n NullValue
		if err := n.UnmarshalJSON(v.NullValue); err != nil {
			return err
		}
		v.plain.NullValue = &n
	}
	*x = Value(v.plain)
	return nil
}

// StructOf builds a genai Struct from Go values: nil, bool, numbers, string,
// []any, map[string]any, and the Struct, ListValue, Value and Content message
// types. Other values are converted through their JSON encoding. Map members
// are ordered by key.
func StructOf(m map[string]any) (*Struct, error) {
	s := &Struct{Fields: make([]*Field, 0, len(m))}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		v, err := ValueOf(m[k])
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", k, err)
		}
		s.Fields = append(s.Fields, &Field{Name: new(k), Value: v})
	}
	return s, nil
}

// ValueOf converts a Go value to a genai Value; see StructOf for the
// supported types. A json.Number becomes a number value. A value that cannot
// be encoded (an invalid json.Number, or one json.Marshal rejects) becomes
// its fmt representation, as upstream falls back to str(), so callers need
// not probe values first; the error is currently always nil.
func ValueOf(v any) (*Value, error) {
	switch v := v.(type) {
	case nil:
		return &Value{NullValue: new(NullValueNull)}, nil
	case bool:
		return &Value{BoolValue: &v}, nil
	case string:
		return &Value{StringValue: &v}, nil
	case float64:
		return &Value{NumberValue: &v}, nil
	case float32:
		return &Value{NumberValue: new(float64(v))}, nil
	case int:
		return &Value{NumberValue: new(float64(v))}, nil
	case int32:
		return &Value{NumberValue: new(float64(v))}, nil
	case int64:
		return &Value{NumberValue: new(float64(v))}, nil
	case uint32:
		return &Value{NumberValue: new(float64(v))}, nil
	case uint64:
		return &Value{NumberValue: new(float64(v))}, nil
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return &Value{StringValue: new(v.String())}, nil
		}
		return &Value{NumberValue: &f}, nil
	case []any:
		l := &ListValue{Values: make([]*Value, 0, len(v))}
		for i, e := range v {
			ev, err := ValueOf(e)
			if err != nil {
				return nil, fmt.Errorf("index %d: %w", i, err)
			}
			l.Values = append(l.Values, ev)
		}
		return &Value{ListValue: l}, nil
	case map[string]any:
		s, err := StructOf(v)
		if err != nil {
			return nil, err
		}
		return &Value{StructValue: s}, nil
	case *Value:
		return v, nil
	case *Struct:
		return &Value{StructValue: v}, nil
	case *ListValue:
		return &Value{ListValue: v}, nil
	case *Content:
		return &Value{ContentValue: v}, nil
	}
	// Anything else (structs, typed slices and maps) goes through its JSON
	// encoding, the Go analogue of upstream's model_dump/dataclass handling.
	b, err := json.Marshal(v)
	if err != nil {
		return &Value{StringValue: new(fmt.Sprint(v))}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return &Value{StringValue: new(fmt.Sprint(v))}, nil
	}
	return ValueOf(generic)
}

// AsMap returns the plain Go form of s: field names to the AsAny form of
// their values. A nil s yields nil.
func (x *Struct) AsMap() map[string]any {
	if x == nil {
		return nil
	}
	m := make(map[string]any, len(x.Fields))
	for _, f := range x.Fields {
		m[f.GetName()] = f.GetValue().AsAny()
	}
	return m
}

// AsAny returns the plain Go form of the value: nil, bool, float64, string,
// []any or map[string]any. Content and Function values are returned as their
// message pointers. An unset value yields nil.
func (x *Value) AsAny() any {
	switch {
	case x == nil:
		return nil
	case x.NumberValue != nil:
		return *x.NumberValue
	case x.StringValue != nil:
		return *x.StringValue
	case x.BoolValue != nil:
		return *x.BoolValue
	case x.StructValue != nil:
		return x.StructValue.AsMap()
	case x.ListValue != nil:
		out := make([]any, len(x.ListValue.Values))
		for i, e := range x.ListValue.Values {
			out[i] = e.AsAny()
		}
		return out
	case x.ContentValue != nil:
		return x.ContentValue
	case x.FunctionValue != nil:
		return x.FunctionValue
	}
	return nil
}
