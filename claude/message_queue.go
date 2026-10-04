package claude

import (
	"context"
	"iter"
	"sync"
)

// messageBufferSize is how many messages the engine buffers ahead of the
// consumer.
const messageBufferSize = 100

// messageOrError is one item of the message stream: a message, or the fatal
// error that ends it.
type messageOrError struct {
	msg Message
	err error
}

// messageQueue is the engine's outgoing message stream. The read loop is its
// main producer and the only one that blocks (push) and closes it; side
// producers such as mirror error reports use tryPush, which never blocks.
type messageQueue struct {
	ch chan messageOrError
	// done releases a blocked push when the engine closes.
	done <-chan struct{}

	// mu serializes side producers with close.
	mu     sync.Mutex
	closed bool
}

// newMessageQueue builds a queue whose blocking pushes give up once done is
// closed.
func newMessageQueue(done <-chan struct{}) *messageQueue {
	return &messageQueue{ch: make(chan messageOrError, messageBufferSize), done: done}
}

// push hands one item to the consumer, waiting for buffer space, and reports
// whether it was accepted. Only the read loop calls it.
func (q *messageQueue) push(item messageOrError) bool {
	select {
	case q.ch <- item:
		return true
	case <-q.done:
		return false
	}
}

// tryPush hands one item to the consumer without blocking. When the buffer is
// full, or the stream has ended, the item is dropped.
func (q *messageQueue) tryPush(item messageOrError) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	select {
	case q.ch <- item:
	default:
	}
}

// close ends the stream. Only the read loop calls it, once.
func (q *messageQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	close(q.ch)
}

// receive yields the queued messages in order until the stream ends; a fatal
// error is the last item. A cancelled ctx ends the sequence with its error.
func (q *messageQueue) receive(ctx context.Context) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		for {
			select {
			case item, ok := <-q.ch:
				if !ok {
					return
				}
				if !yield(item.msg, item.err) || item.err != nil {
					return
				}
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
		}
	}
}
