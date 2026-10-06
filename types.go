package gelati

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"reflect"

	"github.com/ironpark/gelati/internal/jsonschema"
	"github.com/ironpark/gelati/internal/jsonx"
)

// Config holds the settings every provider understands. Zero fields keep the
// provider's own value, which comes from the native options its Provider was
// built with; set fields override them.
type Config struct {
	// Model selects the model, in the provider's naming.
	Model string
	// Dir is the working directory the agent operates in.
	Dir string
	// Instructions are added to the provider's default system prompt.
	Instructions string
	// OutputSchema asks for structured output matching this JSON schema;
	// SchemaFor derives one from a Go type. Result.DecodeStructuredOutput
	// reads the answer. Providers adapt the schema to their requirements.
	OutputSchema map[string]any

	// CLIPath is the provider's executable.
	CLIPath string
	// Env holds extra environment variables for the provider's process,
	// merged over the native options' Env.
	Env map[string]string
	// Logger receives the provider's diagnostics.
	Logger *slog.Logger

	// Approve answers the tool calls the provider asks permission for. Which
	// calls those are is the provider's own policy, from its native
	// options: Claude Code asks about the calls its permission mode does
	// not allow, Codex about the commands and file changes its approval
	// policy holds back, and Antigravity about the calls its policies send
	// to the user (run_command by default). Nil keeps the provider's native
	// answer, as its package documents. An error denies the call, with the
	// error's message as the reason.
	Approve func(ctx context.Context, req ToolRequest) (Decision, error)

	// Tools are custom tools the agent can call, which run in this process.
	// They run without asking Approve: the program that registers them
	// trusts them.
	Tools []Tool
}

// ToolRequest is a tool call awaiting approval.
type ToolRequest struct {
	// ID identifies the call; the matching EventToolCall has the same ID
	// when the provider reports one.
	ID string
	// Name is the tool's name, as its EventToolCall names it.
	Name string
	// Input holds the call's arguments, as far as the provider reports them.
	Input map[string]any
	// Reason is why the provider asks, when it says.
	Reason string
	// Raw is the provider's own request: a claude.ToolPermissionContext; a
	// *codex.CommandApprovalRequest, *codex.FileChangeApprovalRequest or
	// *codex.PermissionsRequest; or an *agy.ToolCall.
	Raw any
}

// Decision is the answer to a ToolRequest. Build one with Allow or Deny.
type Decision struct {
	// Allowed lets the call run.
	Allowed bool
	// Reason tells the agent why the call was denied. Codex and Antigravity
	// take no reason and ignore it.
	Reason string
}

// Allow returns a Decision that lets the call run.
func Allow() Decision { return Decision{Allowed: true} }

// Deny returns a Decision that refuses the call, telling the agent why.
func Deny(reason string) Decision { return Decision{Reason: reason} }

// Tool is a custom tool. NewTool builds one from a typed function.
type Tool struct {
	Name        string
	Description string
	// InputSchema is the JSON schema of the arguments, an object; nil
	// means an object without declared properties.
	InputSchema map[string]any
	// Run runs one call, given its arguments as a JSON object. The model
	// reads the returned text; an error fails the call, and the model reads
	// its message instead.
	Run func(ctx context.Context, args jsontext.Value) (string, error)
}

// NewTool builds a Tool from a typed function. Its input schema is
// SchemaFor[In](), and the call's arguments are decoded into In before fn
// runs; arguments that do not decode fail the call. A string result is the
// text the model reads, and any other result is sent as its JSON encoding.
func NewTool[In, Out any](name, description string, fn func(ctx context.Context, in In) (Out, error)) Tool {
	return Tool{
		Name:        name,
		Description: description,
		InputSchema: SchemaFor[In](),
		Run: func(ctx context.Context, args jsontext.Value) (string, error) {
			var in In
			if len(args) > 0 {
				if err := jsonx.Unmarshal(args, &in); err != nil {
					return "", fmt.Errorf("invalid arguments for tool %q: %w", name, err)
				}
			}
			out, err := fn(ctx, in)
			if err != nil {
				return "", err
			}
			if s, ok := any(out).(string); ok {
				return s, nil
			}
			b, err := jsonx.Marshal(out)
			if err != nil {
				return "", fmt.Errorf("encode the result of tool %q: %w", name, err)
			}
			return string(b), nil
		},
	}
}

