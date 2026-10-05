package jsonx

import (
	"encoding/json/jsontext"
	"reflect"
	"strings"
	"testing"
)

func TestMarshalWithExtra(t *testing.T) {
	t.Parallel()
	type v struct {
		A string `json:"a,omitempty"`
		B int    `json:"b"`
	}
	known := func(k string) bool { return k == "a" || k == "b" }
	cases := []struct {
		name  string
		v     any
		extra map[string]any
		skip  func(string) bool
		want  string
	}{
		{"noExtra", v{A: "x", B: 1}, nil, nil, `{"a":"x","b":1}`},
		{"sortedAfterFields", v{B: 1}, map[string]any{"z": 1, "c": true}, nil, `{"b":1,"c":true,"z":1}`},
		{"fieldsWin", v{B: 1}, map[string]any{"b": 2, "c": 3}, nil, `{"b":1,"c":3}`},
		{"omittedFieldTakesExtra", v{B: 1}, map[string]any{"a": "e"}, nil, `{"b":1,"a":"e"}`},
		{"knownSkipped", v{B: 1}, map[string]any{"a": "e", "c": 3}, known, `{"b":1,"c":3}`},
		{"emptyObject", struct{}{}, map[string]any{"k": "v"}, nil, `{"k":"v"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := MarshalWithExtra(tc.v, tc.extra, tc.skip)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	if _, err := MarshalWithExtra([]int{1}, map[string]any{"k": 1}, nil); err == nil {
		t.Fatal("want an error for a non-object encoding")
	}
}

func TestExtraFields(t *testing.T) {
	t.Parallel()
	known := func(k string) bool { return k == "a" }
	got, err := ExtraFields([]byte(`{"a":1,"b":{"c":[1]}}`), known)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"b": map[string]any{"c": []any{1.0}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if got, err := ExtraFields([]byte(`{"a":1}`), known); err != nil || got != nil {
		t.Fatalf("got %#v, %v; want nil", got, err)
	}
	if _, err := ExtraFields([]byte(`[1]`), known); err == nil {
		t.Fatal("want an error for a non-object")
	}
}

func TestReadValueKeepsNumbers(t *testing.T) {
	dec := jsontext.NewDecoder(strings.NewReader(`{"a":[1e400,2],"b":{"c":null,"d":"x"},"a":true}`), Foreign)
	v, err := ReadValue(dec)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": true, "b": map[string]any{"c": nil, "d": "x"}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("ReadValue = %#v", v)
	}
	b, err := Marshal([]any{Number("1e400"), Number("2")})
	if err != nil || string(b) != "[1e400,2]" {
		t.Fatalf("Number encodes as %s, %v", b, err)
	}
}

func TestMarshalSortsKeys(t *testing.T) {
	b, err := Marshal(map[string]int{"b": 1, "a": 2, "c": 3})
	if err != nil || string(b) != `{"a":2,"b":1,"c":3}` {
		t.Fatalf("Marshal = %s, %v", b, err)
	}
}
