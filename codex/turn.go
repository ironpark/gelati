package codex

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"iter"
	"sync"

	"github.com/ironpark/gelati/internal/jsonx"
	"github.com/ironpark/gelati/internal/lifecycle"
)

// EventKind classifies a turn event.
type EventKind string

// Turn event kinds delivered on a TurnStream.
const (
	EventTurnStarted        EventKind = "turnStarted"
	EventTurnCompleted      EventKind = "turnCompleted"
	EventItemStarted        EventKind = "itemStarted"
	EventItemCompleted      EventKind = "itemCompleted"
	EventAgentMessageDelta  EventKind = "agentMessageDelta"
	EventPlanDelta          EventKind = "planDelta"
	EventReasoningDelta     EventKind = "reasoningDelta"
	EventCommandOutputDelta EventKind = "commandOutputDelta"
	EventPlanUpdated        EventKind = "planUpdated"
	EventDiffUpdated        EventKind = "diffUpdated"
	EventTokenUsageUpdated  EventKind = "tokenUsageUpdated"
	// EventError reports a turn error; WillRetry tells whether the turn
	// continues.
	EventError EventKind = "error"
	// EventNotification carries any other turn-scoped notification, such as
	// thread/compacted or model/rerouted, with only Method and Params set.
	EventNotification EventKind = "notification"
)

// Event is one streamed update for a turn.
type Event struct {
	// Kind classifies the event.
	Kind EventKind
	// Method is the originating notification method.
	Method string
	// ThreadID and TurnID identify the turn the event belongs to.
	ThreadID string
	TurnID   string

	// Turn is set for EventTurnStarted and EventTurnCompleted.
	Turn *Turn
	// Item is set for EventItemStarted and EventItemCompleted.
	Item *ThreadItem
	// ItemID identifies the item a delta appends to.
	ItemID string
	// Delta is the appended text for the delta events, including
	// EventCommandOutputDelta.
	Delta string
	// SummaryIndex increments when a new reasoning summary section opens.
	SummaryIndex int
	// Reasoning reports whether a reasoning delta is a summary or raw text.
	ReasoningSummary bool
	// Plan and Explanation are set for EventPlanUpdated.
	Plan        []PlanStep
	Explanation string
	// Diff is the aggregated unified diff for EventDiffUpdated.
	Diff string
	// Usage is set for EventTokenUsageUpdated. Total is cumulative for the
	// thread; see ThreadTokenUsage for what one update covers.
	Usage *ThreadTokenUsage
	// Error and WillRetry are set for EventError.
	Error     *TurnError
	WillRetry bool
	// Params is the raw notification payload.
	Params jsontext.Value
}

// TurnStream delivers a turn's events in arrival order. Iterate them with
// Events, or call Result to wait for the turn and collect what it produced.
//
// The stream ends once the turn reaches a terminal status, the caller calls
// Close, or the client shuts down. The rules shared by every gelati SDK:
//
//   - Close stops reading and never interrupts the turn. Close on a turn that
//     already ended is a no-op, and Result still returns its result. Events
//     and Result on a stream closed before its turn ended report ErrClosed.
//   - Cancel interrupts the turn while it is still running, also after Close.
//     Once the turn has ended it is a no-op that returns nil.
//   - Result returns the turn's result whenever one exists, together with the
//     error when the turn failed.
type TurnStream struct {
	client   *Client
	threadID string
	events   chan Event
	// done ends when the stream does, with ErrClosed after Close or a client
	// shutdown, and with nil when the turn completed.
	done lifecycle.Done

	mu     sync.Mutex
	turnID string
	closed bool              // Close ended the stream before the turn ended
	final  *Turn             // set by turn/completed, also after Close
	items  []ThreadItem      // completed items, for Result
	usage  *ThreadTokenUsage // latest usage update, for Result
}

// ThreadID returns the thread the turn belongs to.
func (s *TurnStream) ThreadID() string { return s.threadID }

// TurnID returns the turn id, which is empty until turn/start returns.
func (s *TurnStream) TurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnID
}

