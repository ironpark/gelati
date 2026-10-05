package agy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type weatherArgs struct {
	Location string   `json:"location" description:"City name."`
	Units    string   `json:"units,omitempty" enum:"celsius,fahrenheit"`
	Days     *int     `json:"days"`
	Tags     []string `json:"tags,omitempty"`
	internal string
	Skip     string `json:"-"`
}

func TestNewToolSchema(t *testing.T) {
	tool := NewTool("get_weather", "Gets the weather.", func(_ context.Context, _ *ToolContext, in weatherArgs) (string, error) {
		return in.Location, nil
	})
	got := tool.Schema()
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"location": map[string]any{"type": "string", "description": "City name."},
			"units":    map[string]any{"type": "string", "enum": []any{"celsius", "fahrenheit"}},
			"days":     map[string]any{"type": "integer"},
			"tags":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []any{"location"},
	}
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		t.Fatalf("schema %s", gb)
	}
	if tool.Name() != "get_weather" || tool.Description() != "Gets the weather." {
		t.Fatal("name/description")
	}
}

func TestSchemaForTypes(t *testing.T) {
	type Item struct {
		ItemName string `json:"item_name"`
		Quantity int    `json:"quantity,omitzero"`
	}
	type Base struct {
		ID string `json:"id"`
	}
	type Node struct {
		Next *Node `json:"next"`
	}
	type order struct {
		Base
		Count     int               `json:"count"`
		Price     float64           `json:"price"`
		IsExpress bool              `json:"is_express"`
		Items     []Item            `json:"items"`
		Meta      map[string]int    `json:"meta"`
		When      time.Time         `json:"when"`
		Raw       json.RawMessage   `json:"raw"`
		Anything  any               `json:"anything"`
		Blob      []byte            `json:"blob"`
		Tree      Node              `json:"tree"`
		Nested    map[string][]Item `json:"nested"`
	}
	s := SchemaFor[order]()
	props := s["properties"].(map[string]any)
	check := func(name, typ string) {
		t.Helper()
		p, ok := props[name].(map[string]any)
		if !ok {
			t.Fatalf("no property %s in %v", name, props)
		}
		if p["type"] != typ && !(typ == "" && p["type"] == nil) {
			t.Errorf("%s type = %v, want %q", name, p["type"], typ)
		}
	}
	check("id", "string")
	check("count", "integer")
	check("price", "number")
	check("is_express", "boolean")
	check("items", "array")
	check("meta", "object")
	check("when", "string")
	check("raw", "")
	check("anything", "")
	check("blob", "string")
	check("tree", "object")
	items := props["items"].(map[string]any)["items"].(map[string]any)
	if items["required"].([]any)[0] != "item_name" || len(items["required"].([]any)) != 1 {
		t.Errorf("item required %v", items["required"])
	}
	// The recursive field stops instead of looping.
	next := props["tree"].(map[string]any)["properties"].(map[string]any)["next"].(map[string]any)
	if next["type"] != "object" {
		t.Errorf("recursive field %v", next)
	}

	if s := SchemaFor[struct{}](); s["type"] != "object" || len(s["properties"].(map[string]any)) != 0 {
		t.Errorf("empty struct schema %v", s)
	}
	if s := SchemaFor[map[string]any](); s["type"] != "object" {
		t.Errorf("map schema %v", s)
	}
}

type customSchema struct{ X int }

func (customSchema) JSONSchema() map[string]any {
	return map[string]any{"type": "OBJECT", "properties": map[string]any{"x": map[string]any{"type": "INTEGER"}}}
}

func TestSchemaProvider(t *testing.T) {
	s := SchemaFor[customSchema]()
	if s["type"] != "object" || s["properties"].(map[string]any)["x"].(map[string]any)["type"] != "integer" {
		t.Fatalf("provided schema %v", s)
	}
}

func TestNormalizeSchema(t *testing.T) {
	in := map[string]any{
		"type": "OBJECT",
		"properties": map[string]any{
			"type":   map[string]any{"type": "STRING"}, // a property named "type"
			"choice": map[string]any{"any_of": []any{map[string]any{"type": "INTEGER"}, map[string]any{"type": "STRING"}}},
			"list":   map[string]any{"type": "ARRAY", "min_items": 1, "items": map[string]any{"type": "NUMBER"}},
			"level":  map[string]any{"type": "STRING", "enum": []any{"DEBUG", "INFO"}},
		},
		"const":                 "UPPERCASE_CONST",
		"default":               "DEFAULT_VAL",
		"dependent_required":    map[string]any{"a": []any{"B"}},
		"$defs":                 map[string]any{"Thing": map[string]any{"type": "BOOLEAN"}},
		"additional_properties": false,
	}
	got := NormalizeSchema(in).(map[string]any)
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"type":   map[string]any{"type": "string"},
			"choice": map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "string"}}},
			"list":   map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "number"}},
			"level":  map[string]any{"type": "string", "enum": []any{"DEBUG", "INFO"}},
		},
		"const":                "UPPERCASE_CONST",
		"default":              "DEFAULT_VAL",
		"dependentRequired":    map[string]any{"a": []any{"B"}},
		"$defs":                map[string]any{"Thing": map[string]any{"type": "boolean"}},
		"additionalProperties": false,
	}
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		t.Fatalf("normalized %s", gb)
	}
	if NormalizeSchema("STRING") != "string" || NormalizeSchema("Hello") != "Hello" || NormalizeSchema(5) != 5 {
		t.Fatal("scalars")
	}
	if got := NormalizeSchema(map[string]any{"type": []any{"STRING", "NULL"}}); !reflect.DeepEqual(got, map[string]any{"type": []any{"string", "null"}}) {
		t.Fatalf("type list %v", got)
	}
	// The input is not modified.
	if in["type"] != "OBJECT" {
		t.Fatal("input modified")
	}
}

