package claude

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ironpark/gelati/internal/jsonx"
	"github.com/ironpark/gelati/internal/lifecycle"
)

// DefaultInitializeTimeout bounds the initialize handshake when the caller's
// context carries no deadline. Other control requests have no default
// timeout: their context bounds them.
const DefaultInitializeTimeout = 60 * time.Second

// ControlError reports a failure returned by the CLI for a control request.
type ControlError struct {
	baseError
}

// engine speaks the control protocol on top of a Transport: it demultiplexes
// the CLI's output into typed messages, answers the control requests the CLI
// sends (permissions, hooks, in-process MCP traffic) and correlates the control
// requests the SDK sends with their responses.
type engine struct {
	transport Transport
	cfg       engineConfig

	// mcpServers is the mutable registry of in-process MCP servers that
	// answers mcp_message requests.
	mcpServers *sdkMCPRegistry

	// run tracks when the run ends, for holding the input open; errResults
	// keeps the error result a non-zero exit follows.
	run        *runTracker
	errResults errorResultTracker

	// messages is the stream of non-control messages for the consumer.
	messages *messageQueue
	// mirror receives transcript_mirror frames; nil drops them.
	mirror atomic.Pointer[transcriptMirrorBatcher]

	// mu guards the control-request bookkeeping and the initialize state.
	mu sync.Mutex
	// baseCtx parents the handlers of redelivered control requests.
	baseCtx    context.Context
	counter    int
	pending    map[string]*pendingRequest
	inflight   map[string]*inflightHandler
	initResult *InitializeResult
	// latestCommands is the command list of the latest commands_changed
	// frame; nil until one arrives.
	latestCommands []SlashCommand

	handlers sync.WaitGroup

	startOnce  sync.Once
	closeOnce  sync.Once
	closed     chan struct{}
	readerDone chan struct{}

	// startCtx is the ctx start was given; its cancellation is reported as
	// the reason the session ended.
	startCtx context.Context
	// fatal is the error failAll reported, guarded by mu.
	fatal error
	// end records how the session ended, for Client.Done and Client.Err.
	end lifecycle.Done
}

type controlResult struct {
	response map[string]any
	err      error
}

// pendingRequest is an outbound control request awaiting its response.
type pendingRequest struct {
	ch      chan controlResult
	subtype string
}

// inflightHandler is an inbound control request being answered.
type inflightHandler struct {
	cancel context.CancelFunc
}

// newEngine builds an engine over transport, with the in-process MCP servers
// of opts registered. launch is opts resolved by resolveLaunch; it supplies the
// option-derived initialize fields.
func newEngine(transport Transport, opts *Options, launch *launchConfig) *engine {
	closed := make(chan struct{})
	e := &engine{
		transport:  transport,
		cfg:        newEngineConfig(opts, launch),
		run:        newRunTracker(runEndCeiling(opts.Env), closed),
		messages:   newMessageQueue(closed),
		baseCtx:    context.Background(),
		pending:    map[string]*pendingRequest{},
		inflight:   map[string]*inflightHandler{},
		closed:     closed,
		readerDone: make(chan struct{}),
	}
	e.mcpServers = newSDKMCPRegistry(e.sendMCPFrame)
	servers := sdkMCPServers(opts)
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		e.mcpServers.connect(name, servers[name])
	}
	return e
}

// start begins reading from the transport. It is safe to call more than once.
func (e *engine) start(ctx context.Context) {
	e.startOnce.Do(func() {
		base := context.WithoutCancel(ctx)
		e.mu.Lock()
		e.baseCtx = base
		e.startCtx = ctx
		e.mu.Unlock()
		go e.readLoop(base)
	})
}

// receive yields every non-control message the CLI produced, in order. The
// sequence ends when the CLI's output ends; a fatal error is the last item,
// and a cancelled ctx ends it with ctx's error.
func (e *engine) receive(ctx context.Context) iter.Seq2[Message, error] {
	return e.messages.receive(ctx)
}

// outputDone is closed once the CLI's output has ended and the read loop has
// stopped.
func (e *engine) outputDone() <-chan struct{} {
	return e.readerDone
}

// endInput closes the CLI's input stream.
func (e *engine) endInput() error {
	return e.transport.EndInput()
}

// ---------------------------------------------------------------------------
// Frames
// ---------------------------------------------------------------------------

