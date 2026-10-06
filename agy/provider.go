package agy

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/ironpark/gelati"
	"github.com/ironpark/gelati/internal/jsonx"
)

// Provider returns the gelati provider of Antigravity agents, named "agy".
// Its sessions start from base, with the gelati.Config fields that are set
// laid over a copy of it (base itself is never modified):
//
//   - Model sets Model.
//   - Dir becomes the first of Workspaces; base's other workspaces are
//     kept after it. The harness has no working directory of its own: its
//     file tools work in the workspaces.
//   - Instructions are appended to the default system instructions as a
//     "user_system_instructions" section, as TextSystemInstructions is.
//     With base SystemInstructions they come after base's: another section
//     of TextSystemInstructions or TemplatedSystemInstructions, or a
//     further paragraph of CustomSystemInstructions, which replace the
//     defaults.
//   - OutputSchema sets ResponseSchema.
//   - CLIPath and Logger set their fields, and Env is merged over base's
//     Env, Config's value winning per key.
//   - Approve answers the calls the policies send to the user: by default
//     run_command, which the native default denies. A nil base Policies
//     becomes ConfirmRunCommandPolicies(Approve); otherwise Approve becomes
//     the AskUser handler of every DecisionAskUser or Auto policy,
//     replacing base's. The ToolRequest's Raw is the *ToolCall. Approve's
//     Reason is not carried; an error denies the call, its message the
//     denial reason.
//   - Tools are appended to Tools as NewToolWithSchema tools, whose text
//     the model reads as {"result": text}. Each gets a Policy approving it
//     by name, after the default policies when base Policies is nil (an
//     empty one, which applies no rules, stays empty), so wildcard deny or
//     ask policies do not hold them back. Names of builtin tools, "*" and
//     names with "/" are rejected.
//
// gelati.TextInput is sent as Text and gelati.ImageInput as Image; other
// inputs fail with an error matching errors.ErrUnsupported. A turn's
// *TextChunk, *ThoughtChunk and *ToolCall become gelati.EventTextDelta,
// EventThoughtDelta and EventToolCall events, with the chunk as Event.Raw;
// the turn stream reports no tool results, custom tools' included, so there
// are no EventToolResult events (PostToolCallHook observes them). The
// gelati.Result has the TurnResult's text, structured output and usage —
// InputTokens is the prompt token count (cached tokens included),
// OutputTokens the candidates plus thoughts token counts — and the
// *TurnResult as Raw. Agent.ID is the ConversationID, and Agent.Native
// returns the *Agent.
func Provider(base Options) gelati.Provider {
	return provider{base: base}
}

type provider struct {
	base Options
}

func (provider) Name() string { return "agy" }

