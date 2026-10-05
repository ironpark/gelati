package claude

import (
	"context"
	"errors"
	"time"

	"github.com/ironpark/gelati/internal/safecall"
)

// errStoreTimeout matches (errors.Is) a runStoreCall error caused by its
// timeout expiring.
var errStoreTimeout = errors.New("SessionStore call timed out")

// storeTimeoutError is a call error that arrived after the timeout expired.
// It reads as the call's error and also matches errStoreTimeout.
type storeTimeoutError struct{ err error }

func (e storeTimeoutError) Error() string        { return e.err.Error() }
func (e storeTimeoutError) Unwrap() error        { return e.err }
func (e storeTimeoutError) Is(target error) bool { return target == errStoreTimeout }

// runStoreCall runs one SessionStore call with a context derived from parent
// and bounded by timeout. The call runs on its own goroutine so a store that
// ignores its context cannot hang the caller: when the context ends first,
// the call is abandoned and ends whenever the store returns.
//
// The error is the call's own error, or, when the timeout expired, one
// matching errStoreTimeout (wrapping the call's error when it failed), or,
// when parent ended first, parent's error. A panic in call is reported as an
// error.
func runStoreCall[T any](parent context.Context, timeout time.Duration, call func(context.Context) (T, error)) (T, error) {
	var zero T
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		defer func() { done <- r }()
		defer safecall.Recover(&r.err, "SessionStore")
		r.v, r.err = call(ctx)
	}()
	select {
	case r := <-done:
		if r.err == nil {
			return r.v, nil
		}
		if parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return zero, storeTimeoutError{r.err}
		}
		return zero, r.err
	case <-ctx.Done():
		if err := parent.Err(); err != nil {
			return zero, err
		}
		return zero, errStoreTimeout
	}
}
