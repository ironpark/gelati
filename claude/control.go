package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultInitializeTimeout bounds the initialize handshake when the caller's
// context carries no earlier deadline. Other control requests have no
// default timeout: their context bounds them.
const DefaultInitializeTimeout = 60 * time.Second

// defaultRunEndCeiling is how long the run stays open after a result while
// the CLI still reports work, unless CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS says
// otherwise (the CLI's own background-wait ceiling).
const defaultRunEndCeiling = 600 * time.Second

// maxRunEndCeiling caps the ceiling, as the TypeScript SDK's timers do.
const maxRunEndCeiling = (1<<31 - 1) * time.Millisecond

// messageBufferSize is how many messages the engine buffers ahead of the
// consumer.
const messageBufferSize = 100

// deferringTaskTypes are the task types whose completion runs a follow-up turn,
// so the input stream must stay open past the turn's result frame. Background
// shells and other open-ended tasks are deliberately excluded: they may never
// reach a terminal status, and holding input open for one would withhold it
// forever.
var deferringTaskTypes = map[string]bool{"local_agent": true, "local_workflow": true}

// ControlError reports a failure returned by the CLI for a control request.
type ControlError struct {
	baseError
}

// ServerInfo is the CLI's response to the initialize handshake: supported
// commands, output styles and other capability metadata, passed through as-is.
type ServerInfo map[string]any

// engine speaks the control protocol on top of a Transport: it demultiplexes
// the CLI's output into typed messages, answers the control requests the CLI
// sends (permissions, hooks, in-process MCP traffic) and correlates the control
// requests the SDK sends with their responses.
type engine struct {
	transport Transport
	opts      *Options

	// mcpServers is the mutable registry of in-process MCP servers that
	// answers mcp_message requests. Without it such requests are refused
	// with a JSON-RPC method-not-found error.
	mcpServers *sdkMCPRegistry

	hookCallbacks map[string]HookCallback
	// initHooks is the hooks field of the initialize request. It is built
	// once, so a re-initialize registers the same callback IDs.
	initHooks     map[string]any
	initHooksOnce sync.Once

	// baseCtx parents the handlers of redelivered control requests.
	baseCtx context.Context

	mu           sync.Mutex
	counter      int
	pending      map[string]*pendingRequest
	inflight     map[string]*inflightHandler
	inflightTask map[string]bool
	initResult   *InitializeResult
	// latestCommands is the command list of the latest commands_changed
	// frame; nil until one arrives.
	latestCommands []SlashCommand

	// Error-result state, mirroring the TypeScript SDK: the last error
	// result, kept across frames that do not move the conversation on, and
	// the error result before a run of model-less turns.
	lastErrorRes         map[string]any
	errorBeforeModelLess map[string]any
	modelLessTurnEnded   bool

	// Run-end state, mirroring the TypeScript SDK. The run ends on a result
	// once the CLI reports "idle" (or never reports state at all); runEndCh
	// is closed when it does and replaced when work reopens the run.
	sessionState   string
	resultReceived bool
	runEnded       bool
	runFinal       bool
	runEndCh       chan struct{}
	ceiling        time.Duration
	ceilingTimer   *time.Timer
	ceilingGen     int

	handlers sync.WaitGroup

	messages chan messageOrError
	// msgMu serializes senders other than the read loop (mirror error
	// reports) with the read loop closing messages.
	msgMu     sync.Mutex
	msgClosed bool

	// mirror receives transcript_mirror frames; nil drops them.
	mirror atomic.Pointer[transcriptMirrorBatcher]

	startOnce  sync.Once
	closeOnce  sync.Once
	closed     chan struct{}
	readerDone chan struct{}
}

