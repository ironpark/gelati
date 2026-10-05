package agy

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"

	"github.com/ironpark/gelati/internal/safecall"
)

// Tool is a custom tool that runs in this process when the model calls it.
// Build one with NewTool, or NewToolWithSchema for an explicit schema, and
// list it in Options.Tools (or SubagentConfig.Tools).
type Tool struct {
	name        string
	description string
	schema      map[string]any
	run         func(ctx context.Context, tc *ToolContext, args map[string]any) (any, error)
}

// NewTool builds a tool from a typed function. The model sees a parameter
// schema derived from In (see SchemaFor for the rules; In is normally a
// struct with json tags and `description` tags), and its arguments are
// decoded into In through encoding/json before fn runs.
//
// fn's result is sent to the model as JSON: a map or struct as an object,
// anything else wrapped as {"result": value}. Image, Document, Audio and
// Video values anywhere in the result (directly, or inside slices and
// maps) are sent as media attachments instead. A returned error, or a
// panic, fails the call; the model sees the error message, and
// OnToolErrorHook may replace it.
//
// The ToolContext gives access to the conversation ID and a session-scoped
// state store; it is hidden from the model.
func NewTool[In, Out any](name, description string, fn func(ctx context.Context, tc *ToolContext, in In) (Out, error)) *Tool {
	return &Tool{
		name:        name,
		description: description,
		schema:      schemaForType(reflect.TypeFor[In]()),
		run: func(ctx context.Context, tc *ToolContext, args map[string]any) (any, error) {
			var in In
			if err := (ToolCall{Args: args}).DecodeArgs(&in); err != nil {
				return nil, fmt.Errorf("invalid arguments for tool %q: %w", name, err)
			}
			return fn(ctx, tc, in)
		},
	}
}

// DecodeArgs decodes the call's arguments into v through encoding/json, as
// NewTool decodes them into its input type. Nil Args decode as an empty
// object.
func (c ToolCall) DecodeArgs(v any) error {
	if c.Args == nil {
		return jsonConvert(map[string]any{}, v)
	}
	return jsonConvert(c.Args, v)
}

// NewToolWithSchema builds a tool with an explicit JSON parameter schema;
// fn receives the raw arguments. Upstream calls this ToolWithSchema. The
// schema is normalized with NormalizeSchema before it is sent.
func NewToolWithSchema(name, description string, schema map[string]any, fn func(ctx context.Context, tc *ToolContext, args map[string]any) (any, error)) *Tool {
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return &Tool{name: name, description: description, schema: schema, run: fn}
}

// Name returns the tool name.
func (t *Tool) Name() string { return t.name }

// Description returns the tool description.
func (t *Tool) Description() string { return t.description }

// Schema returns the normalized JSON schema of the tool's parameters.
func (t *Tool) Schema() map[string]any { return NormalizeSchema(t.schema).(map[string]any) }

// Call runs the tool with the given arguments, recovering a panic as an
// error. tc may be nil outside a session.
func (t *Tool) Call(ctx context.Context, tc *ToolContext, args map[string]any) (result any, err error) {
	defer safecall.Recover(&err, fmt.Sprintf("tool %q", t.name))
	return t.run(ctx, tc, args)
}

// ToolContext is handed to every custom tool call. It exposes the
// conversation ID and a state store that lives as long as the session and
// is shared by all tools (but not with hooks). Its methods are safe for
// concurrent use.
type ToolContext struct {
	StateStore
	conv *Conversation
}

func newToolContext(conv *Conversation) *ToolContext { return &ToolContext{conv: conv} }

// ConversationID returns the conversation identifier, or "" when the tool
// runs outside a session.
func (tc *ToolContext) ConversationID() string {
	if tc == nil || tc.conv == nil {
		return ""
	}
	return tc.conv.ConversationID()
}

// toolRunner is the registry and executor of custom tools (upstream
// ToolRunner).
type toolRunner struct {
	mu    sync.RWMutex
	tools map[string]*Tool
	order []string
	tc    *ToolContext
}

func newToolRunner(tools []*Tool) (*toolRunner, error) {
	r := &toolRunner{tools: make(map[string]*Tool)}
	for _, t := range tools {
		if err := r.register(t); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *toolRunner) register(t *Tool) error {
	if t == nil || t.run == nil {
		return validationErrorf("nil tool")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[t.name]; ok {
		return validationErrorf("Tool '%s' is already registered.", t.name)
	}
	r.tools[t.name] = t
	r.order = append(r.order, t.name)
	return nil
}

func (r *toolRunner) unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		//lint:ignore ST1005 message text follows the upstream SDK
		return fmt.Errorf("Tool '%s' is not registered.", name)
	}
	delete(r.tools, name)
	r.order = slices.DeleteFunc(r.order, func(n string) bool { return n == name })
	return nil
}

func (r *toolRunner) has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[name]
	return ok
}

// list returns the registered tools in registration order.
func (r *toolRunner) list() []*Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Tool, len(r.order))
	for i, n := range r.order {
		out[i] = r.tools[n]
	}
	return out
}

func (r *toolRunner) setContext(tc *ToolContext) {
	r.mu.Lock()
	r.tc = tc
	r.mu.Unlock()
}

// execute runs a registered tool.
func (r *toolRunner) execute(ctx context.Context, name string, args map[string]any) (any, error) {
	r.mu.RLock()
	t, tc := r.tools[name], r.tc
	r.mu.RUnlock()
	if t == nil {
		//lint:ignore ST1005 message text follows the upstream SDK
		return nil, fmt.Errorf("Tool '%s' is not registered.", name)
	}
	return t.Call(ctx, tc, maps.Clone(args))
}

// processToolCalls runs calls concurrently and returns one result per call,
// in order. Unknown tools and failures become error results; the call's ID,
// step ID and server name are carried over.
func (r *toolRunner) processToolCalls(ctx context.Context, calls []*ToolCall) []*ToolResult {
	results := make([]*ToolResult, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Go(func() {
			res := &ToolResult{ID: call.ID, StepID: call.StepID, ServerName: call.ServerName, Name: call.Name}
			if !r.has(call.Name) {
				res.Error = fmt.Sprintf("Unknown tool: '%s'", call.Name)
				results[i] = res
				return
			}
			out, err := r.execute(ctx, call.Name, call.Args)
			if err != nil {
				res.Error, res.Err = err.Error(), err
			} else {
				res.Result = out
			}
			results[i] = res
		})
	}
	wg.Wait()
	return results
}
