package codex

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/ironpark/gelati/internal/buildinfo"
	"github.com/ironpark/gelati/internal/jsonx"
	"github.com/ironpark/gelati/internal/logx"
	"github.com/ironpark/gelati/internal/proc"
	"github.com/ironpark/gelati/internal/safecall"
	"github.com/ironpark/gelati/internal/tailbuf"
)

// DefaultBinary is the executable looked up on PATH, then in the usual
// install locations, when Options.CLIPath is empty.
const DefaultBinary = "codex"

// defaultEventBuffer bounds each turn's event channel.
const defaultEventBuffer = 64

// stderrTailLines is how much subprocess stderr Err reports after an exit.
const stderrTailLines = 100

// Default client identity, sent when Options.ClientInfo.Name is empty.
const (
	DefaultClientName  = "gelati_go_sdk"
	DefaultClientTitle = "Gelati Go SDK"
)

// Options configures a Client.
type Options struct {
	// CLIPath is the codex executable; it defaults to DefaultBinary on PATH.
	CLIPath string
	// Args replaces the default arguments, ConfigOverrides included
	// ("--config", k=v, ..., "app-server").
	Args []string
	// ConfigOverrides are "key=value" config.toml overrides, each passed as
	// --config before the subcommand; values parse as TOML.
	ConfigOverrides []string
	// Env holds extra environment variables for the app-server, merged over
	// this process's environment.
	Env map[string]string
	// Dir is the working directory of the subprocess.
	Dir string
	// Stderr receives the subprocess stderr; it defaults to os.Stderr. The
	// last lines are also kept for the error Err reports after an exit.
	Stderr io.Writer

	// ClientInfo identifies the integration during initialize. An empty Name
	// sends DefaultClientName, DefaultClientTitle, and this module's version.
	ClientInfo ClientInfo
	// Capabilities are the client capabilities sent at initialize. Nil opts
	// into the experimental API, as upstream does; several turn/start fields
	// (TurnTrigger, ServiceTierForTurn, ExternalMessage) need it.
	Capabilities *ClientCapabilities

	// Approvals answers server-initiated approval requests. When nil, command
	// and file-change approvals are declined, permission requests grant
	// nothing, and any other server request is answered with a
	// method-not-found error, so a turn fails closed instead of hanging.
	Approvals ApprovalHandler

	// OnNotification, when set, receives every notification the server sends,
	// including those routed to thread and turn subscribers.
	OnNotification func(method string, params jsontext.Value)

	// EventBuffer is the per-turn event channel capacity. It defaults to 64.
	EventBuffer int

	// Logger receives debug messages about dropped or unroutable events. Nil
	// passes warnings and errors to slog's default logger.
	Logger *slog.Logger
}

// Client controls a Codex app-server subprocess over the JSON-RPC app-server
// protocol. A Client is safe for concurrent use.
type Client struct {
	opts   Options
	logger *slog.Logger
	tr     *transport
	info   InitializeResult

	pending *pendingRequests

	accounts chan AccountUpdate

	mu          sync.Mutex
	initialized bool
	threads     map[string]*threadSubscription
	logins      map[string][]chan *LoginCompletedParams
	// loginResults holds completions that arrived with no waiter.
	loginResults map[string]*LoginCompletedParams
}

// New spawns `codex app-server`, performs the initialize/initialized
// handshake, and returns a ready client. The context bounds the handshake
// only; the client outlives it.
func New(ctx context.Context, opts Options) (*Client, error) {
	binary := opts.CLIPath
	if binary == "" {
		found, ok := proc.Find(DefaultBinary, proc.InstallCandidates(DefaultBinary)...)
		if !ok {
			return nil, ErrCLINotFound
		}
		binary = found
	}
	args := opts.Args
	if args == nil {
		for _, kv := range opts.ConfigOverrides {
			args = append(args, "--config", kv)
		}
		args = append(args, "app-server")
	}
	stderr := &tailbuf.Buffer{Tee: cmp.Or[io.Writer](opts.Stderr, os.Stderr), Max: stderrTailLines}
	p, err := startProcess(binary, args, opts.Env, opts.Dir, stderr)
	if err != nil {
		return nil, err
	}
	return dial(ctx, opts, p.stdout, p.stdin, p.close)
}