type messageOrError struct {
	msg Message
	err error
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

// newEngine builds an engine over transport. opts may be nil.
func newEngine(transport Transport, opts *Options) *engine {
	if opts == nil {
		opts = &Options{}
	}
	return &engine{
		transport:     transport,
		opts:          opts,
		hookCallbacks: map[string]HookCallback{},
		baseCtx:       context.Background(),
		pending:       map[string]*pendingRequest{},
		inflight:      map[string]*inflightHandler{},
		inflightTask:  map[string]bool{},
		runEndCh:      make(chan struct{}),
		ceiling:       runEndCeiling(opts.Env),
		messages:      make(chan messageOrError, messageBufferSize),
		closed:        make(chan struct{}),
		readerDone:    make(chan struct{}),
	}
}

// Start begins reading from the transport. It is safe to call more than once.
func (e *engine) Start(ctx context.Context) {
	e.startOnce.Do(func() {
		base := context.WithoutCancel(ctx)
		e.mu.Lock()
		e.baseCtx = base
		e.mu.Unlock()
		go e.readLoop(base)
	})
}

// ServerInfo reports the initialize response, or nil before Initialize.
func (e *engine) ServerInfo() ServerInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.initResult == nil {
		return nil
	}
	return ServerInfo(e.initResult.Raw)
}

// Messages yields every non-control message the CLI produced, in order. The
// sequence ends when the CLI's output ends; a fatal error is the last item.
func (e *engine) Messages() iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		for item := range e.messages {
			if !yield(item.msg, item.err) {
				return
			}
			if item.err != nil {
				return
			}
		}
	}
}

// messagesWithContext is Messages with cancellation: a cancelled ctx ends the
// sequence with its error.
func (e *engine) messagesWithContext(ctx context.Context) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		for {
			select {
			case item, ok := <-e.messages:
				if !ok {
					return
				}
				if !yield(item.msg, item.err) {
					return
				}
				if item.err != nil {
					return
				}
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
		}
	}
}

// marshalFrame encodes one outgoing stream-json frame.
func marshalFrame(frame map[string]any) ([]byte, error) {
	payload, err := json.Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("claude: encoding user message: %w", err)
	}
	return payload, nil
}

// ---------------------------------------------------------------------------
// Reader
// ---------------------------------------------------------------------------

// readLoop demultiplexes the transport's frames until the CLI's output ends.
func (e *engine) readLoop(ctx context.Context) {
	defer close(e.readerDone)
	defer e.closeMessages()
	defer e.finalizeRun()
	// Flush mirror entries batched since the last result (late subagent
	// writes, early EOF, transport errors) before the stream ends.
	defer e.flushMirror(ctx)

	for raw, err := range e.transport.ReadMessages() {
		if err != nil {
			e.failAll(err)
			return
		}
		if e.isClosed() {
			return
		}
		var frame map[string]any
		if json.Unmarshal(raw, &frame) != nil || frame == nil {
			// Frames that are not JSON objects carry no message; skip
			// them like the TypeScript SDK instead of failing the run.
			continue
		}
		if done := e.route(ctx, frame, raw); done {
			return
		}
	}
}

// route handles one decoded frame, reporting whether the read loop should stop.
func (e *engine) route(ctx context.Context, frame map[string]any, raw json.RawMessage) bool {
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
			e.mu.Lock()
			h := e.inflight[id]
			delete(e.inflight, id)
			e.mu.Unlock()
			if h != nil {
				h.cancel()
			}
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
		e.trackTaskLifecycle(frame)
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
		e.noteResult(frame)
	case typ == "system" && subtype == "session_state_changed":
		e.noteSessionState(str(frame["state"]))
		if hostOnly, _ := frame["sdk_host_only"].(bool); hostOnly {
			// Sent only because the SDK asked for session state; the
			// caller did not opt in.
			return false
		}
	default:
		e.noteActivity(frame, typ, subtype)
	}

	msg, err := parseMessageMap(frame, raw)
	if err != nil {
		e.failAll(err)
		return true
	}
	if msg == nil {
		return false
	}
	return !e.emit(messageOrError{msg: msg})
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

// emit hands one item to the consumer, reporting whether it was accepted.
func (e *engine) emit(item messageOrError) bool {
	select {
	case e.messages <- item:
		return true
	case <-e.closed:
		return false
	}
}

