package codex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
)

// Error is implemented by every error type this package originates
// (*RPCError, *TurnError, and *ProcessError), so callers can write
//
//	if sdkErr, ok := errors.AsType[codex.Error](err); ok { ... }
type Error interface {
	error
	codexError()
}

func (*RPCError) codexError()     {}
func (*TurnError) codexError()    {}
func (*ProcessError) codexError() {}

// ProcessError reports that the app-server subprocess exited. Client.Err
// wraps it once the stream ends because of the exit.
type ProcessError struct {
	// ExitCode is the exit status, or nil when the process was killed by a
	// signal or its status is unknown.
	ExitCode *int
	// Stderr is the tail of the process's stderr output.
	Stderr string
	// Err is the error cmd.Wait reported, if any.
	Err error
}

func (e *ProcessError) Error() string {
	msg := "codex: app-server exited"
	switch {
	case e.ExitCode != nil:
		msg = fmt.Sprintf("codex: app-server exited with status %d", *e.ExitCode)
	case e.Err != nil:
		msg = "codex: app-server " + e.Err.Error()
	}
	if e.Stderr != "" {
		msg += "\napp-server stderr:\n" + e.Stderr
	}
	return msg
}

func (e *ProcessError) Unwrap() error { return e.Err }

// Sentinel errors returned by the package.
var (
	// ErrClosed is returned when the client or its transport has been closed.
	ErrClosed = errors.New("codex: client closed")
	// ErrCLINotFound is returned when the codex executable cannot be
	// located or started from Options.CLIPath.
	ErrCLINotFound = errors.New("codex: codex CLI not found; install it or set Options.CLIPath")
	// ErrNotInitialized is returned when an API call is made before the
	// initialize/initialized handshake completed.
	ErrNotInitialized = errors.New("codex: not initialized")
)

// RPCError is a JSON-RPC 2.0 error object returned by the app-server.
type RPCError struct {
	// Code is the JSON-RPC error code.
	Code int `json:"code"`
	// Message is the human-readable error message.
	Message string `json:"message"`
	// Data carries optional structured error detail.
	Data json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("codex: rpc error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("codex: rpc error %d: %s", e.Code, e.Message)
}

// JSON-RPC error codes used by the app-server.
const (
	// CodeParseError signals malformed JSON.
	CodeParseError = -32700
	// CodeInvalidRequest signals an invalid request object.
	CodeInvalidRequest = -32600
	// CodeMethodNotFound signals an unknown method.
	CodeMethodNotFound = -32601
	// CodeInvalidParams signals invalid method parameters.
	CodeInvalidParams = -32602
	// CodeInternalError signals an internal server error.
	CodeInternalError = -32603
	// CodeServerOverloaded is returned when request ingress is full and the
	// client should retry with exponential backoff and jitter.
	CodeServerOverloaded = -32001
)

// codexErrorInfo discriminators carried by a failed turn's error. Compare
// them against TurnError.Kind.
const (
	ErrorInfoContextWindowExceeded          = "contextWindowExceeded"
	ErrorInfoSessionBudgetExceeded          = "sessionBudgetExceeded"
	ErrorInfoUsageLimitExceeded             = "usageLimitExceeded"
	ErrorInfoRateLimitExceeded              = "rateLimitExceeded"
	ErrorInfoFlexUnavailable                = "flexUnavailable"
	ErrorInfoServerOverloaded               = "serverOverloaded"
	ErrorInfoCyberPolicy                    = "cyberPolicy"
	ErrorInfoTooManyDenials                 = "tooManyDenials"
	ErrorInfoInternalServerError            = "internalServerError"
	ErrorInfoUnauthorized                   = "unauthorized"
	ErrorInfoBadRequest                     = "badRequest"
	ErrorInfoSandboxError                   = "sandboxError"
	ErrorInfoOther                          = "other"
	ErrorInfoHTTPConnectionFailed           = "httpConnectionFailed"
	ErrorInfoResponseStreamConnectionFailed = "responseStreamConnectionFailed"
	ErrorInfoResponseStreamDisconnected     = "responseStreamDisconnected"
	ErrorInfoResponseTooManyFailedAttempts  = "responseTooManyFailedAttempts"
	ErrorInfoActiveTurnNotSteerable         = "activeTurnNotSteerable"
)

// IsOverloaded reports whether err is a transient server-overload error that
// the caller may retry after a backoff: JSON-RPC code -32001, or a server
// error (-32099..-32000) whose data marks it server_overloaded, the way
// upstream's is_retryable_error decides.
func IsOverloaded(err error) bool {
	rpcErr, ok := serverError(err)
	if !ok {
		return false
	}
	if rpcErr.Code == CodeServerOverloaded {
		return true
	}
	if len(rpcErr.Data) == 0 {
		return false
	}
	var data any
	if json.Unmarshal(rpcErr.Data, &data) != nil {
		return false
	}
	return mentionsOverload(data)
}

// IsRetryLimitExceeded reports whether err is a server error saying its own
// retry budget ran out.
func IsRetryLimitExceeded(err error) bool {
	rpcErr, ok := serverError(err)
	if !ok {
		return false
	}
	msg := strings.ToLower(rpcErr.Message)
	return strings.Contains(msg, "retry limit") || strings.Contains(msg, "too many failed attempts")
}

// serverError returns err's *RPCError when its code is in the JSON-RPC
// implementation-defined server error range, -32099..-32000.
func serverError(err error) (*RPCError, bool) {
	rpcErr, ok := errors.AsType[*RPCError](err)
	if !ok || rpcErr.Code < -32099 || rpcErr.Code > -32000 {
		return nil, false
	}
	return rpcErr, true
}

// mentionsOverload reports whether any string in a decoded JSON value is
// server_overloaded or serverOverloaded.
func mentionsOverload(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.EqualFold(v, "server_overloaded") || strings.EqualFold(v, ErrorInfoServerOverloaded)
	case map[string]any:
		for key, value := range v {
			if strings.EqualFold(key, ErrorInfoServerOverloaded) || mentionsOverload(value) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(v, mentionsOverload)
	}
	return false
}

// RetryOptions tunes RetryOnOverload. Zero fields take upstream's defaults.
type RetryOptions struct {
	// MaxAttempts bounds the calls, including the first; default 3.
	MaxAttempts int
	// InitialDelay is the first backoff; default 250ms. It doubles per retry.
	InitialDelay time.Duration
	// MaxDelay caps the backoff; default 2s.
	MaxDelay time.Duration
	// Jitter is the random fraction added to or taken from each delay;
	// default 0.2.
	Jitter float64
}

// RetryOnOverload calls op until it succeeds, fails with an error IsOverloaded
// rejects, runs out of attempts, or ctx ends, backing off exponentially with
// jitter between attempts.
func RetryOnOverload[T any](ctx context.Context, opts RetryOptions, op func(context.Context) (T, error)) (T, error) {
	attempts := cmp.Or(opts.MaxAttempts, 3)
	delay := cmp.Or(opts.InitialDelay, 250*time.Millisecond)
	maxDelay := cmp.Or(opts.MaxDelay, 2*time.Second)
	jitter := cmp.Or(opts.Jitter, 0.2)
	for attempt := 1; ; attempt++ {
		value, err := op(ctx)
		if err == nil || attempt >= attempts || !IsOverloaded(err) {
			return value, err
		}
		wait := min(delay, maxDelay)
		wait += time.Duration((rand.Float64()*2 - 1) * jitter * float64(wait))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			var zero T
			return zero, ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, maxDelay)
	}
}