// dial wires a client onto an existing byte stream. Tests use it to drive an
// in-process fake app-server.
func dial(ctx context.Context, opts Options, in io.Reader, out io.Writer, release func() error) (*Client, error) {
	logger := logx.Or(opts.Logger)
	c := &Client{
		opts:    opts,
		logger:  logger,
		pending: newPendingRequests(),
		threads: make(map[string]*threadSubscription),
		logins:  make(map[string][]chan *LoginCompletedParams),

		loginResults: make(map[string]*LoginCompletedParams),
	}
	c.accounts = make(chan AccountUpdate, c.eventBuffer())
	c.tr = newTransport(transportConfig{
		in:              in,
		out:             out,
		release:         release,
		onNotification:  c.handleNotification,
		onServerRequest: c.handleServerRequest,
	})

	if err := c.handshake(ctx); err != nil {
		_ = c.tr.Close()
		return nil, err
	}
	go func() {
		<-c.tr.Done()
		c.pending.cancelAll()
		c.shutdown()
	}()
	return c, nil
}

// handshake sends initialize, waits for the result, then acknowledges with the
// initialized notification.
func (c *Client) handshake(ctx context.Context) error {
	params := InitializeParams{ClientInfo: c.opts.ClientInfo, Capabilities: c.opts.Capabilities}
	if params.ClientInfo.Name == "" {
		params.ClientInfo = ClientInfo{Name: DefaultClientName, Title: DefaultClientTitle}
	}
	if params.ClientInfo.Version == "" {
		params.ClientInfo.Version = buildinfo.Version()
	}
	if params.Capabilities == nil {
		params.Capabilities = &ClientCapabilities{ExperimentalApi: true}
	}
	var result InitializeResult
	if err := c.tr.Call(ctx, "initialize", params, &result); err != nil {
		return fmt.Errorf("codex: initialize: %w", err)
	}
	if err := c.tr.Notify("initialized", nil); err != nil {
		return fmt.Errorf("codex: initialized: %w", err)
	}

	c.mu.Lock()
	c.info = result
	c.initialized = true
	c.mu.Unlock()
	return nil
}

// Info returns the app-server details reported by initialize.
func (c *Client) Info() InitializeResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Done returns a channel closed when the client stops, either because Close
// was called or because the subprocess exited.
func (c *Client) Done() <-chan struct{} { return c.tr.Done() }

// Err returns the error that stopped the client, or nil while it runs.
func (c *Client) Err() error { return c.tr.Err() }

// Close terminates the subprocess and releases every waiting caller. It is
// safe to call more than once.
func (c *Client) Close() error { return c.tr.Close() }

// Call sends any app-server request and decodes its result into result,
// which may be nil. Use it for methods this package does not wrap. Errors
// from the server are *RPCError.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, method, params, result)
}

// call performs a JSON-RPC request, rejecting use before the handshake
// completes or after the client is closed.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	initialized := c.initialized
	c.mu.Unlock()
	if !initialized {
		return ErrNotInitialized
	}
	return c.tr.Call(ctx, method, params, result)
}

// notificationEnvelope carries the routing fields shared by app-server
// notifications. Every field is optional.
type notificationEnvelope struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Turn     *struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	} `json:"turn"`
	Thread *struct {
		ID string `json:"id"`
	} `json:"thread"`
}

// routeIDs extracts the thread and turn ids a notification applies to.
func routeIDs(params jsontext.Value) (threadID, turnID string) {
	if len(params) == 0 {
		return "", ""
	}
	var env notificationEnvelope
	if err := jsonx.Unmarshal(params, &env); err != nil {
		return "", ""
	}
	threadID, turnID = env.ThreadID, env.TurnID
	if env.Turn != nil {
		if turnID == "" {
			turnID = env.Turn.ID
		}
		if threadID == "" {
			threadID = env.Turn.ThreadID
		}
	}
	if threadID == "" && env.Thread != nil {
		threadID = env.Thread.ID
	}
	return threadID, turnID
}

// handleNotification runs on the transport reader goroutine. It must not
// block, so every fan-out path uses buffered channels or dedicated goroutines.
func (c *Client) handleNotification(method string, params jsontext.Value) {
	if c.opts.OnNotification != nil {
		if err := c.notifyUser(method, params); err != nil {
			c.logger.Error("codex: notification callback failed", "method", method, "error", err)
		}
	}
	switch method {
	case MethodLoginCompleted, MethodAccountUpdated:
		c.routeAccountNotification(method, params)
		return
	}
	// Thread and turn subscribers are registered by the thread and turn APIs
	// and dispatched from here.
	c.dispatchNotification(method, params)
}

// notifyUser calls Options.OnNotification, returning a panic there as an
// error rather than letting it kill the reader.
func (c *Client) notifyUser(method string, params jsontext.Value) (err error) {
	defer safecall.Recover(&err, "OnNotification")
	c.opts.OnNotification(method, params)
	return nil
}

// dispatchNotification routes a notification to its subscribers.
func (c *Client) dispatchNotification(method string, params jsontext.Value) {
	threadID, turnID := routeIDs(params)
	if c.routeTurnNotification(method, params, threadID, turnID) {
		return
	}
	c.routeThreadNotification(method, params, threadID)
}