// controlRequestFrame wraps an SDK-to-CLI control request.
func controlRequestFrame(id string, request map[string]any) map[string]any {
	return map[string]any{"type": "control_request", "request_id": id, "request": request}
}

// controlSuccessFrame answers a CLI control request with a payload, which is
// sent as {} when nil.
func controlSuccessFrame(id string, response map[string]any) map[string]any {
	if response == nil {
		response = map[string]any{}
	}
	return map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "success", "request_id": id, "response": response},
	}
}

// controlErrorFrame answers a CLI control request with an error.
func controlErrorFrame(id, message string) map[string]any {
	return map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "error", "request_id": id, "error": message},
	}
}

// controlCancelFrame withdraws an SDK-to-CLI control request.
func controlCancelFrame(id string) map[string]any {
	return map[string]any{"type": "control_cancel_request", "request_id": id}
}

// encodeFrame encodes one outgoing stream-json frame.
func encodeFrame(frame map[string]any) ([]byte, error) {
	payload, err := json.Marshal(frame, jsonx.LegacyEncode)
	if err != nil {
		return nil, fmt.Errorf("claude: encoding %s frame: %w", cmp.Or(str(frame["type"]), "stream-json"), err)
	}
	return payload, nil
}

// writeFrame encodes one stream-json frame and writes it to the CLI.
func (e *engine) writeFrame(ctx context.Context, frame map[string]any) error {
	payload, err := encodeFrame(frame)
	if err != nil {
		return err
	}
	return e.transport.Write(ctx, payload)
}

// sendMCPFrame writes a frame for the in-process MCP servers, which may
// outlive the engine.
func (e *engine) sendMCPFrame(ctx context.Context, frame map[string]any) error {
	if e.isClosed() {
		return NewConnectionError("connection closed")
	}
	return e.writeFrame(ctx, frame)
}

// ---------------------------------------------------------------------------
// Reader
// ---------------------------------------------------------------------------

// readLoop demultiplexes the transport's frames until the CLI's output ends.
func (e *engine) readLoop(ctx context.Context) {
	var reason error
	defer close(e.readerDone)
	// Ends the session after the message stream is closed, so a consumer
	// that sees the stream end and then checks Err finds the session over.
	defer func() { e.end.Finish(reason) }()
	defer e.closeMessages()
	defer e.run.finalize()
	// Flush mirror entries batched since the last result (late subagent
	// writes, early EOF, transport errors) before the stream ends.
	defer e.flushMirror(ctx)
	// Decide why the output ended as the loop exits, before the flushes
	// above: a Close during a slow flush must not hide a crash.
	defer func() { reason = e.endReason() }()

	for raw, err := range e.transport.ReadMessages() {
		if err != nil {
			e.failAll(err)
			return
		}
		if e.isClosed() {
			return
		}
		var frame map[string]any
		if jsonx.Unmarshal(raw, &frame) != nil || frame == nil {
			// Frames that are not JSON objects carry no message; skip
			// them like the TypeScript SDK instead of failing the run.
			e.cfg.logger.Debug("claude: skipped non-object frame", "frame", raw)
			continue
		}
		if done := e.route(ctx, frame, raw); done {
			return
		}
	}
}

// route handles one decoded frame, reporting whether the read loop should stop.
func (e *engine) route(ctx context.Context, frame map[string]any, raw jsontext.Value) bool {
	typ := str(frame["type"])
	switch typ {
	case "control_response":
		response, _ := frame["response"].(map[string]any)
		e.deliverControlResponse(response)
		return false
	case "control_request":
		if !e.isClosed() {
			e.spawnControlHandler(ctx, frame)
		}
		return false
	case "control_cancel_request":
		if id := str(frame["request_id"]); id != "" {
			e.cancelInbound(id)
		}
		return false
	case "keep_alive":
		// A heartbeat touches no state.
		return false
	case "transcript_mirror":
		// SessionStore write path: never surfaced to consumers.
		e.enqueueMirrorFrame(frame)
		return false
	}

	subtype := str(frame["subtype"])
	if typ == "system" {
		e.run.onTaskFrame(frame)
		if subtype == "commands_changed" {
			e.noteCommandsChanged(frame)
		}
	}

	switch {
	case isStatePreservingFrame(typ, subtype):
		// Summaries and side-channel state are delivered without counting
		// as conversation activity, so they keep the last error result.
	case typ == "result":
		// Consumers that see the result can rely on the store holding the
		// turn's transcript.
		e.flushMirror(ctx)
		e.errResults.onResult(frame)
		e.run.onResult(e.holdsInput())
	case typ == "system" && subtype == "session_state_changed":
		e.run.onSessionState(str(frame["state"]))
		if hostOnly, _ := frame["sdk_host_only"].(bool); hostOnly {
			// Sent only because the SDK asked for session state; the
			// caller did not opt in.
			return false
		}
	default:
		e.errResults.onActivity(typ, subtype)
		if typ == "assistant" || typ == "stream_event" {
			if parent, ok := frame["parent_tool_use_id"]; ok && parent == nil {
				e.run.onMainThreadActivity()
			}
		}
	}

	msg, err := parseMessageMap(frame, raw)
	if err != nil {
		e.failAll(err)
		return true
	}
	return !e.messages.push(messageOrError{msg: msg})
}

