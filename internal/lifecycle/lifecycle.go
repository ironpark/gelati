// Package lifecycle records how a session ended, for the Done and Err
// methods the SDK packages expose: a channel closed once it ends and the
// reason it ended, set together and only once. It also holds the cleanup
// helpers for work a caller stopped waiting for.
package lifecycle

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DetachedTimeout bounds best-effort cleanup that runs after the caller's
// context has ended.
const DetachedTimeout = 5 * time.Second

// Detached returns a context that keeps ctx's values but not its
// cancellation, bounded by DetachedTimeout.
func Detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), DetachedTimeout)
}

// CancelIfDone calls cancel, best effort and under a Detached context, when
// err is ctx's error: the caller stopped waiting for an operation that should
// not keep running. It reports whether it called cancel.
func CancelIfDone(ctx context.Context, err error, cancel func(context.Context) error) bool {
	if ctx.Err() == nil || !errors.Is(err, ctx.Err()) {
		return false
	}
	dctx, stop := Detached(ctx)
	defer stop()
	_ = cancel(dctx)
	return true
}

// Done records the end of a session. The zero value is a session that has
// not ended. It is safe for concurrent use.
type Done struct {
	mu   sync.Mutex
	ch   chan struct{}
	err  error
	over bool
}

// chLocked returns the channel, creating it on first use. d.mu is held.
func (d *Done) chLocked() chan struct{} {
	if d.ch == nil {
		d.ch = make(chan struct{})
	}
	return d.ch
}

// Finish ends the session with err, which may be nil, and reports whether
// this call ended it. Only the first call has an effect.
func (d *Done) Finish(err error) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.over {
		return false
	}
	d.over, d.err = true, err
	close(d.chLocked())
	return true
}

// C returns a channel closed once the session has ended.
func (d *Done) C() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.chLocked()
}

// Ended reports whether the session has ended.
func (d *Done) Ended() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.over
}

// Err returns the error the session ended with, or nil while it runs.
func (d *Done) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// WaitClosed reports whether ch closes within d.
func WaitClosed(ch <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}
