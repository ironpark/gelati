package codex

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/ironpark/gelati"
	"github.com/ironpark/gelati/internal/jsonschema"
	"github.com/ironpark/gelati/internal/jsonx"
)

// Provider returns the gelati.Provider of Codex, named "codex". Its sessions
// start a Client from base and one thread on it from thread, with
// gelati.Config set over them: CLIPath, Env (merged over base.Env) and Logger
// override the client options; Model and Dir set the thread's Model and Cwd,
// and Instructions its DeveloperInstructions, after thread's own separated
// by a blank line. Config.OutputSchema is made strict and sent with every
// turn as TurnOptions.OutputSchema.
//
// The session owns its client: closing it closes the client. Native returns
// the *Thread, and Thread.Client the client, for approvals, auth and the
// rest of this package's API. Event.Raw is the codex Event and Result.Raw
// the *TurnResult.
//
// Agent message deltas map to text deltas and reasoning deltas to thought
// deltas. Command executions ("command"), file changes ("fileChange"), MCP
// tool calls ("server/tool"), dynamic tool calls (the tool's name), web
// searches ("webSearch") and collab agent calls (the tool's name) map to a
// tool call when they start and a tool result when they complete; every
// other event is gelati.EventOther. Usage is the turn's own,
// TurnResult.TurnUsage. Images are sent as data URLs.
//
// Config.Approve answers the approval requests of the thread's approval
// policy, overriding base.Approvals for them: command approvals as
// "command" with the command, its cwd and any network target as input;
// file change approvals as "fileChange" with the changes, as their tool
// call reports them; and request_permissions as "permissions" with the
// requested permissions, all granted when allowed. Approval allows once
// (DecisionAccept) and a denial declines (DecisionDecline). Without
// Approve, base.Approvals answers, declining when it is nil.
//
// Config.Tools are added to the thread's DynamicTools and run when called;
// base.Approvals still answers calls to the other dynamic tools.
func Provider(base Options, thread StartThreadParams) gelati.Provider {
	return &provider{base: base, thread: thread, newClient: New}
}

// provider implements gelati.Provider.
type provider struct {
	base   Options
	thread StartThreadParams
	// newClient is New; tests substitute a fake app-server.
	newClient func(context.Context, Options) (*Client, error)
}

func (*provider) Name() string { return "codex" }

func (p *provider) Open(ctx context.Context, cfg gelati.Config) (gelati.Conn, error) {
	c := &conn{approve: cfg.Approve}

	opts := p.base
	if cfg.CLIPath != "" {
		opts.CLIPath = cfg.CLIPath
	}
	if len(cfg.Env) > 0 {
		env := make(map[string]string, len(opts.Env)+len(cfg.Env))
		maps.Copy(env, opts.Env)
		maps.Copy(env, cfg.Env)
		opts.Env = env
	}
	if cfg.Logger != nil {
		opts.Logger = cfg.Logger
	}
	if cfg.Approve != nil || len(cfg.Tools) > 0 {
		opts.Approvals = c.approvals(opts.Approvals, cfg.Tools)
	}

	params := p.thread
	if cfg.Model != "" {
		params.Model = cfg.Model
	}
	if cfg.Dir != "" {
		params.Cwd = cfg.Dir
	}
	if cfg.Instructions != "" {
		if params.DeveloperInstructions != "" {
			params.DeveloperInstructions += "\n\n" + cfg.Instructions
		} else {
			params.DeveloperInstructions = cfg.Instructions
		}
	}
	if cfg.OutputSchema != nil {
		// The app-server accepts only the strict form.
		c.schema = jsonschema.MakeStrict(cfg.OutputSchema)
	}
	var err error
	if params.DynamicTools, err = dynamicTools(params.DynamicTools, cfg.Tools); err != nil {
		return nil, err
	}

	client, err := p.newClient(ctx, opts)
	if err != nil {
		return nil, err
	}
	thread, err := client.StartThread(ctx, params)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	c.client, c.thread = client, thread
	return c, nil
}

// conn implements gelati.Conn over one thread of a client it owns.
type conn struct {
	client  *Client
	thread  *Thread
	schema  map[string]any
	approve func(context.Context, gelati.ToolRequest) (gelati.Decision, error)
}

