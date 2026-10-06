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
// New starts the session and Close ends it:
//
//	client, err := claude.New(ctx, opts)
//	if err != nil {
//		return err
//	}
//	defer client.Close()
//	turn, err := client.Send(ctx, claude.Text("Hello"))
//	...
//
// A Client is safe for concurrent use: control calls may run while another
// goroutine reads a turn. The CLI's output is a single message stream,
// though, so it has a single consumer: read one TurnStream at a time, and do
// not mix TurnStreams with ReceiveMessages, which reads the same stream raw.
type Client struct {
	sess *session
	// startID is the session id the options name, for SessionID before a
	// message reports one.
	startID string

	// sendMu serializes Send.
	sendMu sync.Mutex

	mu     sync.Mutex
	closed bool
	// turns are the turns that have not ended, oldest first. Reading a
	// turn reads the stream from the head of the queue.
	turns []*TurnStream
}

// New starts a CLI session, performs the initialize handshake and returns a
// ready client. The zero Options means defaults. On failure nothing is left
// running.
//
// ctx bounds the startup only; the session outlives it and ends on Close or
// when the CLI exits. When ctx has no deadline, DefaultInitializeTimeout
// bounds the handshake.
func New(ctx context.Context, opts Options) (*Client, error) {
	return newClient(ctx, &opts, nil)
}

// newClient is New with replaceable session dependencies.
func newClient(ctx context.Context, opts *Options, deps *sessionDeps) (*Client, error) {
	sess, err := startSession(ctx, opts, entrypointClient, deps)
	if err != nil {
		return nil, err
	}
	c := &Client{sess: sess, startID: opts.SessionID}
	if c.startID == "" && !opts.ForkSession {
		c.startID = opts.Resume
	}
	return c, nil
}

// checkOpen returns an error matching ErrClosed after Close.
func (c *Client) checkOpen() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return closedError("client")
	}
	return nil
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
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	for _, in := range input {
		if in.SessionID == "" {
			in.SessionID = DefaultSessionID
		}
		if err := c.sess.eng.sendUserMessage(ctx, in); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		// Closed while writing.
		return nil, closedError("client")
	}
	t := &TurnStream{client: c}
	c.turns = append(c.turns, t)
	return t, nil
}

// Run sends input as a new turn and waits for its ResultMessage, whose Text
// method returns the final response text: Send followed by
// TurnStream.Result. Messages before the result are dropped; use Send and
// TurnStream.Events or TurnStream.Text to observe them. Errors are as for
// the package-level Run.
//
// When ctx ends before the result arrives, Run interrupts the turn, as
// TurnStream.Cancel does, and returns ctx's error; the session stays open.
func (c *Client) Run(ctx context.Context, input ...UserInput) (*ResultMessage, error) {
	t, err := c.Send(ctx, input...)
	if err != nil {
		return nil, err
	}
	defer t.Close()
	result, err := t.Result(ctx)
	lifecycle.CancelIfDone(ctx, err, t.Cancel)
	return result, err
}

// ReceiveMessages yields every message the session produces until it ends, as
// it comes, regardless of turns. A fatal error is the final item. It reads
// the same stream as TurnStream; use one or the other.
func (c *Client) ReceiveMessages(ctx context.Context) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		if err := c.checkOpen(); err != nil {
			yield(nil, err)
			return
		}
		c.sess.eng.receive(ctx)(yield)
	}
}

// ServerInfo reports the raw initialize response: available commands, output
// styles and other capabilities. InitializationResult is the typed form.
func (c *Client) ServerInfo() ServerInfo {
	return ServerInfo(c.InitializationResult().Raw)
}

// Done returns a channel that is closed when the session ends for any
// reason: Close, the CLI exiting, or a transport failure.
func (c *Client) Done() <-chan struct{} {
	return c.sess.eng.end.C()
}

// Err reports why the session ended: nil while it runs and after a Close
// that ended it; otherwise the fatal error that ended the CLI's output (a
// *ProcessError or *ResultError for a failed exit, say), or a
// *ConnectionError when the output ended cleanly.
func (c *Client) Err() error {
	return c.sess.eng.end.Err()
}

// SessionID returns the session's id: the latest one the CLI's messages
// carried, or before any did, the one the options name (Options.SessionID,
// or Options.Resume unless ForkSession). It is empty until then.
func (c *Client) SessionID() string {
	if id := c.sess.eng.sessionID.Load(); id != nil {
		return *id
	}
	return c.startID
}

// Close ends the session and releases its resources, including the
// temporary config directory of a SessionStore resume. Calls that need the
// session then fail with an error matching ErrClosed. It is idempotent, so
// `defer client.Close()` is safe even on paths that already closed.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.turns = nil
	c.mu.Unlock()
	return c.sess.close()
}
