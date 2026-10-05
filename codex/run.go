package codex

import (
	"context"
	"errors"
	"time"
)

// interruptTimeout bounds the turn/interrupt Run sends after its context ends.
const interruptTimeout = 5 * time.Second

// TurnResult is a finished turn collected from its stream, mirroring
// upstream's TurnResult.
type TurnResult struct {
	// Turn is the final turn reported by turn/completed: its id, status,
	// error, and timing.
	Turn *Turn
	// Items are the turn's completed items in completion order.
	Items []ThreadItem
	// Usage is the turn's last token usage update, or nil when none arrived.
	Usage *ThreadTokenUsage
}

// Text returns the turn's final response: the text of the last agent message
// in the final_answer phase, or else of the last agent message without a
// phase. It is empty when the turn produced neither. With an OutputSchema it
// is the JSON document.
func (r *TurnResult) Text() string {
	if r == nil {
		return ""
	}
	return finalResponse(r.Items)
}

// record captures the parts of an event that Result reports. The pump calls
// it before delivering the event, so Result sees every item even when the
// caller also reads Events.
func (s *TurnStream) record(event Event) {
	if event.Kind != EventItemCompleted && event.Kind != EventTokenUsageUpdated {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done.Ended() {
		return // Result may already hold s.items
	}
	if event.Kind == EventItemCompleted {
		s.items = append(s.items, *event.Item)
	} else {
		s.usage = event.Usage
	}
}

// Result waits for the turn to end and returns what it produced. It consumes
// the remaining events while it waits, so call it instead of iterating
// Events, or after breaking out of that loop.
//
// A failed turn returns both the result and its *TurnError. An interrupted
// turn returns the result and a nil error. A stream closed before its turn
// ended, or ended by a client shutdown, returns ErrClosed. When the context
// ends first, the turn keeps running; call Cancel to stop it, or use Run,
// which does. Repeated calls return the same Items slice, which callers must
// treat as read-only.
func (s *TurnStream) Result(ctx context.Context) (*TurnResult, error) {
	events, done := s.events, s.done.C()
	for done != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case _, ok := <-events:
			if !ok {
				events = nil
			}
		case <-done:
			done = nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.final == nil {
		return nil, s.endErrLocked()
	}
	// Nothing appends to s.items once the stream has ended. The capacity is
	// capped so a caller's append cannot write into a shared backing array.
	result := &TurnResult{Turn: s.final, Items: s.items[:len(s.items):len(s.items)], Usage: s.usage}
	if s.final.Status == TurnFailed {
		if s.final.Error != nil {
			return result, s.final.Error
		}
		return result, &TurnError{}
	}
	return result, nil
}

// finalResponse picks the assistant's answer the way upstream does: the last
// final_answer message, else the last message without a phase.
func finalResponse(items []ThreadItem) string {
	fallback, haveFallback := "", false
	for i := len(items) - 1; i >= 0; i-- {
		msg, ok := items[i].Item.(*AgentMessageItem)
		if !ok {
			continue
		}
		switch msg.Phase {
		case PhaseFinalAnswer:
			return msg.Text
		case "":
			if !haveFallback {
				fallback, haveFallback = msg.Text, true
			}
		}
	}
	return fallback
}

// Run starts a turn and waits for its result, like upstream's thread.run. If
// ctx ends before the turn does, Run asks the server to interrupt the turn
// and returns the context's error.
func (c *Client) Run(ctx context.Context, threadID string, input []InputItem, opts *TurnOptions) (*TurnResult, error) {
	stream, err := c.StartTurn(ctx, threadID, input, opts)
	if err != nil {
		return nil, err
	}
	return collect(ctx, stream)
}

// RunExternal is Run for an ExternalMessage; see StartExternalTurn.
func (c *Client) RunExternal(ctx context.Context, threadID string, msg ExternalMessage, opts *TurnOptions) (*TurnResult, error) {
	stream, err := c.StartExternalTurn(ctx, threadID, msg, opts)
	if err != nil {
		return nil, err
	}
	return collect(ctx, stream)
}

// collect waits for a stream's result, cancelling the turn when ctx ends.
func collect(ctx context.Context, stream *TurnStream) (*TurnResult, error) {
	result, err := stream.Result(ctx)
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), interruptTimeout)
		_ = stream.Cancel(cancelCtx)
		cancel()
		_ = stream.Close()
	}
	return result, err
}

// StartExternalTurn starts a turn whose input is untrusted external content
// rather than user input. The content has tool-level authority and never
// counts as user approval.
func (c *Client) StartExternalTurn(ctx context.Context, threadID string, msg ExternalMessage, opts *TurnOptions) (*TurnStream, error) {
	if msg.ToolName == "" {
		return nil, errors.New("codex: ExternalMessage.ToolName is required")
	}
	return c.startTurn(ctx, StartTurnParams{ThreadID: threadID, Input: []InputItem{}, ToolOutput: &msg}, opts)
}
