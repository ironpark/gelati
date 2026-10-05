package claude

import (
	"context"
	"iter"
	"slices"
	"time"

	"github.com/ironpark/gelati/internal/lifecycle"
)

// Query runs a one-shot prompt and yields the messages it produces, ending with
// a ResultMessage. It is the simple, stateless entry point; use Client for
// interactive sessions that need follow-ups or interrupts.
//
// The CLI subprocess is torn down when the sequence ends, when the caller
// breaks out of the range loop, or when ctx is cancelled.
//
//	for msg, err := range claude.Query(ctx, "What is 2+2?", claude.Options{}) {
//		if err != nil {
//			return err
//		}
//		fmt.Println(msg)
//	}
func Query(ctx context.Context, prompt string, opts Options) iter.Seq2[Message, error] {
	return QueryStream(ctx, slices.Values([]UserInput{Text(prompt)}), opts)
}

// Run runs a one-shot prompt to completion and returns its final
// ResultMessage, whose Text method returns the final response text. It ranges over
// Query, dropping the intermediate messages; use Query to observe them.
//
// The error is the one Query's stream ends with, returned alongside the
// ResultMessage when one arrived first. Unlike Query, Run also reports an
// error result as a *ResultError, and a stream with no result as a
// *ConnectionError.
func Run(ctx context.Context, prompt string, opts Options) (*ResultMessage, error) {
	return collectResult(Query(ctx, prompt, opts))
}

// collectResult drains messages and returns the last ResultMessage, with the
// errors Run documents.
func collectResult(messages iter.Seq2[Message, error]) (*ResultMessage, error) {
	var result *ResultMessage
	for msg, err := range messages {
		if err != nil {
			return result, err
		}
		if r, ok := msg.(*ResultMessage); ok {
			result = r
		}
	}
	return checkResult(result)
}

// QueryStream is Query with several user turns known up front. Every input is
// written before the responses are consumed, so it stays unidirectional: use
// Client when a later turn depends on an earlier response.
func QueryStream(ctx context.Context, inputs iter.Seq[UserInput], opts Options) iter.Seq2[Message, error] {
	return queryStream(ctx, inputs, &opts, nil)
}

// queryStream is QueryStream with replaceable session dependencies.
func queryStream(ctx context.Context, inputs iter.Seq[UserInput], opts *Options, deps *sessionDeps) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		sess, err := startSession(ctx, opts, entrypoint, deps)
		if err != nil {
			yield(nil, err)
			return
		}
		runQuery(ctx, sess, inputs, yield)
	}
}

// runQuery writes inputs to an initialized session, yields its messages and
// closes the session when the sequence ends or the caller stops.
func runQuery(ctx context.Context, sess *session, inputs iter.Seq[UserInput], yield func(Message, error) bool) {
	eng := sess.eng
	defer sess.close()

	// Inputs are written in the background: with hooks, permission
	// callbacks or in-process MCP servers configured the writer holds the
	// input stream open until the run ends, which cannot happen before the
	// caller has consumed the messages.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_ = eng.streamInput(ctx, inputs)
	}()
	stopped := true
	defer func() {
		// A caller that stopped early does not wait for the writer: closing
		// the engine releases one still holding the input open. Either way
		// the writer gets a moment to finish so the goroutine never outlives
		// the query.
		if stopped {
			_ = eng.close()
		}
		lifecycle.WaitClosed(writerDone, 5*time.Second)
	}()

	for msg, err := range eng.receive(ctx) {
		if !yield(msg, err) || err != nil {
			return
		}
	}
	stopped = false
}
