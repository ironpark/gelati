package codex

import (
	"context"

	"github.com/ironpark/gelati/internal/lifecycle"
)

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
func (t *Thread) Run(ctx context.Context, input ...InputItem) (*TurnResult, error) {
	return t.RunTurn(ctx, TurnRequest{Input: input})
}

// RunTurn is Run with per-turn options or an ExternalMessage; see SendTurn.
func (t *Thread) RunTurn(ctx context.Context, req TurnRequest) (*TurnResult, error) {
	stream, err := t.SendTurn(ctx, req)
	if err != nil {
		return nil, err
	}
	return collect(ctx, stream)
}

// collect waits for a stream's result, cancelling the turn when ctx ends.
func collect(ctx context.Context, stream *TurnStream) (*TurnResult, error) {
	result, err := stream.Result(ctx)
	if lifecycle.CancelIfDone(ctx, err, stream.Cancel) {
		_ = stream.Close()
	}
	return result, err
}
