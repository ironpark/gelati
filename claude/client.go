package claude

import (
	"context"
	"errors"
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
// steered with interrupts and setter calls. Use Query or Run for one-shot
// prompts.
//
// The lifecycle is explicit rather than scoped:
//
//	client := claude.NewClient(opts)
//	if err := client.Connect(ctx); err != nil {
//		return err
//	}
//	defer client.Disconnect()
//	turn, err := client.Send(ctx, claude.Text("Hello"))
//	...
//
// A connected client is safe for concurrent use: control calls may run while
// another goroutine reads a turn. The CLI's output is a single message stream,
// though, so it has a single consumer: read one TurnStream at a time, and do
// not mix TurnStreams with ReceiveMessages, which reads the same stream raw.
type Client struct {
	opts Options

	// deps replaces process-level dependencies in tests; nil means the
	// real ones.
	deps *sessionDeps

	// sendMu serializes Send.
	sendMu sync.Mutex

	mu sync.Mutex
	// connecting is set while Connect opens a session.
	connecting bool
	// sess is the live session, nil while disconnected.
	sess *session
	// last is the engine of the current or most recent session, kept after
	// Disconnect for Done and Err.
	last *engine
	// turns are the live session's turns that have not ended, oldest
	// first. Reading a turn reads the stream from the head of the queue.
	turns []*TurnStream
}

// NewClient builds an unconnected client. The zero Options means defaults.
func NewClient(opts Options) *Client {
	return &Client{opts: opts}
}

// Connect starts the CLI session and performs the initialize handshake.
// Calling Connect on an already connected client is an error; after
// Disconnect it starts a new session.
//
// When ctx has no deadline, DefaultInitializeTimeout bounds the handshake.
// ctx governs the whole session: cancelling it after Connect returns
// terminates the CLI.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.sess != nil || c.connecting {
		c.mu.Unlock()
		return newConnectionError("already connected")
	}
	// Reserved for the whole handshake, so a concurrent Connect cannot open
	// a second session and leak one of them.
	c.connecting = true
	c.mu.Unlock()

	sess, err := startSession(ctx, &c.opts, entrypointClient, c.deps)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.connecting = false
	if err != nil {
		return err
	}
	c.sess = sess
	c.last = sess.eng
	c.turns = nil
	return nil
}

// engineOrErr returns the live engine, or an error matching ErrNotConnected
// before the first Connect and ErrClosed after Disconnect.
func (c *Client) engineOrErr() (*engine, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.sess != nil:
		return c.sess.eng, nil
	case c.last != nil:
		return nil, closedError("client")
	default:
		return nil, notConnectedError()
	}
}

// Text returns a UserInput carrying prompt as plain text, the common case of
// a turn's input.
func Text(prompt string) UserInput {
	return UserInput{Content: prompt}
}

// Send writes input as a new turn, in order, and returns the stream of the
// turn's messages. Inputs that name no session are attributed to
// DefaultSessionID. Sending nothing is an error.
//
// A turn is the messages up to and including the next ResultMessage after
// those of the turns sent before it. Several inputs in one Send are meant to
// be answered together: should the CLI answer them with a result each, the
// later results belong to the following turns. For a one-to-one mapping send
// a turn once the previous one's result has arrived, as Run does.
func (c *Client) Send(ctx context.Context, input ...UserInput) (*TurnStream, error) {
	if len(input) == 0 {
		return nil, errors.New("claude: Send needs at least one input")
	}
	// Sends are serialized so that turns are queued in the order the CLI
	// sees them.
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	eng, err := c.engineOrErr()
	if err != nil {
		return nil, err
	}
	for _, in := range input {
		if in.SessionID == "" {
			in.SessionID = DefaultSessionID
		}
		if err := eng.sendUserMessage(ctx, in); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil || c.sess.eng != eng {
		// Disconnected while writing.
		return nil, closedError("client")
	}
	t := &TurnStream{client: c, eng: eng}
	c.turns = append(c.turns, t)
	return t, nil
}

// Run sends input as a new turn and waits for its ResultMessage, whose Text
// method returns the final response text: Send followed by
// TurnStream.Result. Messages before the result are dropped; use Send and
// TurnStream.Events to observe them. Errors are as for the package-level Run.
func (c *Client) Run(ctx context.Context, input ...UserInput) (*ResultMessage, error) {
	t, err := c.Send(ctx, input...)
	if err != nil {
		return nil, err
	}
	defer t.Close()
	return t.Result(ctx)
}

// ReceiveMessages yields every message the session produces until it ends, as
// it comes, regardless of turns. A fatal error is the final item. It reads
// the same stream as TurnStream; use one or the other.
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
		return notConnectedError()
	}
	return last.end.Err()
}

// Disconnect ends the session and releases its resources, including the
// temporary config directory of a SessionStore resume. Calls that need the
// session then fail with an error matching ErrClosed, until the next Connect.
// It is idempotent, so `defer client.Disconnect()` is safe even on paths that
// already disconnected.
func (c *Client) Disconnect() error {
	c.mu.Lock()
	sess := c.sess
	c.sess = nil
	c.turns = nil
	c.mu.Unlock()
	if sess == nil {
		return nil
	}
	return sess.close()
}