// isStatePreservingFrame reports frames the TypeScript SDK passes through
// without resetting the error-result or run-end state.
func isStatePreservingFrame(typ, subtype string) bool {
	switch typ {
	case "active_goal", "autocompact_state":
		return true
	case "system":
		switch subtype {
		case "post_turn_summary", "task_summary", "session_metadata":
			return true
		}
	}
	return false
}

// noteCommandsChanged keeps the command list a commands_changed frame
// carries, which supersedes the one from initialize.
func (e *engine) noteCommandsChanged(frame map[string]any) {
	list, ok := frame["commands"].([]any)
	if !ok {
		return
	}
	commands := make([]SlashCommand, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var cmd SlashCommand
		if decodeResponse(m, &cmd) == nil {
			commands = append(commands, cmd)
		}
	}
	e.mu.Lock()
	e.latestCommands = commands
	e.mu.Unlock()
}

// failAll turns a fatal read error into the stream's final item and fails every
// control request still waiting for a response.
func (e *engine) failAll(err error) {
	err = e.errResults.translate(err)
	e.mu.Lock()
	e.fatal = err
	pending := e.pending
	e.pending = map[string]*pendingRequest{}
	e.mu.Unlock()
	for _, p := range pending {
		select {
		case p.ch <- controlResult{err: err}:
		default:
		}
	}
	e.messages.push(messageOrError{err: err})
}

// ---------------------------------------------------------------------------
// Outgoing control requests
// ---------------------------------------------------------------------------

// deliverControlResponse matches a response to its pending request.
func (e *engine) deliverControlResponse(response map[string]any) {
	if response == nil {
		return
	}
	id := str(response["request_id"])
	e.mu.Lock()
	p, ok := e.pending[id]
	delete(e.pending, id)
	e.mu.Unlock()
	if !ok {
		e.cfg.logger.Debug("claude: control response for unknown request", "request_id", id)
		return
	}
	if p.subtype == "initialize" {
		// Prompts the CLI issued before this client (re)attached are
		// redelivered on the initialize response; the prompt-redelivery
		// fields are ignored on any other response.
		e.redeliverPending(response["pending_permission_requests"], "can_use_tool")
		e.redeliverPending(response["pending_user_dialog_requests"], "request_user_dialog")
	}
	if str(response["subtype"]) == "error" {
		msg := str(response["error"])
		if msg == "" {
			msg = "Unknown error"
		}
		p.ch <- controlResult{err: &ControlError{baseError{Msg: msg}}}
		return
	}
	payload, _ := response["response"].(map[string]any)
	p.ch <- controlResult{response: payload}
}

// redeliverPending answers the control requests listed on an initialize
// response, as if they had just arrived. A request already being answered is
// not answered twice.
func (e *engine) redeliverPending(list any, subtype string) {
	frames, _ := list.([]any)
	if len(frames) == 0 || e.isClosed() {
		return
	}
	e.mu.Lock()
	ctx := e.baseCtx
	e.mu.Unlock()
	for _, item := range frames {
		frame, ok := item.(map[string]any)
		if !ok {
			continue
		}
		request, _ := frame["request"].(map[string]any)
		if str(request["subtype"]) != subtype {
			continue
		}
		e.spawnControlHandler(ctx, frame)
	}
}

// request sends a control request of the given subtype with extra fields and
// returns the response payload, which is never nil on success.
func (e *engine) request(ctx context.Context, subtype string, fields map[string]any) (map[string]any, error) {
	req := make(map[string]any, len(fields)+1)
	maps.Copy(req, fields)
	req["subtype"] = subtype
	resp, err := e.sendControlRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		resp = map[string]any{}
	}
	return resp, nil
}

