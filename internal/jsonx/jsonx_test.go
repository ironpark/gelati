package jsonx

import (
	"reflect"
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
