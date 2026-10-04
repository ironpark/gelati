package claude

import (
	"context"
	"errors"
	"iter"
	"slices"
	"sync"
)

// WarmQuery is a CLI session that has already started and completed the
// initialize handshake, waiting for its one prompt. Build one with Startup
// ahead of time so the prompt's first response arrives without the startup
// latency.
//
// Exactly one of Query or QueryStream may be used, once. Close discards the
// session; it is idempotent and safe to defer, including after the query has
// run.
type WarmQuery struct {
	sess *session

	mu     sync.Mutex
	used   bool
	closed bool
}

// Startup starts a CLI session and performs the initialize handshake,
// returning a WarmQuery that runs one prompt on it. When ctx has no deadline
// the handshake is bounded by DefaultInitializeTimeout; a handshake that does
// not complete tears the session down.
//
// As with Client.Connect, ctx governs the whole session: cancelling it after
// Startup returns terminates the CLI. Use a context that outlives the query.
func Startup(ctx context.Context, opts *Options) (*WarmQuery, error) {
	return startup(ctx, opts, nil)
}

// startup is Startup with replaceable session dependencies.
func startup(ctx context.Context, opts *Options, deps *sessionDeps) (*WarmQuery, error) {
	sess, err := startSession(ctx, opts, entrypoint, deps)
	if err != nil {
		return nil, err
	}
	return &WarmQuery{sess: sess}, nil
}

// InitializationResult reports the session's initialize response.
func (w *WarmQuery) InitializationResult() *InitializeResult {
	return w.sess.eng.initializeResult()
}

// Query sends prompt and yields the messages it produces, ending with a
// ResultMessage, like the package-level Query. The session is torn down when
// the sequence ends, when the caller breaks out of the range loop, or when ctx
// is cancelled.
func (w *WarmQuery) Query(ctx context.Context, prompt string) iter.Seq2[Message, error] {
	return w.QueryStream(ctx, slices.Values([]UserInput{{Content: prompt}}))
}

// QueryStream is Query with several user turns known up front, like the
// package-level QueryStream.
func (w *WarmQuery) QueryStream(ctx context.Context, inputs iter.Seq[UserInput]) iter.Seq2[Message, error] {
	w.mu.Lock()
	var err error
	switch {
	case w.closed:
		err = NewConnectionError("warm query is closed")
	case w.used:
		err = errors.New("claude: WarmQuery can run only one query")
	}
	w.used = true
	w.mu.Unlock()
	if err != nil {
		return func(yield func(Message, error) bool) { yield(nil, err) }
	}
	return singleUse(func(yield func(Message, error) bool) {
		runQuery(ctx, w.sess, inputs, yield)
	}, "claude: a WarmQuery sequence can be ranged over only once")
}

// singleUse makes seq rangeable once: ranging over it again yields an error
// with errMsg. A concurrent second range waits for the first to finish.
func singleUse(seq iter.Seq2[Message, error], errMsg string) iter.Seq2[Message, error] {
	var once sync.Once
	return func(yield func(Message, error) bool) {
		ran := false
		once.Do(func() {
			ran = true
			seq(yield)
		})
		if !ran {
			yield(nil, errors.New(errMsg))
		}
	}
}

// Close terminates the session. It is idempotent.
func (w *WarmQuery) Close() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	return w.sess.close()
}
