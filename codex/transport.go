package codex

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ironpark/gelati/internal/jsonx"
	"github.com/ironpark/gelati/internal/lifecycle"
	"github.com/ironpark/gelati/internal/proc"
	"github.com/ironpark/gelati/internal/safecall"
	"github.com/ironpark/gelati/internal/tailbuf"
)

// maxLineBytes bounds a single JSONL message read from the app-server.
// Aggregated command output can be large, so the limit is generous.
const maxLineBytes = 32 << 20

// wireMessage is the raw JSON-RPC envelope exchanged with the app-server.
// The `jsonrpc` header is omitted on the wire, so the message kind is derived
// from which fields are present:
//
//	method + id -> server-initiated request
//	method      -> notification
//	id          -> response (result or error)
type wireMessage struct {
	ID     jsontext.Value `json:"id,omitempty"`
	Method string         `json:"method,omitempty"`
	Params jsontext.Value `json:"params,omitempty"`
	Result jsontext.Value `json:"result,omitempty"`
	Error  *RPCError      `json:"error,omitzero"`
}

// notifyFunc receives server notifications. It runs on the transport reader
// goroutine and must not block.
type notifyFunc func(method string, params jsontext.Value)

// serverRequestFunc handles a server-initiated request. It runs on its own
// goroutine; the returned value is marshaled as the JSON-RPC result, and a
// non-nil error is reported as a JSON-RPC error object.
type serverRequestFunc func(ctx context.Context, method string, params jsontext.Value) (any, error)

// transportConfig configures a transport.
type transportConfig struct {
	// in is the stream of newline-delimited JSON coming from the peer.
	in io.Reader
	// out receives newline-delimited JSON sent to the peer.
	out io.Writer
	// release frees the underlying resources (process, pipes) on Close.
	release func() error
	// onNotification handles server notifications. Optional.
	onNotification notifyFunc
	// onServerRequest handles server-initiated requests. When nil, every
	// server request is answered with a method-not-found error.
	onServerRequest serverRequestFunc
	// maxLine bounds one JSONL message; it defaults to maxLineBytes.
	maxLine int
}

// transport implements JSON-RPC 2.0 framing over a newline-delimited JSON
// byte stream, correlating responses to in-flight calls by id.
type transport struct {
	cfg transportConfig

	writeMu sync.Mutex
	nextID  atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan *wireMessage

	// end records the fatal error that stopped the transport; it is never
	// nil once the transport has stopped.
	end lifecycle.Done

	ctx     context.Context
	cancel  context.CancelFunc
	handler sync.WaitGroup
	reader  sync.WaitGroup

	closeOnce sync.Once
}

// newTransport starts the reader goroutine and returns a ready transport.
func newTransport(cfg transportConfig) *transport {
	ctx, cancel := context.WithCancel(context.Background())
	t := &transport{
		cfg:     cfg,
		pending: make(map[int64]chan *wireMessage),
		ctx:     ctx,
		cancel:  cancel,
	}
	t.reader.Add(1)
	go t.readLoop()
	return t
}

// Done returns a channel closed when the transport stops for any reason.
func (t *transport) Done() <-chan struct{} { return t.end.C() }

// Err returns the error that terminated the transport, or nil while running.
func (t *transport) Err() error { return t.end.Err() }

// Call sends a request and waits for the matching response. result, when
// non-nil, receives the decoded JSON result.
func (t *transport) Call(ctx context.Context, method string, params any, result any) error {
	id := t.nextID.Add(1)
	ch := make(chan *wireMessage, 1)

	if err := t.Err(); err != nil {
		return err
	}
	// A transport that stops after the check above still releases this
	// call through Done.
	t.mu.Lock()
	t.pending[id] = ch
	t.mu.Unlock()

	msg := map[string]any{"method": method, "id": id}
	if params != nil {
		msg["params"] = params
	}
	if err := t.write(msg); err != nil {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
		return ctx.Err()
	case <-t.Done():
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
		return t.Err()
	case resp := <-ch:
		if resp == nil {
			return t.Err()
		}
		if resp.Error != nil {
			return resp.Error
		}
		if result == nil || len(resp.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(resp.Result, result, jsonx.Foreign); err != nil {
			return fmt.Errorf("codex: decode result of %s: %w", method, err)
		}
		return nil
	}
}

// Notify sends a notification, which has no id and no response.
func (t *transport) Notify(method string, params any) error {
	msg := map[string]any{"method": method}
	if params != nil {
		msg["params"] = params
	}
	return t.write(msg)
}

// Close stops the transport, releases the underlying resources, and fails
// every in-flight call. It is safe to call more than once.
func (t *transport) Close() error {
	var releaseErr error
	t.closeOnce.Do(func() {
		t.fatal(ErrClosed)
		if t.cfg.release != nil {
			releaseErr = t.cfg.release()
		}
		// Unblock the reader even when no release hook was supplied.
		if closer, ok := t.cfg.in.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	t.reader.Wait()
	t.handler.Wait()
	return releaseErr
}

// write serializes v as one compact JSON line.
func (t *transport) write(v any) error {
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("codex: encode message: %w", err)
	}
	b = append(b, '\n')

	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if err := t.Err(); err != nil {
		return err
	}
	if _, err := t.cfg.out.Write(b); err != nil {
		return fmt.Errorf("codex: write message: %w", err)
	}
	return nil
}

// readLoop consumes newline-delimited JSON until EOF or a fatal error.
func (t *transport) readLoop() {
	defer t.reader.Done()

	limit := t.cfg.maxLine
	if limit <= 0 {
		limit = maxLineBytes
	}
	start := min(64*1024, limit)
	scanner := bufio.NewScanner(t.cfg.in)
	scanner.Buffer(make([]byte, 0, start), limit)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg wireMessage
		if err := json.Unmarshal(line, &msg, jsonx.Foreign); err != nil {
			// Malformed input is skipped rather than fatal: a stray line on
			// the stream must not tear down a working connection.
			continue
		}
		t.dispatch(&msg)
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	if errors.Is(err, bufio.ErrTooLong) {
		err = fmt.Errorf("codex: app-server message exceeds %d bytes: %w", limit, bufio.ErrTooLong)
	}
	t.fatal(fmt.Errorf("codex: app-server stream ended: %w", err))
}

// dispatch routes one decoded message to the right handler.
func (t *transport) dispatch(msg *wireMessage) {
	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		t.handleServerRequest(msg)
	case msg.Method != "":
		if t.cfg.onNotification != nil {
			t.cfg.onNotification(msg.Method, msg.Params)
		}
	case len(msg.ID) > 0:
		id, err := strconv.ParseInt(string(msg.ID), 10, 64)
		if err != nil {
			return
		}
		t.mu.Lock()
		ch, ok := t.pending[id]
		delete(t.pending, id)
		t.mu.Unlock()
		if ok {
			ch <- msg
		}
	}
}

// handleServerRequest answers a server-initiated request on its own goroutine
// so that a slow handler (an approval prompt, for instance) never blocks the
// reader.
func (t *transport) handleServerRequest(msg *wireMessage) {
	id := append(jsontext.Value(nil), msg.ID...)
	method := msg.Method
	params := append(jsontext.Value(nil), msg.Params...)

	t.handler.Add(1)
	go func() {
		defer t.handler.Done()
		var (
			result any
			err    error
		)
		if t.cfg.onServerRequest == nil {
			err = &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + method}
		} else {
			result, err = t.serveRequest(method, params)
		}
		t.respond(id, result, err)
	}()
}

