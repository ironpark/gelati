package claude

import (
	"context"
	"iter"
	"sync/atomic"

	"github.com/ironpark/gelati/internal/lifecycle"
)

// TurnStream is one turn of a Client conversation, returned by Client.Send:
// the messages the CLI produces for it, up to and including its
// ResultMessage. Events yields those messages and Text the assistant text
// they carry; both read the same stream, so use one of them per turn.
//
// The client has a single message stream, which its turns share in the order
// they were sent: reading a turn first reads the rest of any earlier turn,
// whose unread messages are skipped (its Result stays available). Read one
// TurnStream at a time, from one goroutine; Cancel and Close may be called
// from any goroutine.
//
// The turn streams of every SDK in this module follow the same rules:
//
//   - Close stops reading and never interrupts the turn. On a turn that has
//     already ended it is a no-op: Result still returns the turn's result.
//     On a turn still running, Events, Text and Result then fail with an
//     error matching ErrClosed.
//   - Cancel interrupts the turn while it runs, also after Close. Once the
//     turn has ended it is a no-op returning nil.
//   - Result returns the turn's result whenever there is one, together with
//     the error when the turn failed.
type TurnStream struct {
	client *Client

	// end records that the turn has ended and the stream error it ended
	// with, if any.
	end lifecycle.Done
	// result is the turn's ResultMessage, nil when it ended without one.
	// It is set before end is finished and read only after.
	result *ResultMessage
	// closed is set by Close.
	closed atomic.Bool
	// streamed holds the IDs of the API messages whose text Text took from
	// stream events, so that it skips their AssistantMessages. Only the
	// reader of the turn uses it.
	streamed map[string]bool
}

// Events yields the turn's messages up to and including its ResultMessage. A
// fatal error, such as a *ProcessError for a CLI that exited, is the final
// item, as is the error of a cancelled ctx.
//
// Breaking out of the loop leaves the rest of the turn unread: ranging over
// Events again resumes where it stopped, and Result reads it to the end.
// Once the turn has ended, Events yields only the error it ended with, if
// any. After Close on a turn that has not ended it yields an error matching
// ErrClosed.
func (t *TurnStream) Events(ctx context.Context) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		if err := t.read(ctx, func(m Message) bool { return yield(m, nil) }); err != nil {
			yield(nil, err)
		}
	}
}

// Text yields the turn's assistant text as it arrives, for the main
// conversation only: subagent output, the messages with a ParentToolUseID,
// is skipped. With Options.IncludePartialMessages it yields the text deltas
// of the StreamEvents, and skips the text of the AssistantMessages that
// repeat them; an AssistantMessage that was not streamed, such as one the
// CLI synthesizes, still contributes its text. Otherwise it yields the text
// of each TextBlock of each AssistantMessage.
//
// Text reads the same stream as Events, so use one or the other per turn.
// When the turn fails, the final item is the error Result returns: a
// *ResultError for an error result, a *ConnectionError for a stream that
// ended without one, and otherwise the error Events ends with, such as that
// of a cancelled ctx or ErrClosed. Breaking out of the loop leaves the rest
// of the turn unread, as with Events; Result still returns the turn's result
// afterwards.
func (t *TurnStream) Text(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		stopped := false
		err := t.read(ctx, func(m Message) bool {
			if !t.emitText(m, func(text string) bool { return yield(text, nil) }) {
				stopped = true
				return false
			}
			return true
		})
		if stopped {
			return
		}
		if err == nil {
			// The turn has ended; an error result or a missing one is
			// reported as Result does.
			_, err = checkResult(t.result)
		}
		if err != nil {
			yield("", err)
		}
	}
}

// emitText passes the main-conversation text of m to yield, as Text
// documents, and reports whether yield asked for more.
func (t *TurnStream) emitText(m Message, yield func(string) bool) bool {
	return t.emitContent(m, func(text string, thinking bool) bool { return thinking || yield(text) })
}

// emitContent passes the main-conversation text and thinking of m to yield,
// each piece once: the deltas of stream events, and the blocks of the
// AssistantMessages whose deltas were not streamed. It reports whether
// yield asked for more. Every message of the turn must pass through it, so
// that it sees the stream events before the AssistantMessages they repeat.
func (t *TurnStream) emitContent(m Message, yield func(text string, thinking bool) bool) bool {
	switch m := m.(type) {
	case *StreamEvent:
		// Stream events arrive only with Options.IncludePartialMessages.
		if m.ParentToolUseID != "" || t.markStreamed(m) {
			return true
		}
		if text, ok := m.TextDelta(); ok && text != "" {
			return yield(text, false)
		}
		if text, ok := m.thinkingDelta(); ok && text != "" {
			return yield(text, true)
		}
	case *AssistantMessage:
		if m.ParentToolUseID != "" || t.streamed[m.MessageID] {
			return true
		}
		for _, block := range m.Content {
			switch b := block.(type) {
			case *TextBlock:
				if b.Text != "" && !yield(b.Text, false) {
					return false
				}
			case *ThinkingBlock:
				if b.Thinking != "" && !yield(b.Thinking, true) {
					return false
				}
			}
		}
	}
	return true
}