// noteResult records a result frame: the error-result state, and the end of
// the run when the CLI reports no further work.
func (e *engine) noteResult(frame map[string]any) {
	isErr, _ := frame["is_error"].(bool)
	numTurns, _ := toInt(frame["num_turns"])
	result, hasResult := frame["result"].(string)
	needs := e.hasBidirectionalNeeds()

	e.mu.Lock()
	defer e.mu.Unlock()
	if isErr {
		e.lastErrorRes = frame
	} else {
		e.lastErrorRes = nil
	}
	// A model-less turn (a local command, say) keeps the error that came
	// before it, should the CLI exit non-zero after it.
	e.modelLessTurnEnded = str(frame["subtype"]) == "success" && !isErr &&
		numTurns == 0 && hasResult && result == ""
	if !e.modelLessTurnEnded {
		e.errorBeforeModelLess = e.lastErrorRes
	}

	e.resultReceived = true
	if e.sessionState == "" || e.sessionState == SessionStateIdle || !needs {
		e.maybeEndRunLocked()
	} else {
		// The CLI still reports work (a background agent, a follow-up turn
		// it owes): wait for "idle", but not forever.
		e.armCeilingLocked()
	}
}

// noteSessionState tracks the CLI's session_state_changed reports.
func (e *engine) noteSessionState(state string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sessionState = state
	if state == SessionStateIdle {
		if e.resultReceived {
			e.maybeEndRunLocked()
		}
		return
	}
	if e.runFinal {
		return
	}
	// Work taken up after the run ended reopens it until the next idle.
	e.reopenRunLocked()
	if state == SessionStateRequiresAction {
		// This host is answering a request; the input must outlast it.
		e.clearCeilingLocked()
	} else if e.resultReceived {
		e.armCeilingLocked()
	}
}

// noteActivity records a frame that moves the conversation on.
func (e *engine) noteActivity(frame map[string]any, typ, subtype string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// A later non-zero exit is now a fresh failure rather than the expected
	// exit after an error result.
	e.lastErrorRes = nil
	if typ == "system" && subtype == "init" {
		e.modelLessTurnEnded = false
	} else if typ != "system" || subtype == "compact_boundary" {
		e.errorBeforeModelLess = nil
		e.modelLessTurnEnded = false
	}
	if typ == "assistant" || typ == "stream_event" {
		// A main-thread turn is under way: the ceiling counts only the wait
		// between turns, and the run reopens.
		if parent, ok := frame["parent_tool_use_id"]; ok && parent == nil && !e.runFinal {
			e.reopenRunLocked()
			e.clearCeilingLocked()
		}
	}
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

// maybeEndRunLocked ends the run, unless the CLI reports no session state and
// a delegated task is still in flight: such a task wakes the session for a
// follow-up turn whose control requests still need the input stream.
func (e *engine) maybeEndRunLocked() {
	if e.sessionState == "" && len(e.inflightTask) > 0 {
		return
	}
	e.endRunLocked()
}

func (e *engine) endRunLocked() {
	e.clearCeilingLocked()
	if e.runEnded {
		return
	}
	e.runEnded = true
	close(e.runEndCh)
}

// reopenRunLocked makes a later wait for the run end wait for new work. A
// waiter the ended run already woke is unaffected.
func (e *engine) reopenRunLocked() {
	if e.runEnded && !e.runFinal {
		e.runEnded = false
		e.runEndCh = make(chan struct{})
	}
}

// armCeilingLocked ends the run anyway once the ceiling passes without the
// CLI reporting "idle" or starting a new turn.
func (e *engine) armCeilingLocked() {
	e.clearCeilingLocked()
	if e.runEnded || e.runFinal || e.ceiling <= 0 || e.isClosed() {
		return
	}
	gen := e.ceilingGen
	e.ceilingTimer = time.AfterFunc(min(e.ceiling, maxRunEndCeiling), func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if gen == e.ceilingGen {
			e.ceilingTimer = nil
			e.endRunLocked()
		}
	})
}

func (e *engine) clearCeilingLocked() {
	e.ceilingGen++
	if e.ceilingTimer != nil {
		e.ceilingTimer.Stop()
		e.ceilingTimer = nil
	}
}