// sendControlRequest writes one control request and waits for its response.
// Only ctx bounds the wait, as in the TypeScript SDK; when ctx ends after the
// request was written, the CLI is told with a control_cancel_request.
func (e *engine) sendControlRequest(ctx context.Context, request map[string]any) (map[string]any, error) {
	wait, err := e.beginControlRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	return wait(ctx)
}

// beginControlRequest writes one control request and returns the function
// that waits for its response. Splitting the two lets a caller order several
// requests on the wire before waiting on any of them.
func (e *engine) beginControlRequest(ctx context.Context, request map[string]any) (func(context.Context) (map[string]any, error), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.isClosed() {
		return nil, NewConnectionError("connection closed")
	}
	e.mu.Lock()
	e.counter++
	id := "req_" + strconv.Itoa(e.counter) + "_" + randomHex(4)
	p := &pendingRequest{ch: make(chan controlResult, 1), subtype: str(request["subtype"])}
	e.pending[id] = p
	e.mu.Unlock()

	cleanup := func() {
		e.mu.Lock()
		delete(e.pending, id)
		e.mu.Unlock()
	}
	if err := e.writeFrame(ctx, controlRequestFrame(id, request)); err != nil {
		cleanup()
		return nil, err
	}

	return func(ctx context.Context) (map[string]any, error) {
		select {
		case res := <-p.ch:
			return res.response, res.err
		case <-ctx.Done():
			cleanup()
			e.cancelOutbound(ctx, id)
			return nil, ctx.Err()
		case <-e.closed:
			cleanup()
			return nil, NewConnectionError("connection closed while awaiting a control response")
		case <-e.readerDone:
			// The read loop delivers any response before it ends, so one
			// that arrived is already buffered in p.ch.
			select {
			case res := <-p.ch:
				return res.response, res.err
			default:
			}
			cleanup()
			return nil, NewConnectionError("CLI output ended while awaiting a control response")
		}
	}, nil
}

// cancelOutbound tells the CLI that the SDK abandoned request id. It is best
// effort: the CLI may already have answered.
func (e *engine) cancelOutbound(ctx context.Context, id string) {
	if e.isClosed() {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = e.writeFrame(writeCtx, controlCancelFrame(id))
}

func randomHex(n int) string {
	buf := make([]byte, n)
	// crypto/rand.Read never fails on supported platforms.
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// ---------------------------------------------------------------------------
// Initialize
// ---------------------------------------------------------------------------

// initialize performs the initialize handshake, registering hooks and
// capabilities, and records the response. A repeated call re-sends the same
// hook registrations. When ctx has no deadline, DefaultInitializeTimeout
// bounds the handshake, and running out of it is reported with a hint at the
// usual causes.
func (e *engine) initialize(ctx context.Context) (*InitializeResult, error) {
	initCtx := ctx
	_, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		var cancel context.CancelFunc
		initCtx, cancel = context.WithTimeout(ctx, DefaultInitializeTimeout)
		defer cancel()
	}
	response, err := e.request(initCtx, "initialize", e.initializeFields())
	if err != nil {
		if !hasDeadline && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("claude: CLI initialization did not complete within %s; "+
				"check authentication and network connectivity: %w", DefaultInitializeTimeout, err)
		}
		return nil, err
	}
	return e.setInitResponse(response), nil
}

// initializeFields assembles the initialize request's fields: hooks, the
// option-derived fields and the in-process MCP server declarations.
func (e *engine) initializeFields() map[string]any {
	fields := map[string]any{}
	if len(e.cfg.hooksWire) > 0 {
		fields["hooks"] = e.cfg.hooksWire
	}
	maps.Copy(fields, e.cfg.initFields)
	// Declare the live in-process servers: after Client.SetMCPServers a
	// re-initialize must announce the current set, not the configured one.
	maps.Copy(fields, sdkMCPInitializeFields(e.mcpServers.configs()))
	return fields
}

// setInitResponse records an initialize response. A field of an unexpected
// shape leaves its typed counterpart empty rather than failing the session;
// Raw still has it.
func (e *engine) setInitResponse(response map[string]any) *InitializeResult {
	result := &InitializeResult{Raw: response}
	decodeValue(response, result)
	e.mu.Lock()
	e.initResult = result
	e.mu.Unlock()
	return result
}