// approvals returns base with the session's Approve answering command, file
// change and permission requests, and tools answering their dynamic tool
// calls.
func (c *conn) approvals(base ApprovalHandler, tools []gelati.Tool) ApprovalHandler {
	f := funcsOf(base)
	if c.approve != nil {
		f.Command, f.FileChange, f.Permissions = c.approveCommand, c.approveFileChange, c.approvePermissions
	}
	if len(tools) > 0 {
		byName := make(map[string]gelati.Tool, len(tools))
		for _, t := range tools {
			byName[t.Name] = t
		}
		next := f.CallDynamicTool
		f.DynamicTool = func(ctx context.Context, req *DynamicToolCallRequest) (*DynamicToolCallResponse, error) {
			t, ok := byName[req.Tool]
			if !ok || req.Namespace != "" {
				return next(ctx, req)
			}
			text, err := t.Run(ctx, req.Arguments)
			if err != nil {
				return DynamicToolText(err.Error(), false), nil
			}
			return DynamicToolText(text, true), nil
		}
	}
	return f
}

// ask reports whether Approve allows req; an error denies it.
func (c *conn) ask(ctx context.Context, req gelati.ToolRequest) bool {
	d, err := c.approve(ctx, req)
	return err == nil && d.Allowed
}

func (c *conn) approveCommand(ctx context.Context, req *CommandApprovalRequest) (Decision, error) {
	input := map[string]any{"command": req.Command}
	if req.Cwd != "" {
		input["cwd"] = req.Cwd
	}
	if n := req.NetworkApprovalContext; n != nil {
		input["network"] = map[string]any{"host": n.Host, "protocol": n.Protocol}
	}
	if c.ask(ctx, gelati.ToolRequest{ID: req.ItemID, Name: "command", Input: input, Reason: req.Reason, Raw: req}) {
		return DecisionAccept, nil
	}
	return DecisionDecline, nil
}

func (c *conn) approveFileChange(ctx context.Context, req *FileChangeApprovalRequest) (Decision, error) {
	input := map[string]any{}
	if req.Changes != nil {
		input = toolCall(&ThreadItem{Item: &FileChangeItem{ID: req.ItemID, Changes: req.Changes}}).Input
	}
	if req.GrantRoot != "" {
		input["grantRoot"] = req.GrantRoot
	}
	if c.ask(ctx, gelati.ToolRequest{ID: req.ItemID, Name: "fileChange", Input: input, Reason: req.Reason, Raw: req}) {
		return DecisionAccept, nil
	}
	return DecisionDecline, nil
}

func (c *conn) approvePermissions(ctx context.Context, req *PermissionsRequest) (*PermissionsResponse, error) {
	if len(req.Permissions) > 0 && c.ask(ctx, gelati.ToolRequest{ID: req.ItemID, Name: "permissions", Input: arguments(req.Permissions), Reason: req.Reason, Raw: req}) {
		return &PermissionsResponse{Permissions: req.Permissions}, nil
	}
	return grantNothing(), nil
}