// Events iterates the turn's events in arrival order until the turn reaches
// a terminal status, the stream is closed, or the client shuts down. When the
// stream ends abnormally, or ctx ends first, the last pair carries the error
// (ErrClosed after Close or a client shutdown, or ctx's error) and a zero
// Event; a failed or interrupted turn ends normally with its
// EventTurnCompleted, and Result reports a failure's *TurnError.
//
// Breaking out of the loop leaves the stream open: call Result to finish
// collecting it, or Close to abandon it. Events and Result consume one queue,
// so do not read the same stream from two goroutines.
func (s *TurnStream) Events(ctx context.Context) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		done := s.done.C()
		for {
			select {
			case <-ctx.Done():
				yield(Event{}, ctx.Err())
				return
			case <-done:
				if err := s.endErr(); err != nil {
					yield(Event{}, err)
					return
				}
				// The turn completed: the pump closes the event channel
				// right after its buffered events.
				done = nil
			case event, ok := <-s.events:
				if !ok || s.isClosed() {
					if err := s.endErr(); err != nil {
						yield(Event{}, err)
					}
					return
				}
				if !yield(event, nil) {
					return
				}
			}
		}
	}
}

// isClosed reports whether Close ended the stream.
func (s *TurnStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// endErr returns the error the stream ended with, or nil.
func (s *TurnStream) endErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endErrLocked()
}

// endErrLocked is endErr with s.mu held.
func (s *TurnStream) endErrLocked() error {
	if s.closed {
		return ErrClosed
	}
	return s.done.Err()
}

// Done returns a channel closed when the stream ends for any reason.
func (s *TurnStream) Done() <-chan struct{} { return s.done.C() }

// Cancel asks the server to interrupt the turn (turn/interrupt). It does not
// close the stream: the turn then ends with status TurnInterrupted, which
// Events delivers as its EventTurnCompleted and Result returns with a nil
// error. Cancel also interrupts a turn whose stream was closed while it was
// still running. It returns nil once the turn has ended.
func (s *TurnStream) Cancel(ctx context.Context) error {
	s.mu.Lock()
	ended := s.final != nil || (s.done.Ended() && !s.closed)
	turnID := s.turnID
	s.mu.Unlock()
	if ended || s.client.Err() != nil {
		return nil // a shut-down client has no turn left to interrupt
	}
	return s.client.InterruptTurn(ctx, s.threadID, turnID)
}

// Close stops reading the stream without interrupting the turn, which keeps
// running on the server until it completes or is cancelled. Pending and
// future events for this turn are discarded, and Events and Result report
// ErrClosed. Close on a stream whose turn already ended does nothing. Close is
// idempotent and always returns nil.
func (s *TurnStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done.Ended() {
		s.closed = true
		s.done.Finish(ErrClosed)
	}
	return nil
}

// finish records the terminal state and releases everyone waiting. The first
// terminal state wins, except that a turn completing after Close still
// records its final turn, so Cancel knows there is nothing left to stop. The
// event channel is closed by the owning thread pump, the only goroutine that
// sends on it.
func (s *TurnStream) finish(final *Turn, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done.Ended() {
		s.final = final
		s.done.Finish(err)
	} else if s.closed && final != nil {
		s.final = final
	}
}

// deliver sends an event, waiting for the consumer. It returns false when the
// stream ended or the subscription closed, dropping the event.
func (s *TurnStream) deliver(event Event, quit <-chan struct{}) bool {
	select {
	case s.events <- event:
		return true
	case <-s.done.C():
		return false
	case <-quit:
		return false
	}
}

// queuedNotification is one turn notification waiting for its subscriber.
type queuedNotification struct {
	method   string
	params   jsontext.Value
	threadID string
	turnID   string
}

