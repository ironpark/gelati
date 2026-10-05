// Package lifecycle records how a session ended, for the Done and Err
// methods the SDK packages expose: a channel closed once it ends and the
// reason it ended, set together and only once.
package lifecycle

import (
	"sync"
	"time"
)

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
