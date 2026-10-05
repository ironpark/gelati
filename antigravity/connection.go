package antigravity

import (
	"cmp"
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"maps"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ironpark/gelati/antigravity/internal/harness"
	"github.com/ironpark/gelati/antigravity/internal/wire"
)

// transport carries wire events to and from the harness. *harness.Harness
// implements it; tests substitute an in-memory fake. Receive returning
// io.EOF is a clean end of stream.
type transport interface {
	Send(ctx context.Context, ev *wire.InputEvent) error
	Receive(ctx context.Context) (*wire.OutputEvent, error)
	Close() error
}

// Timeouts of Connection.Close.
const (
	// sessionEndTimeout bounds the wait for the harness to run the session
	// end hooks. Upstream waits indefinitely.
	sessionEndTimeout = time.Minute
	// backgroundDrainTimeout bounds the wait for in-flight tool calls and
	// hook handlers after their context is cancelled.
	backgroundDrainTimeout = 5 * time.Second
)

// stepKey identifies a step across its updates.
type stepKey struct {
	trajectoryID string
	index        uint32
}

// stepTracker tracks the state transitions of one step, so that wait
// requests are answered once per wait.
type stepTracker struct {
	state   wire.StepUpdateState
	handled map[string]bool
}

func (t *stepTracker) updateState(s wire.StepUpdateState) {
	if t.state == wire.StepUpdateStateWaitingForUser && s != wire.StepUpdateStateWaitingForUser {
		clear(t.handled)
	}
	t.state = s
}

func (t *stepTracker) markHandled(request string) bool {
	if t.handled[request] {
		return false
	}
	if t.handled == nil {
		t.handled = map[string]bool{}
	}
	t.handled[request] = true
	return true
}

// turn is the state of one turn: the steps queued for its reader, how it
// ended, and its hook context and step trackers. Send starts a turn; so
// does the harness starting one by itself (an automated trigger), seen as
// a PreTurn hook request or a RUNNING state while idle after the previous
// turn ended. Readers and ChatResponses keep their turn, so steps of a
// newer turn never reach them. The fields are guarded by Connection.mu.
type turn struct {
	queue []*Step
	wake  chan struct{} // 1-buffered; signalled when the queue grows or the turn ends
	// ended is set when the turn is over: the harness went idle, the turn
	// failed, a newer turn started or the event stream ended. Its reader
	// stops once the queue is drained.
	ended bool
	// err is the error the turn ended with, nil for a clean end. Steps that
	// arrive after it are dropped.
	err        error
	cancelled  bool
	stopReason StopReason
	startUsage UsageMetadata
	endUsage   *UsageMetadata // cumulative usage when a newer turn started
	finished   bool           // a finish step was seen
	structured any            // structured output of the last finish step
	hooks      *HookContext   // nil without hooks
	trackers   map[stepKey]*stepTracker
}

func (t *turn) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// push queues a step for the reader, unless the turn failed.
func (t *turn) push(step *Step) {
	if t.err != nil {
		return
	}
	t.queue = append(t.queue, step)
	t.signal()
}

// end ends the turn with err, or with a *CancelledError when the client
// cancelled it and err is nil. A turn ends once.
func (t *turn) end(err error) {
	if t.ended {
		return
	}
	if err == nil && t.cancelled {
		err = &CancelledError{}
	}
	t.ended, t.err = true, err
	t.signal()
}

func (t *turn) tracker(key stepKey) *stepTracker {
	tr := t.trackers[key]
	if tr == nil {
		if t.trackers == nil {
			t.trackers = map[stepKey]*stepTracker{}
		}
		tr = &stepTracker{}
		t.trackers[key] = tr
	}
	return tr
}