// newStream registers a pending turn stream on a thread subscription.
func (s *threadSubscription) newStream(c *Client, threadID string) *TurnStream {
	stream := &TurnStream{
		client:   c,
		threadID: threadID,
		events:   make(chan Event, c.eventBuffer()),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams = append(s.streams, stream)
	return stream
}

// bindTurnID assigns the id returned by turn/start to a pending stream.
func (s *threadSubscription) bindTurnID(stream *TurnStream, turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stream.mu.Lock()
	if stream.turnID == "" {
		stream.turnID = turnID
	}
	stream.mu.Unlock()
}

// endStream records the stream's terminal state, drops it, and closes its
// event channel. Only the pump calls it, since the pump is the only sender on
// that channel; other goroutines call finish. The channel is closed only by
// the call that removed the stream, so it is closed at most once.
func (s *threadSubscription) endStream(stream *TurnStream, final *Turn, err error) {
	stream.finish(final, err)
	if s.removeStream(stream) {
		close(stream.events)
	}
}

// removeStream drops a stream from the subscription, reporting whether it
// was there.
func (s *threadSubscription) removeStream(target *TurnStream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, stream := range s.streams {
		if stream == target {
			s.streams = append(s.streams[:index], s.streams[index+1:]...)
			return true
		}
	}
	return false
}

// streamFor resolves the stream a notification belongs to. A pending stream
// adopts the first turn id it sees, which covers events that race ahead of the
// turn/start response.
func (s *threadSubscription) streamFor(turnID string) *TurnStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	if turnID != "" {
		for _, stream := range s.streams {
			if stream.TurnID() == turnID {
				return stream
			}
		}
	}
	for _, stream := range s.streams {
		if stream.TurnID() == "" {
			if turnID != "" {
				stream.mu.Lock()
				stream.turnID = turnID
				stream.mu.Unlock()
			}
			return stream
		}
	}
	if len(s.streams) == 1 {
		return s.streams[0]
	}
	return nil
}

// activeStreams returns a snapshot of the subscription's streams.
func (s *threadSubscription) activeStreams() []*TurnStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*TurnStream(nil), s.streams...)
}

// enqueue hands a notification to the thread's pump. It never blocks the
// transport reader: when the queue is full the notification is dropped.
func (s *threadSubscription) enqueue(c *Client, note queuedNotification) {
	if s.quit.Ended() {
		return
	}
	select {
	case s.queue <- note:
	default:
		c.logger.Debug("codex: dropped turn notification", "threadId", s.id, "method", note.method)
	}
}

// pump fans notifications out to turn streams. One pump runs per subscribed
// thread, so a slow consumer stalls only its own thread.
func (s *threadSubscription) pump(c *Client) {
	defer func() {
		for _, stream := range s.activeStreams() {
			s.endStream(stream, nil, ErrClosed)
		}
	}()
	for {
		select {
		case <-s.quit.C():
			return
		case note := <-s.queue:
			c.deliverTurnEvent(s, note)
		}
	}
}

// deliverTurnEvent routes one queued notification to its stream.
func (c *Client) deliverTurnEvent(sub *threadSubscription, note queuedNotification) {
	stream := sub.streamFor(note.turnID)
	if stream == nil {
		c.logger.Debug("codex: event for unknown turn", "threadId", note.threadID,
			"turnId", note.turnID, "method", note.method)
		return
	}
	event, ok := buildEvent(note)
	if !ok {
		return
	}
	if event.ThreadID == "" {
		event.ThreadID = sub.id
	}
	if event.TurnID == "" {
		event.TurnID = stream.TurnID()
	}
	stream.record(event)

	// A closed stream stays registered, its events discarded, until its turn
	// completes, so Cancel still knows whether the turn is running.
	if !stream.isClosed() && !stream.deliver(event, sub.quit.C()) && sub.quit.Ended() {
		sub.endStream(stream, nil, ErrClosed)
		return
	}
	switch event.Kind {
	case EventTurnStarted:
		// A new turn clears prompts left over from the previous one.
		c.pending.cancelTurn(turnKey(event.ThreadID, event.TurnID))
	case EventTurnCompleted:
		// Pending approval prompts for this turn can no longer be answered.
		c.pending.cancelTurn(turnKey(event.ThreadID, event.TurnID))
		sub.endStream(stream, event.Turn, nil)
	}
}

