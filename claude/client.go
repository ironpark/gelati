package claude

import (
	"context"
	"iter"
	"sync"

	"github.com/ironpark/gelati/internal/lifecycle"
)

// DefaultSessionID is the session a client's turns are attributed to when the
// caller does not name one.
const DefaultSessionID = "default"

// ServerInfo is the CLI's response to the initialize handshake: supported
// commands, output styles and other capability metadata, passed through as-is.
type ServerInfo map[string]any

// Client runs a bidirectional, stateful conversation with the Claude Code CLI:
// turns can be sent at any time, responses consumed as they arrive, and the run
// steered with interrupts and setter calls. Use Query for one-shot prompts.
//
// The lifecycle is explicit rather than scoped:
//
//	client := claude.NewClient(opts)
//	if err := client.Connect(ctx); err != nil {
//		return err
//	}
//	defer client.Disconnect()
//
// A connected client is safe for concurrent use: control calls may run while
// another goroutine ranges over ReceiveMessages. The message stream itself has
// a single consumer — ReceiveMessages and ReceiveResponse read from the same
// underlying sequence, so ranging over both at once splits the messages
// between them.
type Client struct {
	opts *Options

	// deps replaces process-level dependencies in tests; nil means the
	// real ones.
	deps *sessionDeps

	mu sync.Mutex
	// connecting is set while Connect opens a session.
	connecting bool
	// sess is the live session, nil while disconnected.
	sess *session
	// last is the engine of the current or most recent session, kept after
	// Disconnect for Done and Err.
	last *engine
}

// NewClient builds an unconnected client. opts may be nil.
func NewClient(opts *Options) *Client {
	return &Client{opts: opts}
}

// Connect starts the CLI session and performs the initialize handshake. Any
// initial turns are sent right after it. Calling Connect on an already
// connected client is an error.
//
// When ctx has no deadline, DefaultInitializeTimeout bounds the handshake.
// ctx governs the whole session: cancelling it after Connect returns
// terminates the CLI.
func (c *Client) Connect(ctx context.Context, initial ...UserInput) error {
	c.mu.Lock()
	if c.sess != nil || c.connecting {
		c.mu.Unlock()
		return NewConnectionError("already connected")
	}
	// Reserved for the whole handshake, so a concurrent Connect cannot open
	// a second session and leak one of them.
	c.connecting = true
	c.mu.Unlock()

	sess, err := startSession(ctx, c.opts, entrypointClient, c.deps)

	c.mu.Lock()
	c.connecting = false
	c.sess = sess
	if err == nil {
		c.last = sess.eng
	}
	c.mu.Unlock()
	if err != nil {
		return err
	}

	for _, input := range initial {
		if err := c.send(ctx, input, DefaultSessionID); err != nil {
			_ = c.Disconnect()
			return err
		}
	}
	return nil
}

// engineOrErr returns the live engine, or an error when not connected.
func (c *Client) engineOrErr() (*engine, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil {
		return nil, NewConnectionError("Not connected. Call Connect first.")
	}
	return c.sess.eng, nil
}

// send writes one user turn, attributed to sessionID unless it names its own
// session.
func (c *Client) send(ctx context.Context, input UserInput, sessionID string) error {
	eng, err := c.engineOrErr()
	if err != nil {
		return err
	}
	if input.SessionID == "" {
		input.SessionID = sessionID
	}
	return eng.sendUserMessage(ctx, input)
}

// Query sends one prompt as a new turn. An empty sessionID uses
// DefaultSessionID.
func (c *Client) Query(ctx context.Context, prompt, sessionID string) error {
	if sessionID == "" {
		sessionID = DefaultSessionID
	}
	return c.send(ctx, UserInput{Content: prompt}, sessionID)
}

// QueryStream sends several turns in order. An empty sessionID uses
// DefaultSessionID; inputs that name their own session keep it.
func (c *Client) QueryStream(ctx context.Context, inputs iter.Seq[UserInput], sessionID string) error {
	if sessionID == "" {
		sessionID = DefaultSessionID
	}
	for input := range inputs {
		if err := c.send(ctx, input, sessionID); err != nil {
			return err
		}
	}
	return nil
}

// ReceiveMessages yields every message the session produces until it ends. A
// fatal error is the final item.
func (c *Client) ReceiveMessages(ctx context.Context) iter.Seq2[Message, error] {
	// The session is looked up when the sequence is ranged over.
	return func(yield func(Message, error) bool) {
		eng, err := c.engineOrErr()
		if err != nil {
			yield(nil, err)
			return
		}
		eng.receive(ctx)(yield)
	}
}

// ReceiveResponse yields messages up to and including the next ResultMessage,
// then stops. Messages of later turns stay queued for the next call.
func (c *Client) ReceiveResponse(ctx context.Context) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		for msg, err := range c.ReceiveMessages(ctx) {
			if !yield(msg, err) || err != nil {
				return
			}
			if _, ok := msg.(*ResultMessage); ok {
				return
			}
		}
	}
}

// Run sends prompt as a new turn and waits for its ResultMessage, whose
// Result field is the final response text. An empty sessionID uses
// DefaultSessionID. Messages before the result are dropped; use Query and
// ReceiveResponse to observe them. Errors are as for the package-level Run.
func (c *Client) Run(ctx context.Context, prompt, sessionID string) (*ResultMessage, error) {
	if err := c.Query(ctx, prompt, sessionID); err != nil {
		return nil, err
	}
	return collectResult(c.ReceiveResponse(ctx))
}

// ServerInfo reports the raw initialize response: available commands, output
// styles and other capabilities. It is nil before Connect. InitializationResult
// is the typed form.
func (c *Client) ServerInfo() ServerInfo {
	if r := c.InitializationResult(); r != nil {
		return ServerInfo(r.Raw)
	}
	return nil
}

// Done returns a channel that is closed when the session ends for any
// reason: Disconnect, cancellation of the ctx given to Connect, the CLI
// exiting, or a transport failure. It refers to the session current at the
// time of the call (the latest one, also after Disconnect); call it again
// after reconnecting. Before the first Connect it returns a closed channel.
func (c *Client) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		return lifecycle.Closed()
	}
	return c.last.end.C()
}

// Err reports why the latest session ended: nil while it runs and after a
// Disconnect that ended it; otherwise the cancellation cause of Connect's
// ctx, the fatal error that ended the CLI's output (a *ProcessError or
// *ResultError for a failed exit, say), or a *ConnectionError when the
// output ended cleanly. Before the first Connect it returns a
// *ConnectionError.
func (c *Client) Err() error {
	c.mu.Lock()
	last := c.last
	c.mu.Unlock()
	if last == nil {
		return NewConnectionError("Not connected. Call Connect first.")
	}
	return last.end.Err()
}

// Disconnect ends the session and releases its resources, including the
// temporary config directory of a SessionStore resume. It is idempotent, so
// `defer client.Disconnect()` is safe even on paths that already disconnected.
func (c *Client) Disconnect() error {
	c.mu.Lock()
	sess := c.sess
	c.sess = nil
	c.mu.Unlock()
	if sess == nil {
		return nil
	}
	return sess.close()
}
