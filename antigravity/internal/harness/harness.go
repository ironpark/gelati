// Package harness launches the Antigravity localharness binary and carries
// wire events to and from it.
//
// The protocol, mirrored from the upstream Python SDK (v0.1.20,
// connections/local/local_connection.py):
//
//  1. Spawn the binary with stdin, stdout and stderr piped. When extra
//     environment variables are configured they are added to the inherited
//     environment.
//  2. Write a little-endian uint32 length followed by a binary-protobuf
//     [wire.InputConfig] (storage directory, client info, env) to stdin.
//  3. Read a little-endian uint32 length and a binary-protobuf
//     [wire.OutputConfig] from stdout: the WebSocket port and an API key.
//  4. Keep stdin open. It is the harness's lifeline: closing it tells the
//     harness to shut down.
//  5. Connect to ws://localhost:<port>/ (falling back to 127.0.0.1), sending
//     the key in the x-goog-api-key header. The harness may still be starting
//     to listen, so connecting is retried with backoff.
//  6. Send an [wire.InitializeConversationEvent] as a protobuf-JSON text
//     message; the first message back is an [wire.OutputEvent] carrying the
//     [wire.InitializeConversationResponse].
//  7. Exchange [wire.InputEvent] (sent) and [wire.OutputEvent] (received)
//     messages until shutdown.
//
// [Start] performs steps 1 to 5, [Harness.Initialize] step 6, and
// [Harness.Send] and [Harness.Receive] step 7. [Harness.Close] shuts down the
// way upstream does: close the WebSocket, close stdin, wait for the process
// to exit, then terminate and finally kill it.
package harness

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/ironpark/gelati/antigravity/internal/wire"
	"github.com/ironpark/gelati/internal/tailbuf"
)

// APIKeyHeader is the request header carrying the key the harness reports in
// its OutputConfig.
const APIKeyHeader = "x-goog-api-key"

// Defaults mirrored from upstream.
const (
	// DefaultShutdownTimeout is how long Close waits for the harness to exit
	// after closing stdin, before terminating it. Upstream allows this much
	// because the harness may need time to flush state.
	DefaultShutdownTimeout = 3 * time.Minute
	// DefaultMaxMessageSize bounds one received WebSocket message. Upstream
	// sets no limit; this is a generous guard.
	DefaultMaxMessageSize = 256 << 20

	connectAttempts     = 5
	connectBaseBackoff  = 100 * time.Millisecond
	wsCloseTimeout      = 500 * time.Millisecond
	terminateGrace      = time.Second
	stderrTailLines     = 100
	maxOutputConfigSize = 1 << 20
	msgBuffer           = 16
)

// ErrClosed is returned by operations on a Harness after Close.
var ErrClosed = errors.New("antigravity harness: closed")

// Options configures [Start].
type Options struct {
	// CLIPath is the localharness executable. When empty, [FindBinary]
	// locates it.
	CLIPath string
	// Env holds extra environment variables. They are added to the inherited
	// environment of the process and also sent in InputConfig.env, as
	// upstream does. Nil inherits the environment unchanged.
	Env map[string]string
	// StorageDirectory is InputConfig.storage_directory, where the harness
	// saves trajectories (upstream's save_dir). Empty lets the harness choose.
	StorageDirectory string
	// ClientInfo identifies the SDK to the harness. Nil uses
	// [DefaultClientInfo].
	ClientInfo *wire.ClientInfo
	// Dir is the working directory of the process. Empty uses the current
	// directory.
	Dir string
	// Stderr, when set, receives a copy of the harness's stderr output. The
	// last lines are retained regardless; see [Harness.StderrTail].
	Stderr io.Writer
	// MaxMessageSize bounds one received message. Zero means
	// DefaultMaxMessageSize; negative means no limit.
	MaxMessageSize int64
	// ShutdownTimeout bounds how long Close waits for a clean exit. Zero
	// means DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
}

