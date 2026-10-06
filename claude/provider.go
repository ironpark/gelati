package claude

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
)

// Provider returns the gelati.Provider of Claude Code, named "claude", which
// opens a Client configured by base with the gelati.Config of gelati.Open set
// over it. Its non-zero fields override base:
//
//   - Model sets Options.Model and Dir sets Options.Cwd.
//   - Instructions are appended to the system prompt: as the Append of
//     Claude Code's default prompt when base sets no SystemPrompt; after
//     the Append of a SystemPromptPreset; after the text
//     of a SystemPromptText; and as a last block of SystemPromptBlocks or
//     SystemPromptCustom. A SystemPromptFile cannot be extended, so
//     Instructions then fail with an error matching errors.ErrUnsupported.
//   - OutputSchema sets Options.OutputFormat to a json_schema format.
//   - CLIPath and Logger set the options of the same name, and Env is
//     merged over Options.Env, its keys winning.
//   - Approve sets Options.CanUseTool and clears
//     Options.PermissionPromptToolName, so it answers the calls the
//     permission mode does not allow. A ToolRequest's Raw is the
//     ToolPermissionContext. A denial without a reason, or an error
//     without a message, tells Claude that the user denied the call.
//   - Tools are served by an in-process MCP server named "gelati", added to
//     Options.MCPServers; base must not have a server of that name. Claude
//     names the tools mcp__gelati__<name>, and those names are added to
//     Options.AllowedTools so that the tools run without asking. Events and
//     ToolRequests name them <name>, as the other providers do.
//
// A prompt of a single text is sent as text, and any other as content
// blocks; an image becomes an ImageBlock with a base64 source.
//
// Unlike Options, whose nil SystemPrompt sends an empty one, a base without
// a SystemPrompt runs with Claude Code's default prompt, as the other
// providers run with theirs. Options.IncludePartialMessages is always
// enabled, so that answers stream as text deltas. Usage.InputTokens counts
// cache reads and writes too, as the other providers count them. The
// Agent's Native method returns the *Client, its ID the session id, and the
// Raw fields of its events and results the Message and *ResultMessage they
// come from.
func Provider(base Options) gelati.Provider {
	return &provider{base: base}
}

// provider is the gelati.Provider Provider returns.
type provider struct {
	base Options
}

func (*provider) Name() string { return "claude" }