// startTurn records a user message written to the CLI: that prompt owes a
// run of its own, result included.
func (e *engine) startTurn() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.runFinal {
		return
	}
	e.reopenRunLocked()
	e.resultReceived = false
	e.clearCeilingLocked()
}

// finalizeRun ends the run for good: the input is closed or the reader is
// gone, so nothing can wait on a reopened run.
func (e *engine) finalizeRun() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runFinal = true
	e.endRunLocked()
}

// waitForRunEnd blocks until the run ends, the engine closes or ctx is done.
func (e *engine) waitForRunEnd(ctx context.Context) error {
	e.mu.Lock()
	ended, ch := e.runEnded, e.runEndCh
	e.mu.Unlock()
	if ended {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-e.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runEndCeiling reads CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS as the CLI will see
// it: Options.Env first, then the process environment. Zero disables the
// ceiling; a value that is not a non-negative integer means the default.
func runEndCeiling(env map[string]string) time.Duration {
	const key = "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS"
	raw, ok := env[key]
	if !ok {
		raw, ok = os.LookupEnv(key)
	}
	if !ok {
		return defaultRunEndCeiling
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || ms < 0 {
		return defaultRunEndCeiling
	}
	if ms > int64(maxRunEndCeiling/time.Millisecond) {
		return maxRunEndCeiling
	}
	return time.Duration(ms) * time.Millisecond
}

// trackTaskLifecycle keeps the set of delegated tasks that are still running.
func (e *engine) trackTaskLifecycle(frame map[string]any) {
	taskID := str(frame["task_id"])
	if taskID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch str(frame["subtype"]) {
	case "task_started":
		if deferringTaskTypes[str(frame["task_type"])] {
			e.inflightTask[taskID] = true
		}
	case "task_notification":
		delete(e.inflightTask, taskID)
	case "task_updated":
		patch, _ := frame["patch"].(map[string]any)
		if TerminalTaskStatuses[str(patch["status"])] {
			delete(e.inflightTask, taskID)
		}
	}
}

// failAll turns a fatal read error into the stream's final item and fails every
// control request still waiting for a response.
func (e *engine) failAll(err error) {
	var perr *ProcessError
	if errors.As(err, &perr) {
		e.mu.Lock()
		last := e.lastErrorRes
		if last == nil && e.modelLessTurnEnded {
			last = e.errorBeforeModelLess
		}
		e.mu.Unlock()
		if last != nil {
			// The CLI exits non-zero on purpose after reporting an error
			// result; the generic exit-code error carries nothing the result
			// does not already say.
			err = NewResultError(
				"Claude Code returned an error result: "+errorResultText(last),
				last, perr.ExitCode)
		}
	}

	e.mu.Lock()
	pending := e.pending
	e.pending = map[string]*pendingRequest{}
	e.mu.Unlock()
	for _, p := range pending {
		select {
		case p.ch <- controlResult{err: err}:
		default:
		}
	}
	e.emit(messageOrError{err: err})
}

// errorResultText picks the most informative text out of a failed result frame.
func errorResultText(frame map[string]any) string {
	if errs := normalizeResultErrors(frame["errors"]); len(errs) > 0 {
		return strings.Join(errs, "; ")
	}
	if result := strings.TrimSpace(str(frame["result"])); result != "" {
		return result
	}
	if subtype := str(frame["subtype"]); subtype != "" && subtype != "success" {
		return subtype
	}
	if status, ok := toInt(frame["api_error_status"]); ok {
		return fmt.Sprintf("API error (HTTP %d)", status)
	}
	return "unknown error"
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

	frame := map[string]any{"type": "control_request", "request_id": id, "request": request}
	payload, err := json.Marshal(frame)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("claude: encoding control request: %w", err)
	}
	if err := e.transport.Write(ctx, payload); err != nil {
		cleanup()
		return nil, err
	}

	return func(ctx context.Context) (map[string]any, error) {
		select {
		case res := <-p.ch:
			if res.err != nil {
				return nil, res.err
			}
			return res.response, nil
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
				if res.err != nil {
					return nil, res.err
				}
				return res.response, nil
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
	payload, err := json.Marshal(map[string]any{"type": "control_cancel_request", "request_id": id})
	if err != nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = e.transport.Write(writeCtx, payload)
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

func randomHex(n int) string {
	buf := make([]byte, n)
	// crypto/rand.Read never fails on supported platforms.
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// Initialize performs the initialize handshake, registering hooks and
// capabilities, and stores the response for ServerInfo. Unless ctx has an
// earlier deadline, DefaultInitializeTimeout bounds it.
func (e *engine) Initialize(ctx context.Context) (ServerInfo, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultInitializeTimeout)
		defer cancel()
	}
	if _, err := e.initialize(ctx); err != nil {
		return nil, err
	}
	return e.ServerInfo(), nil
}

// initialize sends an initialize request and records its response. A repeated
// call re-sends the same hook registrations.
func (e *engine) initialize(ctx context.Context) (*InitializeResult, error) {
	response, err := e.sendControlRequest(ctx, e.buildInitializeRequest())
	if err != nil {
		return nil, err
	}
	if response == nil {
		response = map[string]any{}
	}
	return e.setInitResponse(response), nil
}

// setInitResponse records an initialize response. A field of an unexpected
// shape leaves its typed counterpart empty rather than failing the session;
// Raw still has it.
func (e *engine) setInitResponse(response map[string]any) *InitializeResult {
	result := &InitializeResult{Raw: response}
	_ = decodeResponse(response, result)
	e.mu.Lock()
	e.initResult = result
	e.mu.Unlock()
	return result
}

// buildInitializeRequest assembles the initialize request: hooks and the
// fields this engine owns, plus the option-derived and SDK MCP server fields.
func (e *engine) buildInitializeRequest() map[string]any {
	request := map[string]any{"subtype": "initialize"}
	e.initHooksOnce.Do(func() { e.initHooks = e.registerHooks() })
	if len(e.initHooks) > 0 {
		request["hooks"] = e.initHooks
	}
	maps.Copy(request, initializeExtras(e.opts))
	// Declare the live in-process servers: after Client.SetMCPServers a
	// re-initialize must announce the current set, not the configured one.
	if e.mcpServers != nil {
		maps.Copy(request, sdkMCPDeclarationFields(e.mcpServers.configs()))
	} else {
		maps.Copy(request, sdkMCPInitializeFields(e.opts))
	}
	return request
}

// registerHooks assigns callback IDs to the configured hooks and returns the
// initialize request's hooks field, empty when there are none.
func (e *engine) registerHooks() map[string]any {
	hooksConfig := map[string]any{}
	e.hookCallbacks = map[string]HookCallback{}
	next := 0
	for _, event := range slices.Sorted(maps.Keys(e.opts.Hooks)) {
		matchers := e.opts.Hooks[event]
		if len(matchers) == 0 {
			continue
		}
		configs := make([]map[string]any, 0, len(matchers))
		for _, matcher := range matchers {
			ids := make([]string, 0, len(matcher.Hooks))
			for _, cb := range matcher.Hooks {
				id := "hook_" + strconv.Itoa(next)
				next++
				e.hookCallbacks[id] = cb
				ids = append(ids, id)
			}
			config := map[string]any{"matcher": nil, "hookCallbackIds": ids}
			if matcher.Matcher != "" {
				config["matcher"] = matcher.Matcher
			}
			if matcher.Timeout > 0 {
				config["timeout"] = matcher.Timeout
			}
			configs = append(configs, config)
		}
		hooksConfig[event] = configs
	}
	return hooksConfig
}

// InitializeResult reports the typed initialize response, or nil before
// Initialize.
func (e *engine) InitializeResult() *InitializeResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.initResult
}

// SupportedCommands reports the latest command list: from the last
// commands_changed frame, else from the initialize response.
func (e *engine) SupportedCommands() []SlashCommand {
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

// Interrupt aborts the current turn.
func (e *engine) Interrupt(ctx context.Context) error {
	_, err := e.InterruptWithReceipt(ctx, false)
	return err
}

// InterruptWithReceipt aborts the current turn and returns the CLI's receipt,
// or nil from a CLI that sends none.
func (e *engine) InterruptWithReceipt(ctx context.Context, cancelQueued bool) (*InterruptReceipt, error) {
	fields := map[string]any{}
	if cancelQueued {
		fields["cancel_queued"] = true
	}
	resp, err := e.request(ctx, "interrupt", fields)
	if err != nil {
		return nil, err
	}
	stillQueued, ok := resp["still_queued"].([]any)
	if !ok {
		return nil, nil
	}
	receipt := &InterruptReceipt{StillQueued: stringItems(stillQueued)}
	if cancelled, ok := resp["cancelled"].([]any); ok {
		receipt.Cancelled = stringItems(cancelled)
	}
	return receipt, nil
}

// stringItems keeps the string elements of a JSON array.
func stringItems(list []any) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// SetPermissionMode changes the session's permission mode.
func (e *engine) SetPermissionMode(ctx context.Context, mode PermissionMode) error {
	_, err := e.request(ctx, "set_permission_mode", map[string]any{"mode": mode})
	return err
}

// SetModel changes the model. An empty model restores the CLI default.
func (e *engine) SetModel(ctx context.Context, model string) error {
	var value any
	if model != "" {
		value = model
	}
	_, err := e.request(ctx, "set_model", map[string]any{"model": value})
	return err
}

// RewindFiles restores tracked files to their state at a user message, or
// with dryRun only reports what would change. It requires
// Options.EnableFileCheckpointing.
func (e *engine) RewindFiles(ctx context.Context, userMessageID string, dryRun bool) (map[string]any, error) {
	fields := map[string]any{"user_message_id": userMessageID}
	if dryRun {
		fields["dry_run"] = true
	}
	return e.request(ctx, "rewind_files", fields)
}

// MCPStatus reports the connection status of every configured MCP server.
func (e *engine) MCPStatus(ctx context.Context) (map[string]any, error) {
	return e.request(ctx, "mcp_status", nil)
}

// ContextUsage reports the context window usage breakdown. An empty detail
// uses the CLI default.
func (e *engine) ContextUsage(ctx context.Context, detail string) (map[string]any, error) {
	fields := map[string]any{}
	if detail != "" {
		fields["detail"] = detail
	}
	return e.request(ctx, "get_context_usage", fields)
}

// ReconnectMCPServer reconnects a disconnected or failed MCP server.
func (e *engine) ReconnectMCPServer(ctx context.Context, serverName string) error {
	_, err := e.request(ctx, "mcp_reconnect", map[string]any{"serverName": serverName})
	return err
}

// ToggleMCPServer enables or disables an MCP server.
func (e *engine) ToggleMCPServer(ctx context.Context, serverName string, enabled bool) error {
	_, err := e.request(ctx, "mcp_toggle", map[string]any{"serverName": serverName, "enabled": enabled})
	return err
}

// StopTask stops a running background task.
func (e *engine) StopTask(ctx context.Context, taskID string) error {
	_, err := e.request(ctx, "stop_task", map[string]any{"task_id": taskID})
	return err
}

// ---------------------------------------------------------------------------
// Incoming control requests
// ---------------------------------------------------------------------------

// spawnControlHandler answers one control request from the CLI in its own
// goroutine, so a slow callback cannot stall the read loop. A request whose ID
// is already being answered (a duplicate delivery) is skipped.
func (e *engine) spawnControlHandler(ctx context.Context, frame map[string]any) {
	requestID := str(frame["request_id"])
	handlerCtx, cancel := context.WithCancel(ctx)
	h := &inflightHandler{cancel: cancel}
	e.mu.Lock()
	if _, dup := e.inflight[requestID]; dup || e.isClosed() {
		// A closed engine has already cancelled its handlers (see Close).
		e.mu.Unlock()
		cancel()
		return
	}
	e.inflight[requestID] = h
	e.mu.Unlock()

	e.handlers.Add(1)
	go func() {
		defer e.handlers.Done()
		defer cancel()
		defer func() {
			e.mu.Lock()
			if e.inflight[requestID] == h {
				delete(e.inflight, requestID)
			}
			e.mu.Unlock()
		}()
		e.handleControlRequest(handlerCtx, requestID, frame)
	}()
}

func (e *engine) handleControlRequest(ctx context.Context, requestID string, frame map[string]any) {
	request, _ := frame["request"].(map[string]any)
	data, err := e.dispatchControlRequest(ctx, requestID, request)
	if ctx.Err() != nil {
		// The CLI cancelled the request and is no longer listening for a
		// reply.
		return
	}
	if errors.Is(err, errSuppressReply) || errors.Is(err, ErrRespondedOutOfBand) {
		return
	}
	var reply map[string]any
	if err != nil {
		reply = map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "error",
				"request_id": requestID,
				"error":      err.Error(),
			},
		}
	} else {
		if data == nil {
			data = map[string]any{}
		}
		reply = map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "success",
				"request_id": requestID,
				"response":   data,
			},
		}
	}
	payload, merr := json.Marshal(reply)
	if merr != nil {
		return
	}
	_ = e.transport.Write(ctx, payload)
}