func TestNewToolCallDecodesArguments(t *testing.T) {
	type args struct {
		Count int     `json:"count"`
		Ratio float64 `json:"ratio"`
	}
	tool := NewTool("calc", "", func(_ context.Context, tc *ToolContext, in args) (map[string]any, error) {
		return map[string]any{"product": float64(in.Count) * in.Ratio, "conv": tc.ConversationID()}, nil
	})
	out, err := tool.Call(t.Context(), nil, map[string]any{"count": 3.0, "ratio": 1.5})
	if err != nil || out.(map[string]any)["product"] != 4.5 || out.(map[string]any)["conv"] != "" {
		t.Fatalf("Call = %v, %v", out, err)
	}
	if _, err := tool.Call(t.Context(), nil, map[string]any{"count": "three"}); err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("bad args error %v", err)
	}
	if out, err := tool.Call(t.Context(), nil, nil); err != nil || out.(map[string]any)["product"] != 0.0 {
		t.Fatalf("nil args %v %v", out, err)
	}
}

func TestToolRunnerRegistry(t *testing.T) {
	noop := func(name string) *Tool {
		return NewToolWithSchema(name, "", nil, func(context.Context, *ToolContext, map[string]any) (any, error) { return name, nil })
	}
	r := mustTools(t, noop("a"), noop("b"))
	if names := toolNames(r); !reflect.DeepEqual(names, []string{"a", "b"}) {
		t.Fatalf("names %v", names)
	}
	if err := r.register(noop("a")); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate register %v", err)
	}
	if err := r.unregister("a"); err != nil || r.has("a") {
		t.Fatalf("unregister %v", err)
	}
	if err := r.unregister("missing"); err == nil {
		t.Fatal("unregister of missing tool succeeded")
	}
	if _, err := r.execute(t.Context(), "missing", nil); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("execute missing %v", err)
	}
	if _, err := newToolRunner([]*Tool{nil}); err == nil {
		t.Fatal("nil tool accepted")
	}
}

func toolNames(r *toolRunner) []string {
	var out []string
	for _, t := range r.list() {
		out = append(out, t.Name())
	}
	return out
}

func TestProcessToolCalls(t *testing.T) {
	var running, maxRunning atomic.Int32
	slow := NewToolWithSchema("slow", "", nil, func(context.Context, *ToolContext, map[string]any) (any, error) {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		running.Add(-1)
		return "ok", nil
	})
	sentinel := errors.New("bad input")
	failing := NewToolWithSchema("failing", "", nil, func(context.Context, *ToolContext, map[string]any) (any, error) { return nil, sentinel })
	echo := NewToolWithSchema("echo", "", nil, func(_ context.Context, _ *ToolContext, args map[string]any) (any, error) { return args, nil })
	r := mustTools(t, slow, failing, echo)

	if got := r.processToolCalls(t.Context(), nil); len(got) != 0 {
		t.Fatalf("empty batch %v", got)
	}
	calls := []*ToolCall{
		{Name: "slow", ID: "1", StepID: "s1", ServerName: "srv"},
		{Name: "slow", ID: "2"},
		{Name: "failing", ID: "3", StepID: "s3"},
		{Name: "unknown", ID: "4", StepID: "s4", ServerName: "srv4"},
		{Name: "echo", ID: "5", Args: map[string]any{"k": "v"}},
	}
	res := r.processToolCalls(t.Context(), calls)
	if len(res) != 5 {
		t.Fatalf("results %v", res)
	}
	if maxRunning.Load() < 2 {
		t.Error("tool calls did not run concurrently")
	}
	if res[0].Result != "ok" || res[0].ID != "1" || res[0].StepID != "s1" || res[0].ServerName != "srv" || res[0].Failed() {
		t.Errorf("result 0 %+v", res[0])
	}
	if !res[2].Failed() || res[2].Error != "bad input" || !errors.Is(res[2].Err, sentinel) || res[2].StepID != "s3" {
		t.Errorf("failing result %+v", res[2])
	}
	if res[3].Error != "Unknown tool: 'unknown'" || res[3].StepID != "s4" || res[3].ServerName != "srv4" {
		t.Errorf("unknown result %+v", res[3])
	}
	if !reflect.DeepEqual(res[4].Result, map[string]any{"k": "v"}) {
		t.Errorf("echo result %+v", res[4])
	}
}

func TestToolContextInjection(t *testing.T) {
	var seen *ToolContext
	tool := NewToolWithSchema("ctx_tool", "", nil, func(_ context.Context, tc *ToolContext, _ map[string]any) (any, error) {
		seen = tc
		tc.UpdateState("calls", func(v any) any { return v.(int) + 1 }, 0)
		return nil, nil
	})
	r := mustTools(t, tool)
	tc := newToolContext(nil)
	r.setContext(tc)
	for range 3 {
		r.processToolCalls(t.Context(), []*ToolCall{{Name: "ctx_tool", ID: "x"}})
	}
	if seen != tc {
		t.Fatal("tool did not receive the runner's context")
	}
	if v, _ := StateAs[int](&tc.StateStore, "calls"); v != 3 {
		t.Fatalf("calls state %v", v)
	}
	if tc.ConversationID() != "" || (*ToolContext)(nil).ConversationID() != "" {
		t.Fatal("conversation id outside a session")
	}
	// The context never appears in the schema.
	typed := NewTool("typed", "", func(_ context.Context, _ *ToolContext, in struct {
		Query string `json:"query"`
	}) (string, error) {
		return in.Query, nil
	})
	if props := typed.Schema()["properties"].(map[string]any); len(props) != 1 || props["query"] == nil {
		t.Fatalf("schema properties %v", props)
	}
}