func (p *provider) Open(ctx context.Context, cfg gelati.Config) (gelati.Conn, error) {
	opts, err := providerOptions(p.base, cfg)
	if err != nil {
		return nil, err
	}
	client, err := New(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &providerConn{client: client, tools: newToolNames(cfg.Tools)}, nil
}

// providerOptions sets cfg over base as Provider documents. base is not
// modified.
func providerOptions(base Options, cfg gelati.Config) (Options, error) {
	opts := base
	if opts.SystemPrompt == nil {
		opts.SystemPrompt = &SystemPromptPreset{}
	}
	if cfg.Model != "" {
		opts.Model = cfg.Model
	}
	if cfg.Dir != "" {
		opts.Cwd = cfg.Dir
	}
	if cfg.Instructions != "" {
		prompt, err := appendInstructions(opts.SystemPrompt, cfg.Instructions)
		if err != nil {
			return Options{}, err
		}
		opts.SystemPrompt = prompt
	}
	if cfg.OutputSchema != nil {
		opts.OutputFormat = map[string]any{"type": "json_schema", "schema": cfg.OutputSchema}
	}
	if cfg.CLIPath != "" {
		opts.CLIPath = cfg.CLIPath
	}
	if len(cfg.Env) > 0 {
		env := make(map[string]string, len(base.Env)+len(cfg.Env))
		maps.Copy(env, base.Env)
		maps.Copy(env, cfg.Env)
		opts.Env = env
	}
	if cfg.Logger != nil {
		opts.Logger = cfg.Logger
	}
	if cfg.Approve != nil {
		opts.CanUseTool = canUseTool(cfg.Approve, newToolNames(cfg.Tools))
		opts.PermissionPromptToolName = ""
	}
	if len(cfg.Tools) > 0 {
		if _, ok := base.MCPServers[toolServer]; ok {
			return Options{}, fmt.Errorf("claude: Config.Tools need the MCP server name %q, which Options.MCPServers already uses", toolServer)
		}
		servers := make(map[string]MCPServerConfig, len(base.MCPServers)+1)
		maps.Copy(servers, base.MCPServers)
		servers[toolServer] = toolMCPServer(cfg.Tools)
		opts.MCPServers = servers
		allowed := slices.Clip(base.AllowedTools)
		for _, tool := range cfg.Tools {
			allowed = append(allowed, toolPrefix+tool.Name)
		}
		opts.AllowedTools = allowed
	}
	opts.IncludePartialMessages = true
	return opts, nil
}

// toolServer names the in-process MCP server of Config.Tools.
const toolServer = "gelati"

// toolPrefix starts the names Claude gives the tools of toolServer.
const toolPrefix = "mcp__" + toolServer + "__"

// deniedMessage is what Claude reads of a denial without a reason. The
// message becomes the content of an error tool result, which the API
// rejects when empty.
const deniedMessage = "The user denied this tool call."

// toolMCPServer returns the MCP server that runs tools.
func toolMCPServer(tools []gelati.Tool) *MCPSDKServerConfig {
	defs := make([]ToolDef, len(tools))
	for i, tool := range tools {
		run := tool.Run
		defs[i] = ToolDef{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
			Handler: func(ctx context.Context, args jsontext.Value) (ToolResult, error) {
				text, err := run(ctx, args)
				if err != nil {
					return ErrorResult("%s", err), nil
				}
				return TextResult(text), nil
			},
		}
	}
	return NewSDKMCPServer(toolServer, "", defs...)
}

// toolNames is the set of the names of Config.Tools.
type toolNames map[string]bool

func newToolNames(tools []gelati.Tool) toolNames {
	names := make(toolNames, len(tools))
	for _, tool := range tools {
		names[tool.Name] = true
	}
	return names
}

// name returns the name gelati reports for the tool Claude calls name: name
// without toolPrefix for one of Config.Tools, name itself for any other.
func (n toolNames) name(name string) string {
	if rest, ok := strings.CutPrefix(name, toolPrefix); ok && n[rest] {
		return rest
	}
	return name
}

// canUseTool adapts approve to a CanUseTool, as Provider documents.
func canUseTool(approve func(context.Context, gelati.ToolRequest) (gelati.Decision, error), names toolNames) CanUseTool {
	return func(ctx context.Context, toolName string, input map[string]any, permCtx ToolPermissionContext) (PermissionResult, error) {
		decision, err := approve(ctx, gelati.ToolRequest{
			ID:     permCtx.ToolUseID,
			Name:   names.name(toolName),
			Input:  input,
			Reason: permCtx.DecisionReason,
			Raw:    permCtx,
		})
		if err != nil {
			decision = gelati.Deny(err.Error())
		}
		if decision.Allowed {
			return &PermissionResultAllow{}, nil
		}
		if decision.Reason == "" {
			decision.Reason = deniedMessage
		}
		return &PermissionResultDeny{Message: decision.Reason}, nil
	}
}

// appendInstructions returns prompt with instructions appended, as Provider
// documents.
func appendInstructions(prompt SystemPrompt, instructions string) (SystemPrompt, error) {
	switch p := prompt.(type) {
	case *SystemPromptPreset:
		var out SystemPromptPreset
		if p != nil {
			out = *p
		}
		out.Append = joinPrompt(out.Append, instructions)
		return &out, nil
	case SystemPromptText:
		return SystemPromptText(joinPrompt(string(p), instructions)), nil
	case SystemPromptBlocks:
		return append(slices.Clip(p), instructions), nil
	case *SystemPromptCustom:
		var out SystemPromptCustom
		if p != nil {
			out = *p
		}
		out.Prompt = append(slices.Clip(out.Prompt), instructions)
		return &out, nil
	}
	return nil, fmt.Errorf("claude: Config.Instructions cannot be added to a %T system prompt: %w", prompt, errors.ErrUnsupported)
}

// joinPrompt appends extra to prompt, separated by a blank line.
func joinPrompt(prompt, extra string) string {
	if prompt == "" {
		return extra
	}
	return prompt + "\n\n" + extra
}

// providerConn is the gelati.Conn of a Client.
type providerConn struct {
	client *Client
	// tools names Config.Tools, whose names events report unprefixed.
	tools toolNames
}

func (c *providerConn) Send(ctx context.Context, input []gelati.Input) (gelati.TurnConn, error) {
	in, err := userInput(input)
	if err != nil {
		return nil, err
	}
	ts, err := c.client.Send(ctx, in)
	if err != nil {
		return nil, err
	}
	return &providerTurn{conn: c, ts: ts}, nil
}

// userInput converts the parts of a gelati prompt into one user message.
func userInput(input []gelati.Input) (UserInput, error) {
	blocks := make([]ContentBlock, 0, len(input))
	for _, in := range input {
		switch in := in.(type) {
		case gelati.TextInput:
			blocks = append(blocks, &TextBlock{Text: in.Text})
		case gelati.ImageInput:
			blocks = append(blocks, &ImageBlock{Source: BlockSource{
				Type:      SourceBase64,
				MediaType: in.MIMEType,
				Data:      base64.StdEncoding.EncodeToString(in.Data),
			}})
		default:
			return UserInput{}, fmt.Errorf("claude: unsupported input %T: %w", in, errors.ErrUnsupported)
		}
	}
	if len(blocks) == 0 {
		return UserInput{}, errors.New("claude: Send needs input")
	}
	if text, ok := blocks[0].(*TextBlock); ok && len(blocks) == 1 {
		return Text(text.Text), nil
	}
	return UserInput{Blocks: blocks}, nil
}

func (c *providerConn) ID() string            { return c.client.SessionID() }
func (c *providerConn) Native() any           { return c.client }
func (c *providerConn) Done() <-chan struct{} { return c.client.Done() }
func (c *providerConn) Err() error            { return c.client.Err() }
func (c *providerConn) Close() error          { return c.client.Close() }

// providerTurn is the gelati.TurnConn of a TurnStream.
type providerTurn struct {
	conn *providerConn
	ts   *TurnStream

	// pending holds the events of a message that the reader stopped
	// reading in the middle of; the next Events yields them first.
	pending []gelati.Event
	// buf is reused for the events of each message.
	buf []gelati.Event
	// tools maps the ids of the turn's tool calls to the tools' names, for
	// their results.
	tools map[string]string
}

func (t *providerTurn) Events(ctx context.Context) iter.Seq2[gelati.Event, error] {
	return func(yield func(gelati.Event, error) bool) {
		if !t.yieldPending(yield) {
			return
		}
		for msg, err := range t.ts.Events(ctx) {
			if err != nil {
				yield(gelati.Event{}, err)
				return
			}
			t.buf = t.events(msg, t.buf[:0])
			t.pending = t.buf
			if !t.yieldPending(yield) {
				return
			}
		}
	}
}

// yieldPending yields the pending events and reports whether yield asked for
// more.
func (t *providerTurn) yieldPending(yield func(gelati.Event, error) bool) bool {
	for len(t.pending) > 0 {
		ev := t.pending[0]
		t.pending = t.pending[1:]
		if !yield(ev, nil) {
			return false
		}
	}
	t.pending = nil
	return true
}

// events maps m to its gelati events: the deltas, tool calls and tool
// results of the main conversation, and an EventOther for a message that
// has none, appended to evs. As TurnStream.Text does, it skips the text
// and thinking of the AssistantMessages whose deltas were streamed.
func (t *providerTurn) events(m Message, evs []gelati.Event) []gelati.Event {
	add := func(ev gelati.Event) {
		ev.Raw = m
		evs = append(evs, ev)
	}
	t.ts.emitContent(m, func(text string, thinking bool) bool {
		kind := gelati.EventTextDelta
		if thinking {
			kind = gelati.EventThoughtDelta
		}
		add(gelati.Event{Kind: kind, Text: text})
		return true
	})
	switch m := m.(type) {
	case *AssistantMessage:
		if m.ParentToolUseID != "" {
			break
		}
		for _, block := range m.Content {
			if b, ok := block.(*ToolUseBlock); ok {
				if t.tools == nil {
					t.tools = map[string]string{}
				}
				name := t.conn.tools.name(b.Name)
				t.tools[b.ID] = name
				add(gelati.Event{Kind: gelati.EventToolCall, Tool: &gelati.ToolCall{
					ID: b.ID, Name: name, Input: b.Input}})
			}
		}
	case *UserMessage:
		if m.ParentToolUseID != "" {
			break
		}
		for _, block := range m.Content {
			if b, ok := block.(*ToolResultBlock); ok {
				add(gelati.Event{Kind: gelati.EventToolResult, Tool: &gelati.ToolCall{
					ID:      b.ToolUseID,
					Name:    t.tools[b.ToolUseID],
					Output:  toolResultText(b),
					IsError: b.IsError != nil && *b.IsError,
				}})
			}
		}
	}
	if len(evs) == 0 {
		add(gelati.Event{Kind: gelati.EventOther})
	}
	return evs
}

// toolResultText returns the text of a tool result: its string content, or
// the text blocks of its content list joined by newlines.
func toolResultText(b *ToolResultBlock) string {
	if b.ContentText != nil {
		return *b.ContentText
	}
	var texts []string
	for _, item := range b.ContentList {
		if text, ok := item["text"].(string); ok && item["type"] == "text" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func (t *providerTurn) Result(ctx context.Context) (*gelati.Result, error) {
	res, err := t.ts.Result(ctx)
	if res == nil {
		return nil, err
	}
	return &gelati.Result{
		Text:             res.Text(),
		StructuredOutput: res.StructuredOutput,
		Usage: gelati.Usage{
			InputTokens:       int64(res.Usage.InputTokens + res.Usage.CacheReadInputTokens + res.Usage.CacheCreationInputTokens),
			CachedInputTokens: int64(res.Usage.CacheReadInputTokens),
			OutputTokens:      int64(res.Usage.OutputTokens),
		},
		CostUSD: res.TotalCostUSD,
		Raw:     res,
	}, err
}

func (t *providerTurn) Cancel(ctx context.Context) error { return t.ts.Cancel(ctx) }
func (t *providerTurn) Close() error                     { return t.ts.Close() }