// dispatchControlRequest runs the handler for one control request subtype.
func (e *engine) dispatchControlRequest(ctx context.Context, requestID string, request map[string]any) (map[string]any, error) {
	subtype := str(request["subtype"])
	if unansweredSubtypes[subtype] {
		return nil, errSuppressReply
	}
	switch subtype {
	case "can_use_tool":
		return e.handleCanUseTool(ctx, requestID, request)
	case "hook_callback":
		return e.handleHookCallback(ctx, request)
	case "mcp_message":
		return e.handleMCPMessage(ctx, request)
	case "elicitation":
		return e.handleElicitation(ctx, requestID, request)
	case "request_user_dialog":
		return e.handleUserDialog(ctx, requestID, request)
	default:
		return nil, fmt.Errorf("Unsupported control request subtype: %s", subtype)
	}
}

func (e *engine) handleCanUseTool(ctx context.Context, requestID string, request map[string]any) (data map[string]any, err error) {
	if e.opts.CanUseTool == nil {
		return nil, errors.New("canUseTool callback is not provided")
	}
	input, _ := request["input"].(map[string]any)
	permCtx := toolPermissionContext(requestID, request)

	// A panicking callback becomes an error response rather than taking the
	// process down.
	defer func() {
		if r := recover(); r != nil {
			data, err = nil, fmt.Errorf("can_use_tool callback panicked: %v", r)
		}
	}()

	result, err := e.opts.CanUseTool(ctx, str(request["tool_name"]), input, permCtx)
	if err != nil {
		return nil, err
	}
	switch r := result.(type) {
	case *PermissionResultAllow:
		updated := r.UpdatedInput
		if updated == nil {
			updated = input
		}
		out := map[string]any{"behavior": "allow", "updatedInput": updated}
		if r.UpdatedPermissions != nil {
			out["updatedPermissions"] = r.UpdatedPermissions
		}
		stampPermissionReply(out, request, r.DecisionClassification)
		return out, nil
	case *PermissionResultDeny:
		out := map[string]any{"behavior": "deny", "message": r.Message}
		if r.Interrupt {
			out["interrupt"] = true
		}
		stampPermissionReply(out, request, r.DecisionClassification)
		return out, nil
	default:
		return nil, fmt.Errorf("permission callback returned %T, want *PermissionResultAllow or *PermissionResultDeny", result)
	}
}