// isTurnNotification reports whether a notification belongs on a turn
// stream: the modeled turn and item methods, and any other notification that
// names a turn, except the thread lifecycle ones.
func isTurnNotification(method, turnID string) bool {
	switch method {
	case MethodTurnStarted, MethodTurnCompleted, MethodTurnDiff, MethodTurnPlan,
		MethodItemStarted, MethodItemCompleted, MethodAgentMessageDelta,
		MethodPlanDelta, MethodReasoningSummaryTextDelta,
		MethodReasoningSummaryPartAdded, MethodReasoningTextDelta,
		MethodCommandExecutionOutputDelta, MethodTokenUsageUpdated, MethodError:
		return true
	}
	return turnID != "" && !isThreadMethod(method)
}

// buildEvent decodes a notification into a typed event.
func buildEvent(note queuedNotification) (Event, bool) {
	event := Event{
		Method:   note.method,
		ThreadID: note.threadID,
		TurnID:   note.turnID,
		Params:   note.params,
	}
	switch note.method {
	case MethodTurnStarted, MethodTurnCompleted:
		var payload TurnParams
		if err := jsonx.Unmarshal(note.params, &payload); err != nil {
			return event, false
		}
		event.Turn = &payload.Turn
		if event.TurnID == "" {
			event.TurnID = payload.Turn.ID
		}
		if note.method == MethodTurnStarted {
			event.Kind = EventTurnStarted
		} else {
			event.Kind = EventTurnCompleted
		}
	case MethodItemStarted, MethodItemCompleted:
		var payload ItemParams
		if err := jsonx.Unmarshal(note.params, &payload); err != nil {
			return event, false
		}
		item := payload.Item
		event.Item = &item
		event.ItemID = item.ID()
		if note.method == MethodItemStarted {
			event.Kind = EventItemStarted
		} else {
			event.Kind = EventItemCompleted
		}
	case MethodAgentMessageDelta, MethodPlanDelta, MethodReasoningTextDelta,
		MethodReasoningSummaryTextDelta, MethodReasoningSummaryPartAdded:
		var payload DeltaParams
		if err := jsonx.Unmarshal(note.params, &payload); err != nil {
			return event, false
		}
		event.ItemID = payload.ItemID
		event.Delta = payload.Delta
		event.SummaryIndex = payload.SummaryIndex
		switch note.method {
		case MethodAgentMessageDelta:
			event.Kind = EventAgentMessageDelta
		case MethodPlanDelta:
			event.Kind = EventPlanDelta
		default:
			event.Kind = EventReasoningDelta
			event.ReasoningSummary = note.method != MethodReasoningTextDelta
		}
	case MethodCommandExecutionOutputDelta:
		var payload CommandOutputDeltaParams
		if err := jsonx.Unmarshal(note.params, &payload); err != nil {
			return event, false
		}
		event.Kind = EventCommandOutputDelta
		event.ItemID = payload.ItemID
		event.Delta = payload.Delta
	case MethodTurnPlan:
		var payload TurnPlanParams
		if err := jsonx.Unmarshal(note.params, &payload); err != nil {
			return event, false
		}
		event.Kind = EventPlanUpdated
		event.Plan = payload.Plan
		event.Explanation = payload.Explanation
	case MethodTurnDiff:
		var payload TurnDiffParams
		if err := jsonx.Unmarshal(note.params, &payload); err != nil {
			return event, false
		}
		event.Kind = EventDiffUpdated
		event.Diff = payload.Diff
	case MethodTokenUsageUpdated:
		var payload TokenUsageParams
		if err := jsonx.Unmarshal(note.params, &payload); err != nil {
			return event, false
		}
		event.Kind = EventTokenUsageUpdated
		usage := payload.Usage
		event.Usage = &usage
	case MethodError:
		var payload ErrorParams
		if err := jsonx.Unmarshal(note.params, &payload); err != nil {
			return event, false
		}
		event.Kind = EventError
		event.Error = &payload.Error
		event.WillRetry = payload.WillRetry
	default:
		event.Kind = EventNotification
	}
	return event, true
}