// Harness is a running localharness process with its WebSocket connection.
// Send may be called concurrently; Receive must be called from one goroutine
// at a time.
type Harness struct {
	opts    Options
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *os.File
	stderr  *tailbuf.Buffer
	ws      *websocket.Conn
	msgs    chan []byte   // messages from readLoop; closed when it stops
	readErr error         // why readLoop stopped; valid once msgs is closed
	stop    chan struct{} // closed by Close to release readLoop
	url     string
	port    int
	exited  chan struct{}
	waitErr error

	closing atomic.Bool
	closeFn func() error // shutdown, run once
}

// Start launches the harness, performs the stdin/stdout handshake and opens
// the WebSocket connection. ctx bounds the launch only, not the lifetime of
// the returned Harness. On failure the process is killed and the error
// includes the harness's stderr output.
func Start(ctx context.Context, opts Options) (*Harness, error) {
	bin := opts.CLIPath
	if bin == "" {
		var err error
		if bin, err = FindBinary(opts.Env); err != nil {
			return nil, err
		}
	}
	if opts.MaxMessageSize == 0 {
		opts.MaxMessageSize = DefaultMaxMessageSize
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = DefaultShutdownTimeout
	}
	clientInfo := opts.ClientInfo
	if clientInfo == nil {
		clientInfo = DefaultClientInfo()
	}
	input := &wire.InputConfig{
		// Upstream always sets storage_directory, to "" when unconfigured.
		StorageDirectory: new(opts.StorageDirectory),
		ClientInfo:       clientInfo,
		Env:              opts.Env,
	}
	payload, err := input.MarshalBinary()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(bin)
	cmd.Dir = opts.Dir
	if opts.Env != nil {
		cmd.Env = os.Environ()
		for k, v := range opts.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	stderr := &tailbuf.Buffer{Tee: opts.Stderr, Max: stderrTailLines}
	cmd.Stderr = stderr
	// Children of the harness may inherit stderr; don't let them hold Wait.
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// stdout is a plain pipe so that reading it is independent of Wait.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = stdoutW
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("antigravity harness: start %s: %w", bin, err)
	}
	stdoutW.Close()

	h := &Harness{
		opts:   opts,
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdoutR,
		stderr: stderr,
		exited: make(chan struct{}),
	}
	h.closeFn = sync.OnceValue(h.shutdown)
	go func() {
		h.waitErr = cmd.Wait()
		close(h.exited)
	}()

	out, err := h.handshake(ctx, payload)
	if err != nil {
		return nil, h.abort(fmt.Errorf("antigravity harness: handshake: %w", err))
	}
	// Nothing more is expected on stdout; drain it so the harness never
	// blocks writing there.
	go func() { _, _ = io.Copy(io.Discard, h.stdout) }()

	h.port = int(out.GetPort())
	if h.port <= 0 || h.port > 65535 {
		return nil, h.abort(fmt.Errorf("antigravity harness: invalid port %d in output config", out.GetPort()))
	}
	if err := h.connect(ctx, out.GetAPIKey()); err != nil {
		return nil, h.abort(err)
	}
	return h, nil
}