func (p provider) Open(ctx context.Context, cfg gelati.Config) (gelati.Conn, error) {
	opts, err := p.options(cfg)
	if err != nil {
		return nil, err
	}
	agent, err := New(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &providerConn{agent: agent}, nil
}

// options lays cfg over a copy of the provider's base options.
func (p provider) options(cfg gelati.Config) (Options, error) {
	opts := p.base.clone()
	if cfg.Model != "" {
		opts.Model = cfg.Model
	}
	if cfg.Dir != "" {
		opts.Workspaces = append([]string{cfg.Dir}, slices.DeleteFunc(opts.Workspaces, func(w string) bool { return w == cfg.Dir })...)
	}
	if cfg.Instructions != "" {
		opts.SystemInstructions = appendInstructions(opts.SystemInstructions, cfg.Instructions)
	}
	if cfg.OutputSchema != nil {
		opts.ResponseSchema = cfg.OutputSchema
	}
	if cfg.CLIPath != "" {
		opts.CLIPath = cfg.CLIPath
	}
	if len(cfg.Env) > 0 {
		if opts.Env == nil {
			opts.Env = make(map[string]string, len(cfg.Env))
		}
		maps.Copy(opts.Env, cfg.Env)
	}
	if cfg.Logger != nil {
		opts.Logger = cfg.Logger
	}
	if cfg.Approve == nil && len(cfg.Tools) == 0 {
		return opts, nil
	}
	var handler AskUserHandler
	if cfg.Approve != nil {
		handler = approveHandler(cfg.Approve)
	}
	if opts.Policies == nil {
		// The default policies, made explicit so they can be adjusted.
		opts.Policies = ConfirmRunCommandPolicies(handler)
	} else if handler != nil {
		for i := range opts.Policies {
			if pol := &opts.Policies[i]; pol.Auto || pol.Decision == DecisionAskUser {
				pol.AskUser = handler
			}
		}
	}
	builtin := AllTools()
	for _, t := range cfg.Tools {
		if slices.Contains(builtin, BuiltinTool(t.Name)) || t.Name == WildcardTool || strings.Contains(t.Name, "/") {
			return Options{}, fmt.Errorf("agy: tool name %q is reserved for builtin, wildcard or MCP tools", t.Name)
		}
		opts.Tools = append(opts.Tools, NewToolWithSchema(t.Name, t.Description, t.InputSchema, runTool(t.Run)))
		// Empty Policies apply no rules, so an allow rule is only needed,
		// and only harmless to the safety guard, when there are rules.
		if len(opts.Policies) > 0 {
			opts.Policies = append(opts.Policies, Policy{Tool: t.Name, Decision: DecisionApprove, Name: "gelati_tool"})
		}
	}
	return opts, nil
}

// approveHandler adapts a gelati Approve function to an AskUserHandler. An
// error denies the call: agy reports it as the denial reason without failing
// the turn.
func approveHandler(approve func(context.Context, gelati.ToolRequest) (gelati.Decision, error)) AskUserHandler {
	return func(ctx context.Context, call ToolCall, reason string) (bool, error) {
		d, err := approve(ctx, gelati.ToolRequest{ID: call.ID, Name: call.Name, Input: call.Args, Reason: reason, Raw: &call})
		if err != nil {
			return false, err
		}
		return d.Allowed, nil
	}
}

// runTool adapts a gelati tool's Run to a NewToolWithSchema function.
func runTool(run func(context.Context, jsontext.Value) (string, error)) func(context.Context, *ToolContext, map[string]any) (any, error) {
	return func(ctx context.Context, _ *ToolContext, args map[string]any) (any, error) {
		if args == nil {
			args = map[string]any{}
		}
		b, err := jsonx.Marshal(args, jsonx.Foreign)
		if err != nil {
			return nil, fmt.Errorf("encode tool arguments: %w", err)
		}
		return run(ctx, b)
	}
}

// appendInstructions adds text after the instructions si.
func appendInstructions(si SystemInstructions, text string) SystemInstructions {
	switch v := si.(type) {
	case TextSystemInstructions:
		if v == "" {
			return TextSystemInstructions(text)
		}
		return TemplatedSystemInstructions{Sections: []SystemInstructionSection{{Content: string(v)}, {Content: text}}}
	case TemplatedSystemInstructions:
		v.Sections = append(slices.Clip(v.Sections), SystemInstructionSection{Content: text})
		return v
	case *TemplatedSystemInstructions:
		if v != nil {
			return appendInstructions(*v, text)
		}
	case CustomSystemInstructions:
		if v.Text != "" {
			v.Text += "\n\n"
		}
		v.Text += text
		return v
	case *CustomSystemInstructions:
		if v != nil {
			return appendInstructions(*v, text)
		}
	}
	return TextSystemInstructions(text)
}

// providerConn is an Agent as a gelati.Conn.
type providerConn struct {
	agent *Agent
}

func (c *providerConn) Send(ctx context.Context, input []gelati.Input) (gelati.TurnConn, error) {
	content, err := inputContent(input)
	if err != nil {
		return nil, err
	}
	stream, err := c.agent.Send(ctx, content...)
	if err != nil {
		return nil, err
	}
	return &providerTurn{stream: stream}, nil
}

func (c *providerConn) ID() string            { return c.agent.ConversationID() }
func (c *providerConn) Native() any           { return c.agent }
func (c *providerConn) Done() <-chan struct{} { return c.agent.Done() }
func (c *providerConn) Err() error            { return c.agent.Err() }
func (c *providerConn) Close() error          { return c.agent.Close() }

// inputContent converts gelati prompt parts to Content.
func inputContent(input []gelati.Input) ([]Content, error) {
	content := make([]Content, 0, len(input))
	for _, in := range input {
		switch v := in.(type) {
		case gelati.TextInput:
			content = append(content, Text(v.Text))
		case gelati.ImageInput:
			content = append(content, Image{Data: v.Data, MIMEType: v.MIMEType})
		default:
			return nil, fmt.Errorf("agy: unsupported input %T: %w", in, errors.ErrUnsupported)
		}
	}
	return content, nil
}

// providerTurn is a TurnStream as a gelati.TurnConn.
type providerTurn struct {
	stream *TurnStream
}

func (t *providerTurn) Events(ctx context.Context) iter.Seq2[gelati.Event, error] {
	return func(yield func(gelati.Event, error) bool) {
		for ch, err := range t.stream.Events(ctx) {
			if err != nil {
				yield(gelati.Event{}, err)
				return
			}
			if !yield(chunkEvent(ch), nil) {
				return
			}
		}
	}
}

// chunkEvent converts a chunk to a gelati event.
func chunkEvent(ch Chunk) gelati.Event {
	ev := gelati.Event{Kind: gelati.EventOther, Raw: ch}
	switch c := ch.(type) {
	case *TextChunk:
		ev.Kind, ev.Text = gelati.EventTextDelta, c.Text
	case *ThoughtChunk:
		ev.Kind, ev.Text = gelati.EventThoughtDelta, c.Text
	case *ToolCall:
		ev.Kind, ev.Tool = gelati.EventToolCall, &gelati.ToolCall{ID: c.ID, Name: c.Name, Input: c.Args}
	}
	return ev
}

func (t *providerTurn) Result(ctx context.Context) (*gelati.Result, error) {
	r, err := t.stream.Result(ctx)
	if r == nil {
		return nil, err
	}
	res, convErr := turnResult(r)
	if err == nil {
		err = convErr
	}
	return res, err
}

// turnResult converts a TurnResult to a gelati result. It fails only when
// the structured output does not encode, and still returns the rest.
func turnResult(r *TurnResult) (*gelati.Result, error) {
	res := &gelati.Result{Text: r.Text(), Raw: r}
	if u := r.Usage; u != nil {
		res.Usage = gelati.Usage{
			InputTokens:       val(u.PromptTokenCount),
			CachedInputTokens: val(u.CachedContentTokenCount),
			OutputTokens:      val(u.CandidatesTokenCount) + val(u.ThoughtsTokenCount),
		}
	}
	if r.StructuredOutput != nil {
		b, err := jsonx.Marshal(r.StructuredOutput, jsonx.Foreign)
		if err != nil {
			return res, fmt.Errorf("agy: encode structured output: %w", err)
		}
		res.StructuredOutput = b
	}
	return res, nil
}

func (t *providerTurn) Cancel(ctx context.Context) error { return t.stream.Cancel(ctx) }
func (t *providerTurn) Close() error                     { return t.stream.Close() }