// routeTurnNotification queues a turn or item notification for its thread
// pump. It reports whether the notification was a turn-scoped one.
func (c *Client) routeTurnNotification(method string, params jsontext.Value, threadID, turnID string) bool {
	if !isTurnNotification(method, turnID) {
		return false
	}
	sub := c.lookup(threadID)
	if sub == nil && threadID == "" {
		// Some notifications omit threadId. With a single subscribed thread
		// the target is unambiguous; otherwise the event is dropped.
		sub = c.soleSubscription()
	}
	if sub == nil {
		c.logger.Debug("codex: event for unknown thread", "threadId", threadID, "method", method)
		return true
	}
	sub.enqueue(c, queuedNotification{method: method, params: params, threadID: threadID, turnID: turnID})
	return true
}

// soleSubscription returns the only subscribed thread, if there is exactly
// one.
func (c *Client) soleSubscription() *threadSubscription {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.threads) != 1 {
		return nil
	}
	for _, sub := range c.threads {
		return sub
	}
	return nil
}

// StartTurn adds user input to a thread, begins Codex generation, and returns
// a stream of the turn's events. Read the stream to completion (Events or
// Result) or Close it.
func (c *Client) StartTurn(ctx context.Context, threadID string, input []InputItem, opts *TurnOptions) (*TurnStream, error) {
	return c.startTurn(ctx, StartTurnParams{ThreadID: threadID, Input: input}, opts)
}

// startTurn sends turn/start, with opts applied to params, after registering
// a stream so events that race ahead of the response are not lost.
func (c *Client) startTurn(ctx context.Context, params StartTurnParams, opts *TurnOptions) (*TurnStream, error) {
	if opts != nil {
		params.TurnOptions = *opts
	}
	threadID := params.ThreadID
	sub := c.subscribe(threadID)
	if sub == nil {
		return nil, errors.New("codex: StartTurn requires a thread id")
	}
	stream := sub.newStream(c, threadID)

	var result StartTurnResult
	if err := c.call(ctx, "turn/start", params, &result); err != nil {
		// Only the pump closes the event channel. Nobody holds this stream,
		// so dropping and finishing it is enough.
		sub.removeStream(stream)
		stream.finish(nil, err)
		return nil, err
	}
	sub.bindTurnID(stream, result.Turn.ID)
	return stream, nil
}

// SteerTurn appends user input to the active in-flight turn without starting a
// new one. expectedTurnID must match the active turn id.
func (c *Client) SteerTurn(ctx context.Context, threadID, expectedTurnID string, input []InputItem) (string, error) {
	params := SteerTurnParams{ThreadID: threadID, Input: input, ExpectedTurnID: expectedTurnID}
	var result SteerTurnResult
	if err := c.call(ctx, "turn/steer", params, &result); err != nil {
		return "", err
	}
	return result.TurnID, nil
}

// InterruptTurn requests cancellation of an in-flight turn. On success the
// turn ends with status "interrupted".
func (c *Client) InterruptTurn(ctx context.Context, threadID, turnID string) error {
	return c.call(ctx, "turn/interrupt", InterruptTurnParams{ThreadID: threadID, TurnID: turnID}, nil)
}

// CompactThread triggers manual history compaction. Progress streams as
// ordinary turn and item notifications.
func (c *Client) CompactThread(ctx context.Context, threadID string) error {
	return c.call(ctx, "thread/compact/start", ThreadIDParams{ThreadID: threadID}, nil)
}

// RunShellCommand runs a user-initiated shell command against a thread. It
// runs outside the sandbox with full access.
func (c *Client) RunShellCommand(ctx context.Context, threadID, command string) error {
	params := struct {
		ThreadID string `json:"threadId"`
		Command  string `json:"command"`
	}{ThreadID: threadID, Command: command}
	return c.call(ctx, "thread/shellCommand", params, nil)
}