// Connection is a live session with the local harness (upstream
// LocalConnection). It sends prompts, streams the resulting steps, runs
// custom tools and hooks on the harness's behalf, and tracks idleness and
// token usage.
//
// Most programs use Agent and Conversation instead; a Connection bypasses
// the conversation history. Its methods are safe for concurrent use, but
// only one reader may consume steps at a time.
type Connection struct {
	tr             transport
	tools          *toolRunner
	hooks          *hookRunner
	router         *hookRouter
	dynamic        map[string]*Policy
	logger         *slog.Logger
	initialHistory []*Step
	sandbox        *SandboxStatus

	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup

	readerDone     chan struct{}
	sessionEndDone chan struct{}
	sessionEndOnce sync.Once
	closing        atomic.Bool
	closeOnce      sync.Once
	closeErr       error

	mu             sync.Mutex
	cur            *turn
	idle           bool
	idleCh         chan struct{} // closed while idle
	conversationID string
	cumulative     UsageMetadata
	trajUsages     map[string]UsageMetadata
	receiving      bool
	// streamErr is set once the event stream ended; turns started later
	// end with it at once.
	streamErr error
}

type connectionOptions struct {
	tools          *toolRunner
	hooks          *hookRunner
	dynamic        map[string]*Policy
	logger         *slog.Logger
	conversationID string
	initialHistory []*Step
	initialUsage   *UsageMetadata
	trajUsages     map[string]UsageMetadata
	sandbox        *SandboxStatus
}

// newConnection wraps an initialized transport and starts reading from it.
// The connection starts idle.
func newConnection(tr transport, o connectionOptions) *Connection {
	if o.logger == nil {
		o.logger = defaultLogger()
	}
	c := &Connection{
		tr:             tr,
		tools:          o.tools,
		hooks:          o.hooks,
		dynamic:        o.dynamic,
		logger:         o.logger,
		initialHistory: o.initialHistory,
		sandbox:        o.sandbox,
		readerDone:     make(chan struct{}),
		sessionEndDone: make(chan struct{}),
		idle:           true,
		idleCh:         make(chan struct{}),
		conversationID: o.conversationID,
		trajUsages:     maps.Clone(o.trajUsages),
	}
	if c.trajUsages == nil {
		c.trajUsages = map[string]UsageMetadata{}
	}
	if o.initialUsage != nil {
		c.cumulative = o.initialUsage.Clone()
	}
	close(c.idleCh)
	c.startTurnLocked().end(nil)
	c.bgCtx, c.bgCancel = context.WithCancel(context.Background())
	if c.hooks != nil {
		c.router = &hookRouter{hooks: c.hooks}
	}
	go c.readLoop()
	return c
}

// InitialHistory returns the steps of a resumed session, restored during
// the handshake.
func (c *Connection) InitialHistory() []*Step { return append([]*Step(nil), c.initialHistory...) }

// IsIdle reports whether the agent is idle and ready for input.
func (c *Connection) IsIdle() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.idle
}

// ConversationID returns the conversation identifier (the main trajectory
// ID), as assigned at the handshake.
func (c *Connection) ConversationID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conversationID
}

// CumulativeUsage returns the session's total token usage as last reported
// by the harness.
func (c *Connection) CumulativeUsage() UsageMetadata {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cumulative.Clone()
}

// TrajectoryUsages returns the cumulative token usage per trajectory (the
// main agent and each subagent run).
func (c *Connection) TrajectoryUsages() map[string]UsageMetadata {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]UsageMetadata, len(c.trajUsages))
	for k, v := range c.trajUsages {
		out[k] = v.Clone()
	}
	return out
}

// SandboxStatus returns the OS command sandbox status reported at the
// handshake, or nil when the harness did not report one.
func (c *Connection) SandboxStatus() *SandboxStatus {
	if c.sandbox == nil {
		return nil
	}
	s := *c.sandbox
	return &s
}

// LastTurnStopReason returns why the most recent turn stopped.
func (c *Connection) LastTurnStopReason() StopReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur.stopReason
}

// turnStopReason returns why t stopped.
func (c *Connection) turnStopReason(t *turn) StopReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return t.stopReason
}

// turnUsage returns the token usage of t so far.
func (c *Connection) turnUsage(t *turn) UsageMetadata {
	c.mu.Lock()
	defer c.mu.Unlock()
	end := &c.cumulative
	if t.endUsage != nil {
		end = t.endUsage
	}
	return end.Sub(t.startUsage)
}

// turnStructuredOutput returns the structured output of t's last finish
// step; ok is false when t had none.
func (c *Connection) turnStructuredOutput(t *turn) (out any, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return t.structured, t.finished
}

