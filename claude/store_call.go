package claude

import (
	"context"
	"errors"
	"time"
)

// storeCallStatus is how a call made through runStoreCall ended.
type storeCallStatus int

const (
	// storeCallReturned: the call returned; its error, if any, is reported.
	storeCallReturned storeCallStatus = iota
	// storeCallTimedOut: the timeout expired, either before the call returned
	// (no error is reported) or while it failed (its error is reported).
	storeCallTimedOut
	// storeCallCanceled: the parent context ended before the call returned;
	// the parent's error is reported.
	storeCallCanceled
)

// runStoreCall runs one SessionStore call with a context derived from parent
// and bounded by timeout. The call runs on its own goroutine so a store that
// ignores its context cannot hang the caller: when the context ends first,
// the call is abandoned and ends whenever the store returns. A panic in call
// is reported as the error panicErr builds from the recovered value.
func runStoreCall[T any](parent context.Context, timeout time.Duration, panicErr func(r any) error,
	call func(context.Context) (T, error),
) (T, storeCallStatus, error) {
	var zero T
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: panicErr(r)}
			}
		}()
		v, err := call(ctx)
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			return r.v, storeCallReturned, nil
		}
		if parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return zero, storeCallTimedOut, r.err
		}
		return zero, storeCallReturned, r.err
	case <-ctx.Done():
		if err := parent.Err(); err != nil {
			return zero, storeCallCanceled, err
		}
		return zero, storeCallTimedOut, nil
	}
}