func (e *engine) handleHookCallback(ctx context.Context, request map[string]any) (data map[string]any, err error) {
	id := str(request["callback_id"])
	callback, ok := e.hookCallbacks[id]
	if !ok {
		return nil, fmt.Errorf("No hook callback found for ID: %s", id)
	}
	input, _ := request["input"].(map[string]any)

	defer func() {
		if r := recover(); r != nil {
			data, err = nil, fmt.Errorf("hook callback panicked: %v", r)
		}
	}()

	out, err := callback(ctx, input, str(request["tool_use_id"]), HookContext{Raw: input})
	if err != nil {
		return nil, err
	}
	return toWireMap(out, "hook output")
}

func (e *engine) handleMCPMessage(ctx context.Context, request map[string]any) (map[string]any, error) {
	serverName := str(request["server_name"])
	message, ok := request["message"]
	if serverName == "" || !ok || message == nil {
		return nil, errors.New("Missing server_name or message for MCP request")
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("claude: encoding mcp message: %w", err)
	}
	var id any
	if m, ok := message.(map[string]any); ok {
		id = m["id"]
	}
	if e.mcpServers == nil {
		return map[string]any{"mcp_response": jsonRPCError(id, jsonRPCMethodNotFound,
			fmt.Sprintf("Server '%s' not found", serverName))}, nil
	}
	response, err := e.mcpServers.route(ctx, serverName, raw)
	if err != nil {
		code := jsonRPCInternalError
		var notFound *mcpServerNotFoundError
		if errors.As(err, &notFound) {
			code = jsonRPCMethodNotFound
		}
		return map[string]any{"mcp_response": jsonRPCError(id, code, err.Error())}, nil
	}
	if response == nil {
		// A JSON-RPC notification or response gets no reply, but the
		// control request that carried it still expects an
		// acknowledgement; the TypeScript SDK sends this exact shape.
		return map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "result": map[string]any{}, "id": 0}}, nil
	}
	var decoded any
	if err := json.Unmarshal(response, &decoded); err != nil {
		return nil, fmt.Errorf("claude: decoding mcp response: %w", err)
	}
	return map[string]any{"mcp_response": decoded}, nil
}

