package antigravity

import (
	"context"
	"errors"
	"fmt"

	"github.com/ironpark/gelati/antigravity/internal/harness"
)

// Error is implemented by every error type this package originates, so
// callers can write
//
//	var agErr antigravity.Error
//	if errors.As(err, &agErr) { ... }
type Error interface {
	error
	antigravityError()
}

// Sentinel errors.
var (
	// ErrNotStarted is returned by Agent methods that need a running session
	// before Start (Python raises RuntimeError there).
	ErrNotStarted = errors.New("antigravity: agent session not started; call Start first")
	// ErrClosed is returned by operations on a closed Agent or Connection.
	ErrClosed = errors.New("antigravity: closed")
	// ErrConcurrentReceive is returned when a second consumer starts reading
	// a Connection's steps while another is still reading them. Steps come
	// from a single queue, so two readers would steal steps from each other.
	ErrConcurrentReceive = errors.New("antigravity: concurrent ReceiveSteps calls are not supported on this connection")
	// ErrBinaryNotFound reports that no localharness binary could be
	// located; see Config.CLIPath.
	ErrBinaryNotFound = harness.ErrBinaryNotFound
)

// ConnectionError reports that a connection to the agent backend could not
// be established or hit a fatal protocol-level error: the harness failed to
// launch, exited unexpectedly, or reported a fatal HTTP 400/401/403 system
// error. Ported from AntigravityConnectionError.
type ConnectionError struct {
	// Message describes the failure.
	Message string
	// Code is the WebSocket close code when the harness connection dropped,
	// or zero.
	Code int
	// Stderr is the tail of the harness's stderr output, when known.
	Stderr string
	// Err is the underlying error, if any.
	Err error
}

func (e *ConnectionError) Error() string     { return e.Message }
func (e *ConnectionError) Unwrap() error     { return e.Err }
func (e *ConnectionError) antigravityError() {}

// ExecutionError reports that the agent loop terminated with a fatal error
// (a model call failure, a system constraint violation, a denied turn) and
// cannot continue the turn. Ported from AntigravityExecutionError.
type ExecutionError struct {
	Message string
}

func (e *ExecutionError) Error() string     { return e.Message }
func (e *ExecutionError) antigravityError() {}

// CancelledError reports that the active turn was cancelled by the client
// (see Connection.Cancel). Python's AntigravityCancelledError subclasses
// asyncio.CancelledError; correspondingly errors.Is(err, context.Canceled)
// reports true for a *CancelledError.
type CancelledError struct {
	Message string
}

func (e *CancelledError) Error() string {
	if e.Message == "" {
		return "The request was cancelled by the client."
	}
	return e.Message
}

// Is reports whether target is context.Canceled.
func (e *CancelledError) Is(target error) bool { return target == context.Canceled }
func (e *CancelledError) antigravityError()    {}

// ValidationError reports invalid configuration or input. Ported from
// AntigravityValidationError, which also stands in for the pydantic
// validation errors and ValueErrors upstream raises from its validators.
type ValidationError struct {
	Message string
	// Err is the underlying error, if any.
	Err error
}

func (e *ValidationError) Error() string     { return e.Message }
func (e *ValidationError) Unwrap() error     { return e.Err }
func (e *ValidationError) antigravityError() {}

func validationErrorf(format string, args ...any) *ValidationError {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

// ToolExecutionError describes a failed tool call. OnToolErrorHook receives
// one for every failure the harness reports, built-in or custom; tools may
// also return one. The harness boundary carries only the message, so the
// original error type of a custom tool is not preserved.
type ToolExecutionError struct {
	Message    string
	ToolName   string
	ServerName string
	CallID     string
	StepID     string
}

func (e *ToolExecutionError) Error() string     { return e.Message }
func (e *ToolExecutionError) antigravityError() {}

// connectionErrorFrom maps a harness-package error to a *ConnectionError,
// keeping the harness's stderr and close code.
func connectionErrorFrom(err error) error {
	if err == nil {
		return nil
	}
	if ce, ok := errors.AsType[*harness.ConnectionError](err); ok {
		return &ConnectionError{Message: ce.Error(), Code: int(ce.Code), Stderr: ce.Stderr, Err: err}
	}
	if se, ok := errors.AsType[*harness.StartError](err); ok {
		return &ConnectionError{Message: se.Error(), Stderr: se.Stderr, Err: err}
	}
	return &ConnectionError{Message: err.Error(), Err: err}
}