// handshake writes the framed InputConfig to stdin and reads the framed
// OutputConfig from stdout.
func (h *Harness) handshake(ctx context.Context, payload []byte) (*wire.OutputConfig, error) {
	type result struct {
		out *wire.OutputConfig
		err error
	}
	done := make(chan result, 1)
	go func() {
		frame := binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))
		if _, err := h.stdin.Write(append(frame, payload...)); err != nil {
			done <- result{err: fmt.Errorf("write input config: %w", err)}
			return
		}
		var lenBuf [4]byte
		if _, err := io.ReadFull(h.stdout, lenBuf[:]); err != nil {
			done <- result{err: fmt.Errorf("read output config length: %w", err)}
			return
		}
		n := binary.LittleEndian.Uint32(lenBuf[:])
		if n > maxOutputConfigSize {
			done <- result{err: fmt.Errorf("output config length %d too large", n)}
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(h.stdout, buf); err != nil {
			done <- result{err: fmt.Errorf("read output config: %w", err)}
			return
		}
		var out wire.OutputConfig
		if err := out.UnmarshalBinary(buf); err != nil {
			done <- result{err: err}
			return
		}
		done <- result{out: &out}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// connect dials the harness WebSocket, retrying with exponential backoff
// across localhost and 127.0.0.1 like upstream.
func (h *Harness) connect(ctx context.Context, apiKey string) error {
	header := http.Header{}
	header.Set(APIKeyHeader, apiKey)
	opts := &websocket.DialOptions{HTTPHeader: header}
	var lastErr error
	var lastURL string
	for attempt := range connectAttempts {
		for _, host := range []string{"localhost", "127.0.0.1"} {
			lastURL = "ws://" + net.JoinHostPort(host, strconv.Itoa(h.port)) + "/"
			ws, _, err := websocket.Dial(ctx, lastURL, opts)
			if err == nil {
				limit := h.opts.MaxMessageSize
				if limit < 0 {
					limit = -1
				}
				ws.SetReadLimit(limit)
				h.ws, h.url = ws, lastURL
				// A small buffer lets readLoop read ahead of Receive.
				h.msgs, h.stop = make(chan []byte, msgBuffer), make(chan struct{})
				go h.readLoop()
				return nil
			}
			lastErr = err
			if ctx.Err() != nil {
				return fmt.Errorf("antigravity harness: connect to %s: %w", lastURL, ctx.Err())
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("antigravity harness: connect to %s: %w", lastURL, ctx.Err())
		case <-h.exited:
			return fmt.Errorf("antigravity harness: process exited before accepting the WebSocket connection: %w", lastErr)
		case <-time.After(connectBaseBackoff << attempt):
		}
	}
	return fmt.Errorf("antigravity harness: failed to connect to WebSocket at %s after %d attempts: %w",
		lastURL, connectAttempts, lastErr)
}

// abort kills the process after a failed launch and returns err annotated
// with the harness's stderr.
func (h *Harness) abort(err error) error {
	h.closing.Store(true)
	if h.ws != nil {
		_ = h.ws.CloseNow()
	}
	_ = h.cmd.Process.Kill()
	_ = h.stdin.Close()
	<-h.exited
	_ = h.stdout.Close()
	return &StartError{Err: err, Stderr: h.stderr.String()}
}

// Initialize sends the InitializeConversationEvent built from cfg and returns
// the harness's InitializeConversationResponse, which it sends as its first
// message. On failure the process is killed, as upstream does, and the
// Harness must not be used further.
func (h *Harness) Initialize(ctx context.Context, cfg *wire.HarnessConfig) (*wire.InitializeConversationResponse, error) {
	resp, err := h.initialize(ctx, cfg)
	if err != nil {
		return nil, h.abort(fmt.Errorf("antigravity harness: initialize conversation at %s: %w", h.url, err))
	}
	return resp, nil
}

func (h *Harness) initialize(ctx context.Context, cfg *wire.HarnessConfig) (*wire.InitializeConversationResponse, error) {
	if err := h.send(ctx, &wire.InitializeConversationEvent{Config: cfg}); err != nil {
		return nil, err
	}
	ev, err := h.Receive(ctx)
	if err != nil {
		return nil, err
	}
	resp := ev.GetInitializeConversationResponse()
	if resp == nil {
		b, _ := wire.Marshal(ev)
		return nil, fmt.Errorf("first event is not an initialize response: %s", truncate(b, 512))
	}
	return resp, nil
}

// Send writes an InputEvent to the harness.
func (h *Harness) Send(ctx context.Context, ev *wire.InputEvent) error {
	return h.send(ctx, ev)
}

func (h *Harness) send(ctx context.Context, msg any) error {
	if h.closing.Load() {
		return ErrClosed
	}
	b, err := wire.Marshal(msg)
	if err != nil {
		return fmt.Errorf("antigravity harness: encode %T: %w", msg, err)
	}
	if err := h.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return h.connErr(err)
	}
	return nil
}

// Receive returns the next OutputEvent. After Close it returns ErrClosed. If
// the connection drops while the Harness is open, it returns a
// *ConnectionError carrying the close code and the harness's stderr.
func (h *Harness) Receive(ctx context.Context) (*wire.OutputEvent, error) {
	if h.closing.Load() {
		return nil, ErrClosed
	}
	var data []byte
	select {
	case d, ok := <-h.msgs:
		if !ok {
			return nil, h.connErr(h.readErr)
		}
		data = d
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var ev wire.OutputEvent
	if err := wire.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("antigravity harness: decode output event: %w: %s", err, truncate(data, 512))
	}
	return &ev, nil
}

// readLoop reads messages for the lifetime of the connection. Reading with a
// context that is never canceled matters: coder/websocket closes the
// connection when a Read's context is canceled, and Receive must stay
// cancelable without tearing the session down.
func (h *Harness) readLoop() {
	defer close(h.msgs)
	for {
		_, data, err := h.ws.Read(context.Background())
		if err != nil {
			h.readErr = err
			return
		}
		select {
		case h.msgs <- data:
		case <-h.stop:
			return
		}
	}
}

// connErr maps a WebSocket error to the error Send and Receive report.
func (h *Harness) connErr(err error) error {
	if h.closing.Load() {
		return ErrClosed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// The harness likely crashed: give it a moment to exit so its stderr is
	// complete.
	waitClosed(h.exited, time.Second)
	code := websocket.CloseStatus(err)
	if code < 0 {
		code = websocket.StatusAbnormalClosure
	}
	return &ConnectionError{Code: code, Stderr: h.stderr.String(), Err: err}
}

// Close shuts the harness down: it closes the WebSocket, closes stdin to
// tell the harness to exit, and waits up to Options.ShutdownTimeout before
// terminating and then killing the process. It is safe to call more than
// once.
func (h *Harness) Close() error { return h.closeFn() }

func (h *Harness) shutdown() error {
	h.closing.Store(true)
	close(h.stop)
	closed := make(chan struct{})
	go func() {
		_ = h.ws.Close(websocket.StatusNormalClosure, "")
		close(closed)
	}()
	if !waitClosed(closed, wsCloseTimeout) {
		_ = h.ws.CloseNow()
		<-closed
	}
	_ = h.stdin.Close()
	err := h.waitOrKill()
	_ = h.stdout.Close()
	return err
}

func (h *Harness) waitOrKill() error {
	if waitClosed(h.exited, h.opts.ShutdownTimeout) {
		return nil
	}
	_ = terminate(h.cmd.Process)
	if waitClosed(h.exited, terminateGrace) {
		return nil
	}
	if err := h.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("antigravity harness: kill: %w", err)
	}
	if !waitClosed(h.exited, terminateGrace) {
		return errors.New("antigravity harness: process did not exit after kill")
	}
	return nil
}

// waitClosed waits up to d for ch to be closed and reports whether it was.
func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

// Port returns the harness's WebSocket port.
func (h *Harness) Port() int { return h.port }

// PID returns the harness process id.
func (h *Harness) PID() int { return h.cmd.Process.Pid }

// Exited returns a channel closed when the harness process has exited.
func (h *Harness) Exited() <-chan struct{} { return h.exited }

// ExitErr returns the process's exit error (nil for a clean exit) once
// Exited is closed, and nil before.
func (h *Harness) ExitErr() error {
	select {
	case <-h.exited:
		return h.waitErr
	default:
		return nil
	}
}

// StderrTail returns the last lines the harness wrote to stderr.
func (h *Harness) StderrTail() string { return h.stderr.String() }

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
