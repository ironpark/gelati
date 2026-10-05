package antigravity

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
)

// ChatResponse is the streaming response of one turn, returned by
// Agent.Chat and Conversation.Chat.
//
// The stream is pulled lazily from the connection and buffered: every
// iterator (Chunks, Text, Thoughts, ToolCalls) is an independent cursor
// over the shared buffer, so the same response can be read several times,
// sequentially or from concurrent goroutines. When the stream fails, every
// cursor that reaches the end gets the error. Cancelling the context of one
// cursor ends only that cursor; the stream stays readable.
type ChatResponse struct {
	next   func(ctx context.Context) (Chunk, bool, error)
	conv   *Conversation // nil in tests
	turn   *turn
	onDone func()

	pull chan struct{} // one-slot semaphore serializing pulls from next

	mu   sync.Mutex
	buf  []Chunk
	done bool
	err  error
}

func newChatResponse(next func(ctx context.Context) (Chunk, bool, error)) *ChatResponse {
	return &ChatResponse{next: next, pull: make(chan struct{}, 1)}
}

func (r *ChatResponse) isDone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

// Chunks yields every chunk of the turn: *TextChunk, *ThoughtChunk and
// *ToolCall. A stream error is the final item.
func (r *ChatResponse) Chunks(ctx context.Context) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
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
			if r.done {
				err := r.err
				r.mu.Unlock()
				if err != nil {
					yield(nil, err)
				}
				return
			}
			r.mu.Unlock()
			if err := r.pullOne(ctx, pos); err != nil {
				yield(nil, err)
				return
			}
		}
	}
}

// pullOne pulls the next chunk into the buffer unless another cursor did so
// while this one waited (the buffer grew past pos, or the stream ended).
// It returns only context errors; stream errors are stored.
func (r *ChatResponse) pullOne(ctx context.Context, pos int) error {
	select {
	case r.pull <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.pull }()
	r.mu.Lock()
	stale := pos < len(r.buf) || r.done
	r.mu.Unlock()
	if stale {
		return nil
	}
	ch, ok, err := r.next(ctx)
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return err
	}
	r.mu.Lock()
	switch {
	case err != nil:
		r.done, r.err = true, err
	case !ok:
		r.done = true
	default:
		r.buf = append(r.buf, ch)
	}
	finished := r.done
	r.mu.Unlock()
	if finished && r.onDone != nil {
		r.onDone()
	}
	return nil
}

// filterChunks yields the chunks of type C, mapped through f.
func filterChunks[C Chunk, T any](r *ChatResponse, ctx context.Context, f func(C) T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for ch, err := range r.Chunks(ctx) {
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
func (r *ChatResponse) Text(ctx context.Context) iter.Seq2[string, error] {
	return filterChunks(r, ctx, func(c *TextChunk) string { return c.Text })
}

// Thoughts yields the deltas of the model's reasoning.
func (r *ChatResponse) Thoughts(ctx context.Context) iter.Seq2[string, error] {
	return filterChunks(r, ctx, func(c *ThoughtChunk) string { return c.Text })
}

// ToolCalls yields the turn's tool calls as they are dispatched.
func (r *ChatResponse) ToolCalls(ctx context.Context) iter.Seq2[*ToolCall, error] {
	return filterChunks(r, ctx, func(c *ToolCall) *ToolCall { return c })
}

// Resolve drains the stream and returns every chunk.
func (r *ChatResponse) Resolve(ctx context.Context) ([]Chunk, error) {
	var out []Chunk
	for ch, err := range r.Chunks(ctx) {
		if err != nil {
			return out, err
		}
		out = append(out, ch)
	}
	return out, nil
}

// WaitText drains the stream and returns the full response text.
func (r *ChatResponse) WaitText(ctx context.Context) (string, error) {
	var b strings.Builder
	for t, err := range r.Text(ctx) {
		if err != nil {
			return b.String(), err
		}
		b.WriteString(t)
	}
	return b.String(), nil
}

// StructuredOutput drains the stream and returns the parsed JSON output of
// the turn's finish step (see Config.ResponseSchema), or nil. When the turn
// had no finish step it falls back to the latest one in the conversation
// history, as upstream.
func (r *ChatResponse) StructuredOutput(ctx context.Context) (any, error) {
	if !r.isDone() {
		if _, err := r.Resolve(ctx); err != nil {
			return nil, err
		}
	}
	if r.conv == nil {
		return nil, nil
	}
	if out, ok := r.conv.conn.turnStructuredOutput(r.turn); ok {
		return out, nil
	}
	return r.conv.LastStructuredOutput(), nil
}

// DecodeStructuredOutput drains the stream and decodes the structured
// output into v through encoding/json. It reports an error when the turn
// produced none.
func (r *ChatResponse) DecodeStructuredOutput(ctx context.Context, v any) error {
	out, err := r.StructuredOutput(ctx)
	if err != nil {
		return err
	}
	if out == nil {
		return errors.New("antigravity: the turn produced no structured output")
	}
	return jsonConvert(out, v)
}

// UsageMetadata returns the token usage of the turn so far, or nil when
// none was reported. It does not wait for the stream.
func (r *ChatResponse) UsageMetadata() *UsageMetadata {
	if r.conv == nil {
		return nil
	}
	u := r.conv.conn.turnUsage(r.turn)
	if val(u.TotalTokenCount) == 0 {
		return nil
	}
	return &u
}

// StopReason returns why the turn stopped; meaningful once the stream has
// ended.
func (r *ChatResponse) StopReason() StopReason {
	if r.conv == nil {
		return StopReasonUnspecified
	}
	return r.conv.conn.turnStopReason(r.turn)
}

// Cancel halts the turn. It is a no-op once the stream has ended.
func (r *ChatResponse) Cancel(ctx context.Context) error {
	if r.isDone() || r.conv == nil {
		return nil
	}
	return r.conv.conn.cancel(ctx, r.turn)
}