// startTurnLocked ends the current turn and installs a new one.
func (c *Connection) startTurnLocked() *turn {
	if old := c.cur; old != nil {
		old.end(nil)
		u := c.cumulative.Clone()
		old.endUsage = &u
	}
	t := &turn{wake: make(chan struct{}, 1), stopReason: StopReasonUnspecified, startUsage: c.cumulative.Clone()}
	if c.hooks != nil {
		t.hooks = c.hooks.newTurnContext()
	}
	if c.streamErr != nil {
		t.end(c.streamErr)
	}
	c.cur = t
	return t
}

// Send starts a turn with the given prompt; no content sends an empty
// prompt. It starts a new turn (ending the previous one for its reader)
// but does not wait for the agent: read the turn with ReceiveSteps.
func (c *Connection) Send(ctx context.Context, content ...Content) error {
	_, err := c.sendTurn(ctx, content)
	return err
}

// sendTurn sends a prompt and returns the turn it started.
func (c *Connection) sendTurn(ctx context.Context, content []Content) (*turn, error) {
	parts, err := userInputParts(content)
	if err != nil {
		return nil, err
	}
	if c.closing.Load() {
		return nil, ErrClosed
	}
	c.mu.Lock()
	c.setBusyLocked()
	t := c.startTurnLocked()
	c.mu.Unlock()
	return t, c.send(ctx, &wire.InputEvent{UserInput: &wire.UserInput{Parts: parts}})
}

// SendTriggerNotification injects an automated trigger message, also while
// a turn is running.
func (c *Connection) SendTriggerNotification(ctx context.Context, content string) error {
	return c.send(ctx, &wire.InputEvent{AutomatedTrigger: new(content)})
}

// Cancel asks the harness to halt the current turn. The turn's step stream
// then ends with a *CancelledError.
func (c *Connection) Cancel(ctx context.Context) error { return c.cancel(ctx, nil) }

// cancel halts turn t (nil for the current turn); it is a no-op when t is
// no longer the current turn.
func (c *Connection) cancel(ctx context.Context, t *turn) error {
	c.mu.Lock()
	if t == nil {
		t = c.cur
	}
	if t != c.cur {
		c.mu.Unlock()
		return nil
	}
	t.cancelled = true
	if t.ended && t.err == nil {
		t.err = &CancelledError{}
	}
	c.mu.Unlock()
	return c.send(ctx, &wire.InputEvent{HaltRequest: new(true)})
}