// dynamicTools adds tools to the thread's dynamic tools.
func dynamicTools(specs []DynamicToolSpec, tools []gelati.Tool) ([]DynamicToolSpec, error) {
	if len(tools) == 0 {
		return specs, nil
	}
	out := slices.Clip(specs)
	for _, t := range tools {
		if slices.ContainsFunc(specs, func(s DynamicToolSpec) bool { return s.Type != DynamicToolNamespace && s.Name == t.Name }) {
			return nil, fmt.Errorf("codex: tool %q is also one of the thread's DynamicTools", t.Name)
		}
		out = append(out, DynamicToolSpec{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out, nil
}

func (c *conn) Send(ctx context.Context, input []gelati.Input) (gelati.TurnConn, error) {
	items := make([]InputItem, 0, len(input))
	for _, in := range input {
		switch in := in.(type) {
		case gelati.TextInput:
			items = append(items, Text(in.Text))
		case gelati.ImageInput:
			items = append(items, ImageInput{URL: "data:" + in.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(in.Data)})
		default:
			return nil, fmt.Errorf("codex: input %T: %w", in, errors.ErrUnsupported)
		}
	}
	stream, err := c.thread.SendTurn(ctx, TurnRequest{Input: items, TurnOptions: TurnOptions{OutputSchema: c.schema}})
	if err != nil {
		return nil, err
	}
	return &turnConn{stream: stream, structured: c.schema != nil}, nil
}

func (c *conn) ID() string            { return c.thread.ID() }
func (c *conn) Native() any           { return c.thread }
func (c *conn) Done() <-chan struct{} { return c.client.Done() }
func (c *conn) Err() error            { return c.client.Err() }
func (c *conn) Close() error          { return c.client.Close() }

// turnConn implements gelati.TurnConn over a TurnStream.
type turnConn struct {
	stream     *TurnStream
	structured bool
}

func (t *turnConn) Events(ctx context.Context) iter.Seq2[gelati.Event, error] {
	return func(yield func(gelati.Event, error) bool) {
		for ev, err := range t.stream.Events(ctx) {
			if err != nil {
				yield(gelati.Event{}, err)
				return
			}
			if !yield(mapEvent(ev), nil) {
				return
			}
		}
	}
}

func (t *turnConn) Result(ctx context.Context) (*gelati.Result, error) {
	r, err := t.stream.Result(ctx)
	if r == nil {
		return nil, err
	}
	res := &gelati.Result{Text: r.Text(), Raw: r}
	if t.structured && res.Text != "" {
		if v := jsontext.Value(res.Text); v.IsValid() {
			res.StructuredOutput = v
		}
	}
	// The turn's own usage, or else its last request's.
	u := r.TurnUsage
	if u == nil && r.Usage != nil {
		u = &r.Usage.Last
	}
	if u != nil {
		res.Usage = gelati.Usage{
			InputTokens:       u.InputTokens,
			CachedInputTokens: u.CachedInputTokens,
			OutputTokens:      u.OutputTokens,
		}
	}
	return res, err
}

func (t *turnConn) Cancel(ctx context.Context) error { return t.stream.Cancel(ctx) }
func (t *turnConn) Close() error                     { return t.stream.Close() }

// mapEvent converts a turn event to its gelati form.
func mapEvent(ev Event) gelati.Event {
	out := gelati.Event{Kind: gelati.EventOther, Raw: ev}
	switch ev.Kind {
	case EventAgentMessageDelta:
		out.Kind, out.Text = gelati.EventTextDelta, ev.Delta
	case EventReasoningDelta:
		if ev.Method != MethodReasoningSummaryPartAdded {
			out.Kind, out.Text = gelati.EventThoughtDelta, ev.Delta
		}
	case EventItemStarted:
		if call := toolCall(ev.Item); call != nil {
			out.Kind, out.Tool = gelati.EventToolCall, call
		}
	case EventItemCompleted:
		if call := toolResult(ev.Item); call != nil {
			out.Kind, out.Tool = gelati.EventToolResult, call
		}
	}
	return out
}

// toolCall describes a started tool-like item, or returns nil for any other
// item.
func toolCall(item *ThreadItem) *gelati.ToolCall {
	if item == nil {
		return nil
	}
	switch it := item.Item.(type) {
	case *CommandExecutionItem:
		input := map[string]any{"command": it.Command}
		if it.Cwd != "" {
			input["cwd"] = it.Cwd
		}
		return &gelati.ToolCall{ID: it.ID, Name: "command", Input: input}
	case *FileChangeItem:
		changes := make([]any, 0, len(it.Changes))
		for _, ch := range it.Changes {
			change := map[string]any{"path": ch.Path, "kind": ch.Kind.Type, "diff": ch.Diff}
			if ch.Kind.MovePath != "" {
				change["movePath"] = ch.Kind.MovePath
			}
			changes = append(changes, change)
		}
		return &gelati.ToolCall{ID: it.ID, Name: "fileChange", Input: map[string]any{"changes": changes}}
	case *McpToolCallItem:
		return &gelati.ToolCall{ID: it.ID, Name: mcpName(it), Input: arguments(it.Arguments)}
	case *DynamicToolCallItem:
		return &gelati.ToolCall{ID: it.ID, Name: it.Tool, Input: arguments(it.Arguments)}
	case *WebSearchItem:
		return &gelati.ToolCall{ID: it.ID, Name: "webSearch", Input: webSearchInput(it)}
	case *CollabAgentToolCallItem:
		input := map[string]any{}
		if it.Prompt != "" {
			input["prompt"] = it.Prompt
		}
		if it.Model != "" {
			input["model"] = it.Model
		}
		if len(it.ReceiverThreadIDs) > 0 {
			input["receiverThreadIds"] = it.ReceiverThreadIDs
		}
		return &gelati.ToolCall{ID: it.ID, Name: it.Tool, Input: input}
	}
	return nil
}

// toolResult describes a completed tool-like item, or returns nil for any
// other item.
func toolResult(item *ThreadItem) *gelati.ToolCall {
	if item == nil {
		return nil
	}
	switch it := item.Item.(type) {
	case *CommandExecutionItem:
		failed := statusFailed(it.Status) || (it.ExitCode != nil && *it.ExitCode != 0)
		return &gelati.ToolCall{ID: it.ID, Name: "command", Output: it.AggregatedOutput, IsError: failed}
	case *FileChangeItem:
		return &gelati.ToolCall{ID: it.ID, Name: "fileChange", IsError: statusFailed(it.Status)}
	case *McpToolCallItem:
		call := &gelati.ToolCall{ID: it.ID, Name: mcpName(it), Output: contentText(it.Result)}
		call.IsError = statusFailed(it.Status) || it.Error != nil || resultIsError(it.Result)
		if call.Output == "" && it.Error != nil {
			call.Output = it.Error.Message
		}
		return call
	case *DynamicToolCallItem:
		failed := statusFailed(it.Status) || (it.Success != nil && !*it.Success)
		return &gelati.ToolCall{ID: it.ID, Name: it.Tool, Output: contentText(it.ContentItems), IsError: failed}
	case *WebSearchItem:
		return &gelati.ToolCall{ID: it.ID, Name: "webSearch"}
	case *CollabAgentToolCallItem:
		return &gelati.ToolCall{ID: it.ID, Name: it.Tool, IsError: statusFailed(it.Status)}
	}
	return nil
}

// statusFailed reports whether an item status is a failure.
func statusFailed(status string) bool {
	return status == ItemStatusFailed || status == ItemStatusDeclined
}

// mcpName names an MCP tool call "server/tool".
func mcpName(it *McpToolCallItem) string {
	if it.Server == "" {
		return it.Tool
	}
	return it.Server + "/" + it.Tool
}

// webSearchInput builds the input of a web search from its query and action.
func webSearchInput(it *WebSearchItem) map[string]any {
	input := map[string]any{}
	if it.Query != "" {
		input["query"] = it.Query
	}
	if a := it.Action; a != nil {
		action := map[string]any{"type": a.Type}
		if a.Query != "" {
			action["query"] = a.Query
		}
		if len(a.Queries) > 0 {
			action["queries"] = a.Queries
		}
		if a.URL != "" {
			action["url"] = a.URL
		}
		if a.Pattern != "" {
			action["pattern"] = a.Pattern
		}
		input["action"] = action
	}
	return input
}

// arguments decodes tool arguments. An object becomes the input map; any
// other value is kept under "arguments".
func arguments(raw jsontext.Value) map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m map[string]any
	if err := jsonx.Unmarshal(raw, &m); err == nil {
		return m
	}
	var v any
	if err := jsonx.Unmarshal(raw, &v); err != nil {
		return map[string]any{"arguments": string(raw)}
	}
	return map[string]any{"arguments": v}
}

// contentText extracts the text of a tool result: the text parts of an MCP
// result's content or of a content item list, a bare string, or else the
// JSON itself.
func contentText(raw jsontext.Value) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var v any
	if err := jsonx.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	switch v := v.(type) {
	case string:
		return v
	case map[string]any:
		if parts, ok := v["content"].([]any); ok {
			if text, ok := joinText(parts); ok {
				return text
			}
		}
	case []any:
		if text, ok := joinText(v); ok {
			return text
		}
	}
	return string(raw)
}

// joinText joins the "text" members of content parts, reporting false when
// none has one.
func joinText(parts []any) (string, bool) {
	var texts []string
	for _, part := range parts {
		if m, ok := part.(map[string]any); ok {
			if text, ok := m["text"].(string); ok {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, "\n"), len(texts) > 0
}

// resultIsError reports whether an MCP result sets isError.
func resultIsError(raw jsontext.Value) bool {
	if len(raw) == 0 {
		return false
	}
	var r struct {
		IsError bool `json:"isError"`
	}
	return jsonx.Unmarshal(raw, &r) == nil && r.IsError
}