// serveRequest runs the server-request handler, which calls user code (the
// approval handlers): a panic becomes an internal-error reply instead of
// taking the process down.
func (t *transport) serveRequest(method string, params jsontext.Value) (result any, err error) {
	defer safecall.Recover(&err, method+" handler")
	return t.cfg.onServerRequest(t.ctx, method, params)
}

// respond writes the reply to a server-initiated request.
func (t *transport) respond(id jsontext.Value, result any, err error) {
	reply := map[string]any{"id": id}
	if err != nil {
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) {
			rpcErr = &RPCError{Code: CodeInternalError, Message: err.Error()}
		}
		reply["error"] = rpcErr
	} else {
		if result == nil {
			result = struct{}{}
		}
		reply["result"] = result
	}
	_ = t.write(reply)
}

// fatal marks the transport dead with err, which must not be nil, and
// releases every waiting caller. Only the first call has an effect.
func (t *transport) fatal(err error) {
	if !t.end.Finish(err) {
		return
	}
	t.mu.Lock()
	pending := t.pending
	t.pending = make(map[int64]chan *wireMessage)
	t.mu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
	// Closing the input unblocks a peer that is still writing to us.
	if closer, ok := t.cfg.in.(io.Closer); ok {
		_ = closer.Close()
	}
	t.cancel()
}

// exitWait bounds how long a stdout EOF waits for the process to exit
// before it is reported without an exit status.
const exitWait = 2 * time.Second

// How long Close lets the app-server exit on its own after stdin closes, and
// after SIGTERM, before escalating.
const (
	closeGrace     = 2 * time.Second
	terminateGrace = 5 * time.Second
)

// processHandle holds the streams and lifecycle of a spawned app-server.
type processHandle struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader // reports the process exit in place of io.EOF
	pipe   io.ReadCloser
	stderr *tailbuf.Buffer

	exited  chan struct{} // closed once cmd.Wait returns
	waitErr error
}

// startProcess spawns `codex app-server` (or the configured equivalent) with
// piped stdin/stdout, keeping the tail of stderr for the exit error.
func startProcess(bin string, args []string, env map[string]string, dir string, stderr *tailbuf.Buffer) (*processHandle, error) {
	cmd := exec.Command(bin, args...)
	proc.NewGroup(cmd)
	cmd.Dir = dir
	cmd.Env = proc.Environ(env)
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		if proc.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %w", ErrCLINotFound, err)
		}
		return nil, fmt.Errorf("codex: start %s: %w", bin, err)
	}
	p := &processHandle{cmd: cmd, stdin: stdin, pipe: stdout, stderr: stderr, exited: make(chan struct{})}
	p.stdout = exitReader{p}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// exitReader reads the process's stdout and turns its EOF into a
// *ProcessError carrying the exit status and the tail of stderr. Waiting
// for the exit first guarantees stderr has been fully copied.
type exitReader struct{ p *processHandle }

func (r exitReader) Read(b []byte) (int, error) {
	n, err := r.p.pipe.Read(b)
	if err != io.EOF {
		return n, err
	}
	if !lifecycle.WaitClosed(r.p.exited, exitWait) {
		return n, io.EOF
	}
	exit := &ProcessError{Stderr: r.p.stderr.String(), Err: r.p.waitErr}
	if code := r.p.cmd.ProcessState.ExitCode(); code >= 0 {
		exit.ExitCode = &code
	}
	return n, exit
}

// Close closes stdout so a blocked read returns.
func (r exitReader) Close() error { return r.p.pipe.Close() }

// close asks the subprocess to exit by closing its stdin, escalating to
// SIGTERM after closeGrace and to a kill after terminateGrace, and waits for
// it to exit.
func (p *processHandle) close() error {
	_ = p.stdin.Close()
	_ = proc.Stop(proc.Group(p.cmd.Process), p.exited, closeGrace, terminateGrace)
	_ = p.pipe.Close()
	return nil
}