func jsonRPCError(id any, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
}

// ---------------------------------------------------------------------------
// Input
// ---------------------------------------------------------------------------

// hasBidirectionalNeeds reports whether the CLI may still send control requests
// that need a reply, in which case the input stream must stay open.
func (e *engine) hasBidirectionalNeeds() bool {
	return e.opts.CanUseTool != nil || len(e.opts.Hooks) > 0 || e.mcpServers.hasServers() ||
		e.opts.OnElicitation != nil || e.opts.OnUserDialog != nil
}

// StreamInput writes each user-message frame and then ends the input stream.
func (e *engine) StreamInput(ctx context.Context, inputs iter.Seq[map[string]any]) error {
	written := 0
	var writeErr error
	for input := range inputs {
		if e.isClosed() {
			break
		}
		payload, err := json.Marshal(input)
		if err != nil {
			writeErr = fmt.Errorf("claude: encoding user message: %w", err)
			break
		}
		e.startTurn()
		if err := e.transport.Write(ctx, payload); err != nil {
			writeErr = err
			break
		}
		written++
	}
	if written == 0 {
		// Nothing was sent, so no result will arrive to release the hold.
		e.finalizeRun()
		if err := e.transport.EndInput(); err != nil && writeErr == nil {
			writeErr = err
		}
		return writeErr
	}
	if err := e.waitForResultAndEndInput(ctx); err != nil && writeErr == nil {
		writeErr = err
	}
	return writeErr
}

// waitForResultAndEndInput closes the input stream, first waiting for the run
// to end when the session still needs to answer control requests.
func (e *engine) waitForResultAndEndInput(ctx context.Context) error {
	if e.hasBidirectionalNeeds() {
		if err := e.waitForRunEnd(ctx); err != nil {
			return err
		}
	}
	e.finalizeRun()
	return e.transport.EndInput()
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (e *engine) isClosed() bool {
	select {
	case <-e.closed:
		return true
	default:
		return false
	}
}

// Close stops the engine: it releases anything blocked on the message stream,
// waits for in-flight control handlers, and closes the transport. With
// transcript mirroring enabled it first flushes pending entries to the
// SessionStore, waiting at most the batcher's close timeout in total.
func (e *engine) Close() error {
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
		e.finalizeRun()
		err = e.transport.Close()
		select {
		case <-e.readerDone:
		case <-time.After(5 * time.Second):
			// A transport whose Close leaves the reader blocked must not
			// wedge the caller; the goroutine ends when its stream does.
		}
		e.handlers.Wait()
		if mirror != nil {
			// Final flush of whatever the reader enqueued while stopping;
			// past the deadline in-flight appends are abandoned.
			mirror.close(mirrorCtx)
		}
	})
	return err
}