func (c *Connection) send(ctx context.Context, ev *wire.InputEvent) error {
	if err := c.tr.Send(ctx, ev); err != nil {
		if errors.Is(err, harness.ErrClosed) {
			return ErrClosed
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return connectionErrorFrom(err)
	}
	return nil
}

// WaitForIdle blocks until the agent is idle, then discards queued steps.
// It fails when the harness connection is lost first.
func (c *Connection) WaitForIdle(ctx context.Context) error {
	c.mu.Lock()
	ch := c.idleCh
	c.mu.Unlock()
	select {
	case <-ch:
	case <-c.readerDone:
		if !c.IsIdle() {
			return &ConnectionError{Message: "antigravity: the harness connection closed before the agent went idle"}
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	c.mu.Lock()
	if c.idle {
		c.cur.queue = nil
	}
	c.mu.Unlock()
	return nil
}

// WaitForWakeup reports whether the connection woke up (for example, a
// scheduled task fired) within timeout. The local harness does not report
// wakeups, so, as upstream, it returns false at once.
func (c *Connection) WaitForWakeup(ctx context.Context, timeout time.Duration) (bool, error) {
	return false, ctx.Err()
}

// ReceiveSteps yields the steps of the current turn as they arrive, until
// the agent goes idle. A fatal error ends the sequence: *ExecutionError when
// the turn failed, *ConnectionError for fatal system errors (HTTP 400, 401,
// 403) or a harness crash, *CancelledError after Cancel. Only one reader
// may consume steps at a time; a second one gets ErrConcurrentReceive.
func (c *Connection) ReceiveSteps(ctx context.Context) iter.Seq2[*Step, error] {
	return c.receiveSteps(ctx, nil)
}

// receiveSteps implements ReceiveSteps; record, when set, sees each step
// before it is yielded.
func (c *Connection) receiveSteps(ctx context.Context, record func(*Step)) iter.Seq2[*Step, error] {
	return func(yield func(*Step, error) bool) {
		r, err := c.newReceiver(nil)
		if err != nil {
			yield(nil, err)
			return
		}
		defer r.release()
		for {
			step, err := r.next(ctx)
			if err != nil {
				yield(nil, err)
				return
			}
			if step == nil {
				return
			}
			if record != nil {
				record(step)
			}
			if !yield(step, nil) {
				return
			}
		}
	}
}

// stepReceiver is the single active consumer of a turn's steps.
type stepReceiver struct {
	c        *Connection
	t        *turn
	released bool
}

// newReceiver takes the reader slot to read t, or the current turn when t
// is nil.
func (c *Connection) newReceiver(t *turn) (*stepReceiver, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.receiving {
		return nil, ErrConcurrentReceive
	}
	c.receiving = true
	if t == nil {
		t = c.cur
	}
	return &stepReceiver{c: c, t: t}, nil
}

// release gives up the reader slot; it is idempotent.
func (r *stepReceiver) release() {
	if r.released {
		return
	}
	r.released = true
	r.c.mu.Lock()
	r.c.receiving = false
	r.c.mu.Unlock()
}

// next returns the next step, nil at the end of the turn, or the error that
// ended it. Reaching the end releases the reader slot; a context error
// leaves the receiver usable.
func (r *stepReceiver) next(ctx context.Context) (*Step, error) {
	c, t := r.c, r.t
	c.mu.Lock()
	for len(t.queue) == 0 {
		if t.ended {
			err := t.err
			c.mu.Unlock()
			r.release()
			return nil, err
		}
		c.mu.Unlock()
		select {
		case <-t.wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.mu.Lock()
	}
	step := t.queue[0]
	t.queue[0] = nil
	t.queue = t.queue[1:]
	c.mu.Unlock()
	return step, nil
}

func (c *Connection) setIdleLocked() {
	if !c.idle {
		c.idle = true
		close(c.idleCh)
	}
}

func (c *Connection) setBusyLocked() {
	if c.idle {
		c.idle = false
		c.idleCh = make(chan struct{})
	}
}

// readLoop reads harness events until the stream ends, then ends the
// current turn.
func (c *Connection) readLoop() {
	defer close(c.readerDone)
	var streamErr error
	defer func() {
		c.mu.Lock()
		c.cur.end(streamErr)
		c.streamErr = streamErr
		if c.streamErr == nil {
			c.streamErr = &ConnectionError{Message: "antigravity: the harness connection is closed"}
		}
		c.mu.Unlock()
	}()
	for {
		// The reader runs until the transport is closed; cancelling a
		// read mid-message would tear the WebSocket down.
		ev, err := c.tr.Receive(context.Background())
		if err != nil {
			switch {
			case c.closing.Load(), errors.Is(err, harness.ErrClosed):
				c.logger.Info("harness connection closed")
			case errors.Is(err, io.EOF):
			default:
				streamErr = connectionErrorFrom(err)
				c.logger.Error("harness connection lost", "error", streamErr)
			}
			return
		}
		c.processEvent(ev)
	}
}

func (c *Connection) readerFinished() bool {
	select {
	case <-c.readerDone:
		return true
	default:
		return false
	}
}

// goBackground runs fn on its own goroutine with the connection's
// background context.
func (c *Connection) goBackground(fn func(ctx context.Context)) {
	c.bg.Go(func() { fn(c.bgCtx) })
}

// processEvent handles one harness event: steps are queued for the reader,
// requests are answered on background goroutines.
func (c *Connection) processEvent(ev *wire.OutputEvent) {
	switch {
	case ev.PolicyDecisionRequest != nil:
		req := ev.PolicyDecisionRequest
		c.goBackground(func(ctx context.Context) {
			resp := decidePolicy(ctx, c.logger, c.dynamic, req)
			if err := c.send(ctx, &wire.InputEvent{PolicyDecisionResponse: resp}); err != nil {
				c.logger.Warn("send policy decision", "error", err)
			}
		})
	case ev.CallHookRequest != nil:
		req := ev.CallHookRequest
		c.logger.Debug("hook request", "type", req.GetType())
		var turnCtx *HookContext
		if c.router != nil {
			c.mu.Lock()
			// A PreTurn request while idle opens a turn the harness started
			// by itself; the harness reports RUNNING only after it.
			if req.GetType() == wire.LifecycleHookPreTurn && c.idle && c.cur.ended {
				c.startTurnLocked()
			}
			turnCtx = c.cur.hooks
			c.mu.Unlock()
		}
		c.goBackground(func(ctx context.Context) {
			var resp *wire.CallHookResponse
			if c.router != nil {
				resp = c.router.handle(ctx, turnCtx, req, c.logger)
			} else {
				resp = &wire.CallHookResponse{RequestID: new(req.GetRequestID()), EmptyResult: &wire.EmptyResult{}}
			}
			if err := c.send(ctx, &wire.InputEvent{CallHookResponse: resp}); err != nil {
				c.logger.Warn("send hook response", "error", err)
			}
		})
	case ev.SessionEndResponse != nil:
		c.logger.Debug("session end response")
		c.sessionEndOnce.Do(func() { close(c.sessionEndDone) })
	case ev.StepUpdate != nil:
		c.processStepUpdate(ev.StepUpdate)
	case ev.UsageUpdate != nil:
		c.mu.Lock()
		if t := ev.UsageUpdate.GetTotal(); t != nil {
			c.cumulative = parseUsage(t)
		}
		for _, e := range ev.UsageUpdate.GetAgents() {
			if e.GetTrajectoryID() != "" && e.GetUsage() != nil {
				c.trajUsages[e.GetTrajectoryID()] = parseUsage(e.GetUsage())
			}
		}
		c.mu.Unlock()
	case ev.TrajectoryStateUpdate != nil:
		c.processTrajectoryState(ev.TrajectoryStateUpdate)
	case ev.ToolCall != nil:
		tc := ev.ToolCall
		c.goBackground(func(ctx context.Context) { c.handleToolCall(ctx, tc) })
	}
}

// fatalSystemError returns the error a failed system step ends its turn
// with: a *ConnectionError for HTTP 400, 401 and 403, nil otherwise.
func fatalSystemError(step *Step) error {
	if step.Status != StepStatusError || step.Source != StepSourceSystem {
		return nil
	}
	switch step.HTTPCode {
	case 400, 401, 403:
		return &ConnectionError{Message: cmp.Or(step.Error, "System error occurred.")}
	}
	return nil
}

func (c *Connection) processStepUpdate(su *wire.StepUpdate) {
	key := stepKey{su.GetTrajectoryID(), su.GetStepIndex()}
	step := stepFromUpdate(su)

	queued := step
	if c.tools != nil && len(step.ToolCalls) > 0 {
		for _, tc := range step.ToolCalls {
			if c.tools.has(tc.Name) {
				// A local custom tool also arrives as a separate tool_call
				// event, which queues its own step; drop the call here to
				// avoid reporting it twice. (History restored at handshake
				// keeps it, since no tool_call events are replayed.)
				queued = step.clone()
				queued.ToolCalls = nil
				break
			}
		}
	}
	fatal := fatalSystemError(step)
	if fatal == nil && step.Status == StepStatusError && step.Source == StepSourceSystem {
		c.logger.Warn("system step error", "http_code", step.HTTPCode, "error", step.Error)
	}

	c.mu.Lock()
	t := c.cur
	if c.conversationID == "" && su.GetTrajectoryID() != "" && su.GetParentTrajectoryID() == "" {
		c.conversationID = su.GetTrajectoryID()
	}
	tracker := t.tracker(key)
	tracker.updateState(su.GetState())
	t.push(queued)
	if step.Type == StepTypeFinish {
		t.finished, t.structured = true, step.StructuredOutput
	}
	if fatal != nil {
		t.end(fatal)
	}
	var handleQuestions, handleConfirmation bool
	if su.GetState() == wire.StepUpdateStateWaitingForUser {
		handleQuestions = su.QuestionsRequest != nil && tracker.markHandled("questions_request")
		handleConfirmation = su.ToolConfirmationRequest != nil && tracker.markHandled("tool_confirmation_request")
	}
	turnCtx := t.hooks
	c.mu.Unlock()

	if handleQuestions {
		c.goBackground(func(ctx context.Context) { c.handleQuestions(ctx, turnCtx, su) })
	}
	if handleConfirmation {
		// Pre-tool gating happens through the PreTool hook; the legacy
		// confirmation request is always accepted.
		c.goBackground(func(ctx context.Context) {
			ev := &wire.InputEvent{ToolConfirmation: &wire.ToolConfirmation{
				TrajectoryID: new(su.GetTrajectoryID()),
				StepIndex:    new(su.GetStepIndex()),
				Accepted:     new(true),
			}}
			if err := c.send(ctx, ev); err != nil {
				c.logger.Warn("send tool confirmation", "error", err)
			}
		})
	}
}

func (c *Connection) processTrajectoryState(tsu *wire.TrajectoryStateUpdate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conversationID != "" && tsu.GetTrajectoryID() != c.conversationID {
		// Subagents are coordinated by the harness; only the main
		// trajectory's state matters here.
		if tsu.Error != nil {
			c.logger.Info("subagent trajectory failed", "error", tsu.GetError())
		}
		return
	}
	t := c.cur
	switch tsu.GetState() {
	case wire.TrajectoryStateUpdateStateRunning:
		if c.idle && t.ended {
			// The harness started a turn by itself.
			t = c.startTurnLocked()
		}
		c.setBusyLocked()
	case wire.TrajectoryStateUpdateStateFullyIdle:
		var err error
		if tsu.Error != nil {
			err = &ExecutionError{Message: tsu.GetError()}
		}
		t.end(err)
		c.setIdleLocked()
	case wire.TrajectoryStateUpdateStateCancelled:
		msg := "Turn cancelled"
		if tsu.Error != nil {
			msg = tsu.GetError()
		}
		t.end(&ExecutionError{Message: msg})
		c.setIdleLocked()
	}
	if r := tsu.GetStopReason(); r != "" && r != wire.TrajectoryStateUpdateStopReasonUnspecified {
		t.stopReason = parseStopReason(r)
	}
}

// handleToolCall runs a custom tool the harness asks for and sends the
// result back. A step describing the call is queued for the reader first.
func (c *Connection) handleToolCall(ctx context.Context, tc *wire.ToolCall) {
	// Unlike step and hook tool calls, the arguments reach the tool as the
	// model wrote them, as upstream passes them on.
	call := &ToolCall{ID: tc.GetID(), Name: tc.GetName(), Args: toolCallArgs(tc)}
	step := &Step{
		ID:           tc.GetID(),
		StepIndex:    1,
		TrajectoryID: tc.GetTrajectoryID(),
		Type:         StepTypeToolCall,
		Source:       StepSourceModel,
		Target:       StepTargetEnvironment,
		Status:       StepStatusActive,
		ToolCalls:    []*ToolCall{call.clone()},
	}
	c.mu.Lock()
	c.cur.push(step)
	c.mu.Unlock()
	if c.tools == nil {
		c.logger.Warn("received a tool call but no tool runner is configured", "tool", tc.GetName())
		return
	}
	result := c.tools.processToolCalls(ctx, []*ToolCall{call})[0]
	resp, err := toolResponse(result)
	if err != nil {
		c.logger.Error("build tool response", "tool", tc.GetName(), "error", err)
		resp = &wire.ToolResponse{ID: new(tc.GetID()), ErrorMessage: new("Internal SDK error: " + err.Error())}
	}
	if err := c.send(ctx, &wire.InputEvent{ToolResponse: resp}); err != nil {
		c.logger.Warn("send tool response", "tool", tc.GetName(), "error", err)
	}
}

// handleQuestions answers an ask_question request through the interaction
// hooks.
func (c *Connection) handleQuestions(ctx context.Context, turn *HookContext, su *wire.StepUpdate) {
	questions := su.GetQuestionsRequest().GetQuestions()
	answers, err := c.answerQuestions(ctx, turn, questions)
	if err != nil {
		c.logger.Error("answer questions", "error", err)
		answers = make([]*wire.UserQuestionAnswer, len(questions))
		for i := range answers {
			answers[i] = &wire.UserQuestionAnswer{MultipleChoiceAnswer: &wire.MultipleChoiceAnswer{
				FreeformResponse: new("SDK error processing question: " + err.Error()),
			}}
		}
	}
	ev := &wire.InputEvent{QuestionResponse: &wire.UserQuestionsResponse{
		TrajectoryID: new(su.GetTrajectoryID()),
		StepIndex:    new(su.GetStepIndex()),
		Response:     &wire.UserQuestionsResponseQuestionsResponse{Answers: answers},
	}}
	if err := c.send(ctx, ev); err != nil {
		c.logger.Warn("send question response", "error", err)
	}
}

func (c *Connection) answerQuestions(ctx context.Context, turn *HookContext, questions []*wire.UserQuestion) ([]*wire.UserQuestionAnswer, error) {
	answers := make([]*wire.UserQuestionAnswer, len(questions))
	for i := range answers {
		answers[i] = &wire.UserQuestionAnswer{Unanswered: new(true)}
	}
	var spec AskQuestionInteractionSpec
	var indices []int
	for i, q := range questions {
		mc := q.GetMultipleChoice()
		if mc == nil {
			continue
		}
		entry := AskQuestionEntry{Question: mc.GetQuestion(), IsMultiSelect: mc.GetIsMultiSelect()}
		for j, choice := range mc.GetChoices() {
			entry.Options = append(entry.Options, AskQuestionOption{ID: strconv.Itoa(j + 1), Text: choice})
		}
		spec.Questions = append(spec.Questions, entry)
		indices = append(indices, i)
	}
	switch {
	case c.hooks != nil && len(spec.Questions) > 0:
		res, err := c.hooks.dispatchInteraction(ctx, turn, spec)
		if err != nil {
			return nil, err
		}
		if res == nil {
			break
		}
		for k, r := range res.Responses {
			if k >= len(indices) {
				break
			}
			if r.Skipped {
				continue
			}
			mc := &wire.MultipleChoiceAnswer{}
			for _, id := range r.SelectedOptionIDs {
				if n, err := strconv.Atoi(id); err == nil {
					mc.SelectedChoiceIndices = append(mc.SelectedChoiceIndices, int32(n-1))
				}
			}
			if r.FreeformResponse != "" {
				mc.FreeformResponse = new(r.FreeformResponse)
			}
			answers[indices[k]] = &wire.UserQuestionAnswer{MultipleChoiceAnswer: mc}
		}
	case len(spec.Questions) == 0 && len(questions) > 0:
		c.logger.Warn("received a question request with no multiple-choice questions; skipping all")
	case c.hooks == nil:
		c.logger.Warn("received a question request but no hooks are configured; skipping")
	}
	return answers, nil
}

// Close ends the session. When session end hooks are registered and the
// harness is still connected, it first asks the harness to run them and
// waits for it to finish; then it stops
// in-flight tool calls and hook handlers, closes the WebSocket and stdin,
// and waits for the harness to exit (terminating it after a grace period).
// It returns an error when the session end request could not be completed.
// Close is idempotent.
func (c *Connection) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.close() })
	return c.closeErr
}

func (c *Connection) close() error {
	// Set first, so that the reader treats the harness hanging up after the
	// session end handshake as expected.
	c.closing.Store(true)
	var hookErr error
	if c.hooks != nil && c.hooks.has(hookSessionEnd) && !c.readerFinished() {
		c.logger.Debug("requesting session end")
		ctx, cancel := context.WithTimeout(context.Background(), sessionEndTimeout)
		if err := c.send(ctx, &wire.InputEvent{SessionEndRequest: new(true)}); err != nil {
			hookErr = err
		} else {
			select {
			case <-c.sessionEndDone:
			case <-c.readerDone:
			case <-ctx.Done():
				hookErr = &ConnectionError{Message: "timed out waiting for the session end hooks"}
			}
		}
		cancel()
	}
	c.bgCancel()
	drained := make(chan struct{})
	go func() {
		c.bg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(backgroundDrainTimeout):
		c.logger.Warn("tool calls or hooks still running after close")
	}
	closeErr := c.tr.Close()
	<-c.readerDone
	if hookErr != nil {
		return hookErr
	}
	if closeErr != nil {
		return connectionErrorFrom(closeErr)
	}
	return nil
}
