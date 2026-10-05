package codex

import (
	"context"
	"iter"
	"sync"
)

// broadcast fans values out to the iterators currently ranging over it. Each
// listener has its own bounded buffer; publish never blocks, so a listener
// that falls behind loses the newest values rather than stalling the
// transport reader.
type broadcast[T any] struct {
	buffer int

	mu        sync.Mutex
	closed    bool
	listeners map[chan T]struct{}
}

func newBroadcast[T any](buffer int) *broadcast[T] {
	return &broadcast[T]{buffer: buffer, listeners: make(map[chan T]struct{})}
}

// publish offers v to every listener, reporting whether any listener's
// buffer was full and dropped it.
func (b *broadcast[T]) publish(v T) (dropped bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.listeners {
		select {
		case ch <- v:
		default:
			dropped = true
		}
	}
	return dropped
}

// close ends every listener once it drains its buffer, and makes later
// listeners end immediately.
func (b *broadcast[T]) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.listeners {
		close(ch)
		delete(b.listeners, ch)
	}
}

// listen registers a listener. The returned channel is closed by close;
// release unregisters it.
func (b *broadcast[T]) listen() (ch chan T, release func()) {
	ch = make(chan T, b.buffer)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	b.listeners[ch] = struct{}{}
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.listeners, ch)
	}
}

// seq iterates b from the moment iteration starts until the loop exits, ctx
// ends (yielding ctx's error), or b closes. When b closes, endErr is called
// once the buffered values are drained; a non-nil result is yielded last.
func (b *broadcast[T]) seq(ctx context.Context, endErr func() error) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		ch, release := b.listen()
		defer release()
		for {
			select {
			case <-ctx.Done():
				var zero T
				yield(zero, ctx.Err())
				return
			case v, ok := <-ch:
				if !ok {
					if err := endErr(); err != nil {
						var zero T
						yield(zero, err)
					}
					return
				}
				if !yield(v, nil) {
					return
				}
			}
		}
	}
}
