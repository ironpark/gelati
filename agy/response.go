package agy

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"

	"github.com/ironpark/gelati/internal/lifecycle"
)

// TurnStream is the streaming response of one turn, returned by
// Agent.Chat and Conversation.Chat.
//
// The stream is pulled lazily from the connection and buffered: every
// iterator (Events, Text, Thoughts, ToolCalls) is an independent cursor
// over the shared buffer, so the same turn can be read several times,
// sequentially or from concurrent goroutines. When the stream fails, every
// cursor that reaches the end gets the error. Cancelling the context of one
// cursor ends only that cursor; the stream stays readable. Result waits for
// the turn to end and summarizes it.
//
// While the stream is being read it holds the connection's single step
// reader; it gives it up when the turn ends or on Close.
//
// Close and Cancel follow the rules shared by every gelati SDK:
//   - Close stops reading and never interrupts the turn. It is a no-op once
//     the turn has ended, and Result still returns its result. Events and
//     Result on a stream closed before its turn ended return ErrClosed.
//   - Cancel interrupts a running turn, also after Close. Once the turn has
//     ended it is a no-op returning nil.
//   - Result returns the turn's result whenever one exists, together with
//     the error when the turn failed.
type TurnStream struct {
	next   func(ctx context.Context) (Chunk, bool, error)
	conv   *Conversation // nil in tests
	turn   *turn
	onDone func()

	pull     chan struct{} // one-slot semaphore serializing pulls from next
	closeCtx context.Context
	close    context.CancelFunc // called by Close; interrupts a pull

	mu  sync.Mutex // guards buf; held while ending end, so both stay consistent
	buf []Chunk
	end lifecycle.Done // ended with the stream error, ErrClosed after Close
}

func newTurnStream(next func(ctx context.Context) (Chunk, bool, error)) *TurnStream {
	r := &TurnStream{next: next, pull: make(chan struct{}, 1)}
	r.closeCtx, r.close = context.WithCancel(context.Background())
	return r
}

// TurnResult summarizes a finished turn; TurnStream.Result returns it.
type TurnResult struct {
	// Chunks holds every chunk of the turn, in order: *TextChunk,
	// *ThoughtChunk and *ToolCall.
	Chunks []Chunk
	// StructuredOutput is the parsed JSON output of the turn's finish step
	// (see Options.ResponseSchema), or nil. When the turn had no finish step
	// it is the latest one in the conversation history, as upstream.
	StructuredOutput any
	// StopReason says why the turn stopped.
	StopReason StopReason
	// Usage is the token usage of the turn, or nil when none was reported.
	Usage *UsageMetadata
}

// Text returns the model's full response text: the concatenated text
// chunks.
func (r *TurnResult) Text() string {
	return strings.Join(chunksOf(r.Chunks, func(c *TextChunk) string { return c.Text }), "")
}

// Thoughts returns the model's full reasoning text.
func (r *TurnResult) Thoughts() string {
	return strings.Join(chunksOf(r.Chunks, func(c *ThoughtChunk) string { return c.Text }), "")
}

// ToolCalls returns the turn's tool calls.
func (r *TurnResult) ToolCalls() []*ToolCall {
	return chunksOf(r.Chunks, func(c *ToolCall) *ToolCall { return c })
}

// chunksOf returns the chunks of type C, mapped through f.
func chunksOf[C Chunk, T any](chunks []Chunk, f func(C) T) []T {
	var out []T
	for _, ch := range chunks {
		if c, ok := ch.(C); ok {
			out = append(out, f(c))
		}
	}
	return out
}

// DecodeStructuredOutput decodes StructuredOutput into v through
// encoding/json. It reports an error when the turn produced none.
func (r *TurnResult) DecodeStructuredOutput(v any) error {
	if r.StructuredOutput == nil {
		return errors.New("agy: the turn produced no structured output")
	}
	return jsonConvert(r.StructuredOutput, v)
}

// Events yields every chunk of the turn: *TextChunk, *ThoughtChunk and
// *ToolCall. A stream error is the final item; after Close it is
// ErrClosed.
func (r *TurnStream) Events(ctx context.Context) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		// Close interrupts a pull in progress through pctx.
		pctx, cancel := context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(r.closeCtx, cancel)()
		pos := 0
		for {
			r.mu.Lock()
			if pos < len(r.buf) {
				ch := r.buf[pos]
				pos++
				r.mu.Unlock()
				if !yield(ch, nil) {
					return
				}
				continue
			}
			ended, err := r.end.Ended(), r.end.Err()
			r.mu.Unlock()
			if ended {
				if err != nil {
					yield(nil, err)
				}
				return
			}
			if err := r.pullOne(ctx, pctx, pos); err != nil {
				yield(nil, err)
				return
			}
		}
	}
}