// SchemaFor returns the JSON schema of T, for Config.OutputSchema. A struct
// becomes an object of its exported fields, named by their json tags, with
// `description` and `enum` tags carried over; a field is required unless it
// is a pointer or tagged omitempty or omitzero. A type with a JSONSchema()
// map[string]any method supplies its own schema.
func SchemaFor[T any]() map[string]any {
	return jsonschema.For(reflect.TypeFor[T]())
}

// Input is one part of a prompt, built by Text or Image.
type Input interface {
	isInput()
}

// TextInput is prompt text.
type TextInput struct {
	Text string
}

func (TextInput) isInput() {}

// Text returns a text Input.
func Text(text string) Input { return TextInput{Text: text} }

// ImageInput is an image.
type ImageInput struct {
	Data []byte
	// MIMEType is the image's type, such as "image/png".
	MIMEType string
}

func (ImageInput) isInput() {}

// Image returns an image Input. An empty mimeType is detected from data.
func Image(data []byte, mimeType string) Input {
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	return ImageInput{Data: data, MIMEType: mimeType}
}

// EventKind classifies an Event.
type EventKind string

const (
	// EventTextDelta carries a piece of the assistant's answer in Text: a
	// streamed delta, or a whole block from a provider that does not
	// stream.
	EventTextDelta EventKind = "textDelta"
	// EventThoughtDelta carries a piece of the model's reasoning in Text.
	EventThoughtDelta EventKind = "thoughtDelta"
	// EventToolCall reports in Tool a tool the agent is calling.
	EventToolCall EventKind = "toolCall"
	// EventToolResult reports in Tool the outcome of a tool call.
	EventToolResult EventKind = "toolResult"
	// EventOther is any other provider event; read it from Raw.
	EventOther EventKind = "other"
)

// Event is one event of a turn.
type Event struct {
	Kind EventKind
	// Text is set for EventTextDelta and EventThoughtDelta.
	Text string
	// Tool is set for EventToolCall and EventToolResult.
	Tool *ToolCall
	// Raw is the provider's own event: a claude.Message, a codex.Event or
	// an agy.Chunk.
	Raw any
}

// ToolCall describes a tool call. A call event sets ID, Name and Input; a
// result event sets ID, Output and IsError, and Name when the provider
// reports it.
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
	// Output is the tool's result as text.
	Output  string
	IsError bool
}

// Usage is the token usage of a turn. Fields a provider does not report are
// zero.
type Usage struct {
	// InputTokens counts every input token, cached ones included.
	InputTokens int64
	// CachedInputTokens is the part of InputTokens read from a cache.
	CachedInputTokens int64
	// OutputTokens counts the generated tokens, reasoning included.
	OutputTokens int64
}

// Result is a finished turn.
type Result struct {
	// Text is the assistant's final answer.
	Text string
	// StructuredOutput is the answer to Config.OutputSchema, or nil.
	StructuredOutput jsontext.Value
	Usage            Usage
	// CostUSD is the turn's cost, when the provider reports it.
	CostUSD *float64
	// Raw is the provider's own result: a *claude.ResultMessage, a
	// *codex.TurnResult or an *agy.TurnResult.
	Raw any
}

// DecodeStructuredOutput decodes the turn's structured output into v. It
// fails when the turn produced none.
func (r *Result) DecodeStructuredOutput(v any) error {
	if r == nil || len(r.StructuredOutput) == 0 {
		return errors.New("gelati: the turn produced no structured output")
	}
	return jsonx.Unmarshal(r.StructuredOutput, v)
}

// Provider opens sessions with one agent backend. claude.Provider,
// codex.Provider and agy.Provider return one; the interfaces below are what
// a provider implements, and programs use Open instead.
type Provider interface {
	// Name identifies the provider, such as "claude".
	Name() string
	// Open starts a session configured by cfg over the provider's native
	// options. ctx bounds the startup only. A setting the provider cannot
	// honor fails with an error matching errors.ErrUnsupported.
	Open(ctx context.Context, cfg Config) (Conn, error)
}

// Conn is a provider session, as an Agent drives it.
type Conn interface {
	// Send starts a turn with the given prompt parts.
	Send(ctx context.Context, input []Input) (TurnConn, error)
	// ID returns the provider's conversation id; it may be empty until the
	// provider reports one.
	ID() string
	// Native returns the provider's own session handle.
	Native() any
	Done() <-chan struct{}
	Err() error
	Close() error
}

// TurnConn is a provider turn, as a Turn reads it. Its methods follow the
// turn rules documented on Turn.
type TurnConn interface {
	Events(ctx context.Context) iter.Seq2[Event, error]
	Result(ctx context.Context) (*Result, error)
	Cancel(ctx context.Context) error
	Close() error
}
