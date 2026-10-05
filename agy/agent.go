package agy

import (
	"context"
	"strings"
)

// Agent is the high-level API: it runs a harness session started from
// Options and sends it prompts.
//
//	agent, err := agy.New(ctx, agy.Options{})
//	if err != nil {
//		return err
//	}
//	defer agent.Close()
//	stream, err := agent.Send(ctx, agy.Text("Hello"))
//	if err != nil {
//		return err
//	}
//	for delta, err := range stream.Text(ctx) {
//		...
//	}
//
// New and Close stand in for Python's "async with Agent(...)". An Agent is
// one session and is safe for concurrent use.
type Agent struct {
	conv     *Conversation
	triggers *TriggerRunner
}

// New validates opts (see Options.Validate, including its safety policy
// guard), launches the harness and opens the session: it registers the
// hooks and custom tools, connects, and starts the triggers. The zero
// Options is valid. The agent keeps its own copy of opts; tools, hooks,
// policies and triggers keep their identity.
//
// ctx bounds the launch only; the session outlives it and ends on Close or
// when the harness goes away. On failure everything started so far is torn
// down.
func New(ctx context.Context, opts Options) (*Agent, error) {
	opts = opts.clone()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	cc, err := opts.compile()
	if err != nil {
		return nil, err
	}
	conn, err := connectLocal(ctx, cc)
	if err != nil {
		return nil, err
	}
	conv := newConversation(conn, conn.InitialHistory())

	triggers := NewTriggerRunner(opts.Triggers, conn)
	triggers.logger = opts.logger()
	if err := triggers.Start(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	cc.tools.setContext(newToolContext(conv))
	return &Agent{conv: conv, triggers: triggers}, nil
}

// Run runs a single prompt in a fresh session: New, Agent.Run and Close.
// Unlike New's, ctx bounds the whole call: if it ends before the turn
// does, the turn is cancelled and Run returns ctx's error. A failed turn
// returns the partial result together with its error; a Close error is
// returned only when the turn succeeded.
func Run(ctx context.Context, prompt string, opts Options) (*TurnResult, error) {
	agent, err := New(ctx, opts)
	if err != nil {
		return nil, err
	}
	res, err := agent.Run(ctx, Text(prompt))
	if cerr := agent.Close(); err == nil && cerr != nil {
		return res, cerr
	}
	return res, err
}

// Close stops the triggers and ends the session (see Connection.Close).
// It is idempotent, so `defer agent.Close()` is safe.
func (a *Agent) Close() error {
	a.triggers.Stop()
	return a.conv.Close()
}

// Done returns a channel closed when the agent's session ends: Close was
// called, the harness process exited, or the harness connection was lost.
func (a *Agent) Done() <-chan struct{} {
	return a.conv.Done()
}

// Err returns the error that ended the session (see Connection.Err): nil
// while it runs or when it ended because of Close, a *ConnectionError when
// the harness exited (wrapping a *ProcessError) or the connection was lost.
func (a *Agent) Err() error {
	return a.conv.Err()
}

// Send sends a prompt and returns the turn's stream; see
// Conversation.Send. The prompt must not be empty: at least one part, and
// not only blank text.
func (a *Agent) Send(ctx context.Context, content ...Content) (*TurnStream, error) {
	if len(content) == 0 {
		return nil, validationErrorf("Send requires non-empty message content.")
	}
	blank := true
	for _, c := range content {
		t, ok := c.(Text)
		if !ok || strings.TrimSpace(string(t)) != "" {
			blank = false
			break
		}
	}
	if blank {
		if len(content) == 1 {
			return nil, validationErrorf("Send requires a non-empty message string.")
		}
		return nil, validationErrorf("Send requires non-empty message content.")
	}
	return a.conv.Send(ctx, content...)
}

// Run sends a prompt and waits for the turn's result: Send followed by
// TurnStream.Result. A failed turn returns the partial result together
// with its error. If ctx ends before the turn does, Run cancels the turn,
// closes its stream and returns ctx's error.
func (a *Agent) Run(ctx context.Context, content ...Content) (*TurnResult, error) {
	stream, err := a.Send(ctx, content...)
	if err != nil {
		return nil, err
	}
	return collect(ctx, stream)
}

// Conversation returns the session's conversation, for history, turn
// counts, usage and direct step access.
func (a *Agent) Conversation() *Conversation { return a.conv }

// ConversationID returns the conversation identifier assigned by the
// runtime, available once a turn has started. Pass it as
// Options.ConversationID to resume the session later. It is "" before.
func (a *Agent) ConversationID() string { return a.conv.ConversationID() }

// SandboxStatus returns the OS command sandbox status reported by the
// harness, or nil when the harness reported none. When
// RunCommandConfig.EnableSandbox is set but Available is false, commands
// run unsandboxed.
func (a *Agent) SandboxStatus() *SandboxStatus { return a.conv.SandboxStatus() }