// pullOne pulls the next chunk into the buffer through pctx, a child of the
// cursor's ctx that Close cancels, unless another cursor did so while this
// one waited (the buffer grew past pos, or the stream ended). It returns
// only ctx's errors; stream errors are stored.
func (r *TurnStream) pullOne(ctx, pctx context.Context, pos int) error {
	select {
	case r.pull <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.pull }()
	r.mu.Lock()
	stale := pos < len(r.buf) || r.end.Ended()
	r.mu.Unlock()
	if stale {
		return nil
	}
	var (
		ch  Chunk
		ok  bool
		err error
	)
	if r.closeCtx.Err() == nil {
		ch, ok, err = r.next(pctx)
	}
	switch {
	case r.closeCtx.Err() != nil:
		r.finish(ErrClosed)
	case err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return err
	case err != nil:
		r.finish(err)
	case !ok:
		r.finish(nil)
	default:
		r.mu.Lock()
		r.buf = append(r.buf, ch)
		r.mu.Unlock()
	}
	return nil
}

// finish ends the stream with err, unless it already ended, and then
// releases its source. The caller holds the pull semaphore.
func (r *TurnStream) finish(err error) {
	r.mu.Lock()
	first := r.end.Finish(err)
	r.mu.Unlock()
	if first && r.onDone != nil {
		r.onDone()
	}
}

// filterChunks yields the chunks of type C, mapped through f.
func filterChunks[C Chunk, T any](r *TurnStream, ctx context.Context, f func(C) T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for ch, err := range r.Events(ctx) {
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			if c, ok := ch.(C); ok && !yield(f(c), nil) {
				return
			}
		}
	}
}

// Text yields the deltas of the model's response text as they stream in.
func (r *TurnStream) Text(ctx context.Context) iter.Seq2[string, error] {
	return filterChunks(r, ctx, func(c *TextChunk) string { return c.Text })
}

// Thoughts yields the deltas of the model's reasoning.
func (r *TurnStream) Thoughts(ctx context.Context) iter.Seq2[string, error] {
	return filterChunks(r, ctx, func(c *ThoughtChunk) string { return c.Text })
}

// ToolCalls yields the turn's tool calls as they are dispatched.
func (r *TurnStream) ToolCalls(ctx context.Context) iter.Seq2[*ToolCall, error] {
	return filterChunks(r, ctx, func(c *ToolCall) *ToolCall { return c })
}

// drain reads the stream to its end and returns every chunk.
func (r *TurnStream) drain(ctx context.Context) ([]Chunk, error) {
	var out []Chunk
	for ch, err := range r.Events(ctx) {
		if err != nil {
			return out, err
		}
		out = append(out, ch)
	}
	return out, nil
}

// Result waits for the turn to end and returns its summary. It reads the
// rest of the stream through its own cursor, so it can be called before,
// during or after iterating Events, Text, Thoughts or ToolCalls.
//
// When the turn fails (*ExecutionError, *ConnectionError, *CancelledError)
// Result returns the partial result together with that error. When ctx
// ends first it returns nil and ctx's error, and the turn keeps running;
// after Close it returns nil and ErrClosed.
func (r *TurnStream) Result(ctx context.Context) (*TurnResult, error) {
	chunks, err := r.drain(ctx)
	if err != nil && (errors.Is(err, ErrClosed) || ctx.Err() != nil && errors.Is(err, ctx.Err())) {
		return nil, err
	}
	res := &TurnResult{Chunks: chunks, StopReason: StopReasonUnspecified}
	if r.conv == nil {
		return res, err
	}
	conn := r.conv.conn
	res.StopReason = conn.turnStopReason(r.turn)
	if u := conn.turnUsage(r.turn); val(u.TotalTokenCount) != 0 {
		res.Usage = &u
	}
	if out, ok := conn.turnStructuredOutput(r.turn); ok {
		res.StructuredOutput = out
	} else {
		res.StructuredOutput = r.conv.LastStructuredOutput()
	}
	return res, err
}

// Cancel halts the turn, also after Close. It is a no-op returning nil
// once the turn has ended.
func (r *TurnStream) Cancel(ctx context.Context) error {
	if r.conv == nil {
		return nil
	}
	if r.end.Ended() && (!errors.Is(r.end.Err(), ErrClosed) || r.conv.conn.turnEnded(r.turn)) {
		return nil // the turn is over
	}
	return r.conv.conn.cancel(ctx, r.turn)
}

// Close stops reading the stream without cancelling the turn: it gives up
// the connection's step reader, so that Conversation.ReceiveSteps (or the
// next Chat or Send, which drain the rest of the turn into the history)
// can proceed. Cursors yield the chunks already buffered and then
// ErrClosed. Close is a no-op once the stream has ended; it always returns
// nil.
func (r *TurnStream) Close() error {
	r.close()
	r.pull <- struct{}{}
	r.finish(ErrClosed)
	<-r.pull
	return nil
}
