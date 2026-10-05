package agy

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/ironpark/gelati/internal/lifecycle"
)

// Agent is the high-level API: it starts a harness session from Options
// and chats with it.
//
//	agent, err := agy.NewAgent(agy.Options{})
//	if err != nil {
//		return err
//	}
//	if err := agent.Start(ctx); err != nil {
//		return err
//	}
//	defer agent.Close()
//	stream, err := agent.Chat(ctx, agy.Text("Hello"))
//	if err != nil {
//		return err
//	}
//	for delta, err := range stream.Text(ctx) {
//		...
//	}
//
// Start and Close stand in for Python's "async with Agent(...)". An Agent
// is safe for concurrent use; it runs one session at a time.
type Agent struct {
	opts Options

	mu       sync.Mutex
	starting bool
	conv     *Conversation
	triggers *TriggerRunner
	// last is the most recently started session's connection, kept after
	// Close for Done and Err.
	last *Connection
}

// NewAgent validates opts (see Options.Validate, including its safety
// policy guard) and returns an unstarted agent. The zero Options is valid.
// The agent keeps its own copy of opts; tools, hooks, policies and triggers
// keep their identity.
func NewAgent(opts Options) (*Agent, error) {
	opts = opts.clone()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return &Agent{opts: opts}, nil
}

// Start launches the harness and opens the session: it registers the hooks
// and custom tools, connects, and starts the triggers. ctx bounds the
// launch only. On failure everything started so far is torn down.
func (a *Agent) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.conv != nil || a.starting {
		a.mu.Unlock()
		return errors.New("agy: agent already started")
	}
	a.starting = true
	a.mu.Unlock()

	conv, triggers, err := a.start(ctx)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.starting = false
	if err != nil {
		return err
	}
	a.conv, a.triggers, a.last = conv, triggers, conv.conn
	return nil
}

func (a *Agent) start(ctx context.Context) (*Conversation, *TriggerRunner, error) {
	opts := &a.opts
	cc, err := opts.compile()
	if err != nil {
		return nil, nil, err
	}
	conn, err := connectLocal(ctx, cc)
	if err != nil {
		return nil, nil, err
	}
	conv := newConversation(conn, conn.InitialHistory())

	var triggers *TriggerRunner
	if len(opts.Triggers) > 0 {
		triggers = NewTriggerRunner(opts.Triggers, conn)
		triggers.logger = opts.logger()
		if err := triggers.Start(); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
	}
	cc.tools.setContext(newToolContext(conv))
	return conv, triggers, nil
}

// Close stops the triggers and ends the session (see Connection.Close). It
// is a no-op when the agent is not started; the agent can be started again
// afterwards.
func (a *Agent) Close() error {
	a.mu.Lock()
	conv, triggers := a.conv, a.triggers
	a.conv, a.triggers = nil, nil
	a.mu.Unlock()
	if conv == nil {
		return nil
	}
	if triggers != nil {
		triggers.Stop()
	}
	return conv.Close()
}

// Done returns a channel closed when the agent's session ends: Close was
// called, the harness process exited, or the harness connection was lost.
// It refers to the session current at the time of the call (the latest
// one, also after Close); call it again after a restart. Before the first
// Start it returns a closed channel.
//
// A session that ended on its own leaves the agent started: Chat fails,
// and Close must be called before Start can open a new session.
func (a *Agent) Done() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == nil {
		return lifecycle.Closed()
	}
	return a.last.Done()
}

// Err returns the error that ended the latest session (see
// Connection.Err): nil while it runs or when it ended because of Close,
// a *ConnectionError when the harness exited (wrapping a *ProcessError) or
// the connection was lost.
// Before the first Start it returns ErrNotStarted.
func (a *Agent) Err() error {
	a.mu.Lock()
	last := a.last
	a.mu.Unlock()
	if last == nil {
		return ErrNotStarted
	}
	return last.Err()
}

// Chat sends a prompt and returns the turn's stream; see
// Conversation.Chat. The prompt must not be empty: at least one part, and
// not only blank text.
func (a *Agent) Chat(ctx context.Context, content ...Content) (*TurnStream, error) {
	if len(content) == 0 {
		return nil, validationErrorf("Chat requires non-empty message content.")
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
			return nil, validationErrorf("Chat requires a non-empty message string.")
		}
		return nil, validationErrorf("Chat requires non-empty message content.")
	}
	conv := a.Conversation()
	if conv == nil {
		return nil, ErrNotStarted
	}
	return conv.Chat(ctx, content...)
}

// IsStarted reports whether the session is running.
func (a *Agent) IsStarted() bool { return a.Conversation() != nil }

// Conversation returns the session's conversation, for history, turn
// counts, usage and direct step access; nil when the agent is not started.
func (a *Agent) Conversation() *Conversation {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conv
}

// ConversationID returns the conversation identifier assigned by the
// runtime, available once a turn has started. Pass it as
// Options.ConversationID to resume the session later. It is "" before.
func (a *Agent) ConversationID() string {
	if conv := a.Conversation(); conv != nil {
		return conv.ConversationID()
	}
	return ""
}

// SandboxStatus returns the OS command sandbox status reported by the
// harness, or nil before Start or when the harness reported none. When
// RunCommandConfig.EnableSandbox is set but Available is false, commands
// run unsandboxed.
func (a *Agent) SandboxStatus() *SandboxStatus {
	if conv := a.Conversation(); conv != nil {
		return conv.SandboxStatus()
	}
	return nil
}