// initializeResult reports the typed initialize response, or nil before
// initialize.
func (e *engine) initializeResult() *InitializeResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.initResult
}

// supportedCommands reports the latest command list: from the last
// commands_changed frame, else from the initialize response.
func (e *engine) supportedCommands() []SlashCommand {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.latestCommands != nil {
		return e.latestCommands
	}
	if e.initResult != nil {
		return e.initResult.Commands
	}
	return nil
}

// ---------------------------------------------------------------------------
// Input
// ---------------------------------------------------------------------------

// holdsInput reports whether the CLI may still send control requests that need
// a reply, in which case the input stream must stay open until the run ends.
func (e *engine) holdsInput() bool {
	return e.cfg.canUseTool != nil || e.cfg.hasHooks || e.mcpServers.hasServers() ||
		e.cfg.onElicitation != nil || e.cfg.onUserDialog != nil
}

// sendUserMessage writes one user turn, stamped per Options.VerbatimPrompts,
// and opens the run it owes.
func (e *engine) sendUserMessage(ctx context.Context, input UserInput) error {
	payload, err := encodeFrame(stampUserMessage(input.frame(), e.cfg.verbatimPrompts))
	if err != nil {
		return err
	}
	e.run.startTurn()
	return e.transport.Write(ctx, payload)
}

// streamInput writes each user turn and then ends the input stream, once the
// run has ended when the session may still need it.
func (e *engine) streamInput(ctx context.Context, inputs iter.Seq[UserInput]) error {
	written := 0
	var writeErr error
	for input := range inputs {
		if e.isClosed() {
			break
		}
		if err := e.sendUserMessage(ctx, input); err != nil {
			writeErr = err
			break
		}
		written++
	}
	// Hold the input open until the run ends while the session may still
	// need it. With nothing sent no result would arrive to end the run, so
	// there is nothing to wait for.
	if written > 0 && e.holdsInput() {
		if err := e.run.wait(ctx); err != nil {
			return cmp.Or(writeErr, err)
		}
	}
	e.run.finalize()
	return cmp.Or(writeErr, e.transport.EndInput())
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (e *engine) isClosed() bool {
	return isDone(e.closed)
}

// close stops the engine: it releases anything blocked on the message stream,
// waits for in-flight control handlers, and closes the transport. With
// transcript mirroring enabled it first flushes pending entries to the
// SessionStore, waiting at most the batcher's close timeout in total.
func (e *engine) close() error {
	var err error
	e.closeOnce.Do(func() {
		mirror := e.mirror.Load()
		mirrorCtx := context.Background()
		if mirror != nil {
			var cancel context.CancelFunc
			mirrorCtx, cancel = context.WithTimeout(mirrorCtx, mirror.closeTimeout)
			defer cancel()
			// Flush before tearing down so a consumer that stops early does
			// not lose the current turn when the process exits.
			mirror.flush(mirrorCtx)
		}
		close(e.closed)
		// Cancel in-flight control handlers so a callback that honors its
		// ctx (a permission prompt awaiting a user, say) cannot hold up
		// handlers.Wait below indefinitely.
		e.mu.Lock()
		inflight := slices.Collect(maps.Values(e.inflight))
		clear(e.inflight)
		e.mu.Unlock()
		for _, h := range inflight {
			h.cancel()
		}
		// Detach the in-process MCP servers, cancelling their in-flight
		// requests for the same reason.
		e.mcpServers.closeAll()
		e.run.finalize()
		err = e.transport.Close()
		// A transport whose Close leaves the reader blocked must not wedge
		// the caller; the goroutine ends when its stream does.
		lifecycle.WaitClosed(e.readerDone, 5*time.Second)
		e.handlers.Wait()
		if mirror != nil {
			// Final flush of whatever the reader enqueued while stopping;
			// past the deadline in-flight appends are abandoned.
			mirror.close(mirrorCtx)
		}
		// Covers an engine never started, or a reader still wedged.
		e.end.Finish(nil)
	})
	return err
}

// endReason reports why the read loop stopped: nil when close stopped it,
// else the cancellation of the session's ctx, the fatal error that ended
// the output, or a ConnectionError when the output simply ended.
func (e *engine) endReason() error {
	if e.isClosed() {
		return nil
	}
	e.mu.Lock()
	ctx, fatal := e.startCtx, e.fatal
	e.mu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if fatal != nil {
		return fatal
	}
	return NewConnectionError("Claude Code output ended")
}
