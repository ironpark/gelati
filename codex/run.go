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
	// FinalResponse is the text of the last agent message in the
	// final_answer phase, or else of the last agent message without a phase.
	// It is empty when the turn produced neither. With an OutputSchema it is
	// the JSON document.
	FinalResponse string
	// Usage is the turn's last token usage update, or nil when none arrived.
	Usage *ThreadTokenUsage
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
	if event.Kind == EventItemCompleted {
		s.items = append(s.items, *event.Item)
	} else {
		s.usage = event.Usage
	}
}

// Result waits for the turn to end and returns what it produced. It drains
// the event channel while it waits, so call it instead of reading Events, or
// after reading as many events as you need.
//
// A failed turn returns both the result and its *TurnError. An interrupted
// turn returns the result and a nil error. When the context ends first, the
// turn keeps running; Run interrupts it instead.
func (s *TurnStream) Result(ctx context.Context) (*TurnResult, error) {
	events := s.events
	for done := false; !done; {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case _, ok := <-events:
			if !ok {
				events = nil
			}
		case <-s.done:
			done = true
		}
	}
	final, err := s.Wait(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	result := &TurnResult{
		Turn:  final,
		Items: append([]ThreadItem(nil), s.items...),
		Usage: s.usage,
	}
	s.mu.Unlock()
	result.FinalResponse = finalResponse(result.Items)
	if final != nil && final.Status == TurnFailed {
		if final.Error != nil {
			return result, final.Error
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
	return c.collect(ctx, stream)
}

// RunExternal is Run for an ExternalMessage; see StartExternalTurn.
func (c *Client) RunExternal(ctx context.Context, threadID string, msg ExternalMessage, opts *TurnOptions) (*TurnResult, error) {
	stream, err := c.StartExternalTurn(ctx, threadID, msg, opts)
	if err != nil {
		return nil, err
	}
	return c.collect(ctx, stream)
}

// collect waits for a stream's result, interrupting the turn when ctx ends.
func (c *Client) collect(ctx context.Context, stream *TurnStream) (*TurnResult, error) {
	result, err := stream.Result(ctx)
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		interruptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), interruptTimeout)
		_ = c.InterruptTurn(interruptCtx, stream.ThreadID(), stream.TurnID())
		cancel()
		stream.Close()
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