// markStreamed records the API message that e starts when e is a
// message_start event, and reports whether it is: the AssistantMessages of
// that API message repeat the deltas that follow.
func (t *TurnStream) markStreamed(e *StreamEvent) bool {
	id, ok := e.messageStartID()
	if !ok {
		return false
	}
	if t.streamed == nil {
		t.streamed = map[string]bool{}
	}
	t.streamed[id] = true
	return true
}

// Result waits for the end of the turn, discarding the messages not yet read,
// and returns its ResultMessage, whose Text method returns the final response
// text. An error result is returned alongside a *ResultError, and a stream
// that ended without a result as a *ConnectionError; otherwise the error is
// the one Events ends with. After Close on a turn that has not ended it
// returns an error matching ErrClosed; a turn that ended before Close keeps
// its result.
func (t *TurnStream) Result(ctx context.Context) (*ResultMessage, error) {
	if err := t.read(ctx, nil); err != nil {
		return nil, err
	}
	return checkResult(t.result)
}

// Cancel interrupts the turn, as Client.Interrupt does; the turn then ends
// with its ResultMessage as usual. It works after Close too, and is a no-op
// returning nil once the turn has ended.
func (t *TurnStream) Cancel(ctx context.Context) error {
	if t.end.Ended() {
		return nil
	}
	return t.client.Interrupt(ctx)
}

// Close stops reading the turn; it does not interrupt it. The turn's unread
// messages stay queued on the client and are skipped when the next turn is
// read. On a turn that has not ended, Events, Text and Result then fail
// with ErrClosed; on one that has, Close changes nothing. Close is
// idempotent and always returns nil.
func (t *TurnStream) Close() error {
	t.closed.Store(true)
	return nil
}

// read reads the turn as Events, Text and Result do, passing each of its
// messages to emit, which may be nil to discard them. The client's pending
// turns are read from the head of its queue, so the turns sent before this
// one are finished first. read returns when the turn ends or emit returns
// false.
func (t *TurnStream) read(ctx context.Context, emit func(Message) bool) error {
	for {
		if t.end.Ended() {
			return t.end.Err()
		}
		if t.closed.Load() {
			return closedError("turn stream")
		}
		head, err := t.client.headTurn()
		if err != nil {
			t.end.Finish(err)
			return err
		}
		if head != t {
			if err := head.readOwn(ctx, nil); err != nil {
				return err
			}
			continue
		}
		return t.readOwn(ctx, emit)
	}
}

// readOwn reads the messages of t, which is at the head of the client's
// queue, until its result or until emit returns false. The end of the
// stream, cleanly or with an error, ends every pending turn; a cancelled ctx
// ends none, so a later read can resume.
func (t *TurnStream) readOwn(ctx context.Context, emit func(Message) bool) error {
	for msg, err := range t.client.sess.eng.receive(ctx) {
		if err != nil {
			if ctx.Err() == nil {
				t.client.endTurns(t, err)
			}
			return err
		}
		if result, ok := msg.(*ResultMessage); ok {
			t.client.endTurn(t, result)
			if emit != nil {
				emit(result)
			}
			return nil
		}
		if emit != nil && !emit(msg) {
			return nil
		}
	}
	// The stream ended without a result.
	t.client.endTurns(t, nil)
	return nil
}

// checkResult returns the outcome of a turn or run whose ResultMessage is
// result: a *ConnectionError when there is none, the result with a
// *ResultError for an error result, and the result alone otherwise.
func checkResult(result *ResultMessage) (*ResultMessage, error) {
	if result == nil {
		return nil, newConnectionError("Claude Code ended without a result")
	}
	if result.IsError {
		return result, newErrorResultError(result.Data, nil)
	}
	return result, nil
}

// headTurn returns the oldest pending turn.
func (c *Client) headTurn() (*TurnStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || len(c.turns) == 0 {
		return nil, closedError("client")
	}
	return c.turns[0], nil
}

// endTurn ends t, the head of the queue, with result and removes it.
func (c *Client) endTurn(t *TurnStream, result *ResultMessage) {
	c.mu.Lock()
	if len(c.turns) > 0 && c.turns[0] == t {
		c.turns[0] = nil
		c.turns = c.turns[1:]
	}
	c.mu.Unlock()
	t.result = result
	t.end.Finish(nil)
}

// endTurns ends t and every pending turn with err, after the session's
// stream ended.
func (c *Client) endTurns(t *TurnStream, err error) {
	c.mu.Lock()
	pending := c.turns
	c.turns = nil
	c.mu.Unlock()
	for _, p := range pending {
		p.end.Finish(err)
	}
	t.end.Finish(err)
}
