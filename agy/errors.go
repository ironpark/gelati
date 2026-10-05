package agy

import (
	"context"
	"errors"
	"fmt"

	"github.com/ironpark/gelati/agy/internal/harness"
)

// Error is implemented by every error type this package originates, so
// callers can write
//
//	var agErr agy.Error
//	if errors.As(err, &agErr) { ... }
type Error interface {
	error
	agyError()
}

// Sentinel errors.
var (
	// ErrClosed is returned by operations on a closed Connection (and so a
	// closed Agent or Conversation), and ends the cursors of a closed
	// TurnStream.
	ErrClosed = harness.ErrClosed
	// ErrConcurrentReceive is returned when a second consumer starts reading
	// a Connection's steps while another is still reading them. Steps come
	// from a single queue, so two readers would steal steps from each other.
	ErrConcurrentReceive = errors.New("agy: concurrent ReceiveSteps calls are not supported on this connection")
	// ErrCLINotFound reports that no localharness binary could be located
	// or started; see Options.CLIPath. New returns it inside a
	// *ConnectionError.
	ErrCLINotFound = harness.ErrCLINotFound
)

// ConnectionError reports that a connection to the agent backend could not
// be established or hit a fatal protocol-level error: the harness failed to
// launch, exited unexpectedly, or reported a fatal HTTP 400/401/403 system
// error. Ported from AntigravityConnectionError.
//
// When the harness process exited on its own, Err is a *ProcessError, so
//
//	var pe *agy.ProcessError
//	if errors.As(err, &pe) { ... }
//
// finds its exit status.
type ConnectionError struct {
	// Message describes the failure.
	Message string
	// Code is the WebSocket close code when the harness connection dropped,
	// or zero.
	Code int
	// Stderr is the tail of the harness's stderr output, when known.
	Stderr string
	// Err is the underlying error, if any: a *ProcessError when the
	// harness process exited.
	Err error
}

func (e *ConnectionError) Error() string { return e.Message }
func (e *ConnectionError) Unwrap() error { return e.Err }
func (e *ConnectionError) agyError()     {}

// ProcessError reports that the harness process exited on its own: it
// crashed or quit during the launch or mid-session. It is found inside
// the *ConnectionError that reports the failure.
type ProcessError struct {
	// ExitCode is the exit status, or nil when the process was killed by a
	// signal or its status is unknown.
	ExitCode *int
	// Stderr is the tail of the harness's stderr output.
	Stderr string
	// Err is the error cmd.Wait reported, if any.
	Err error
}

func (e *ProcessError) Error() string {
	msg := "agy: harness exited"
	switch {
	case e.ExitCode != nil:
		msg = fmt.Sprintf("agy: harness exited with status %d", *e.ExitCode)
	case e.Err != nil:
		msg = "agy: harness " + e.Err.Error()
	}
	if e.Stderr != "" {
		msg += "\nharness stderr:\n" + e.Stderr
	}
	return msg
}

func (e *ProcessError) Unwrap() error { return e.Err }
func (e *ProcessError) agyError()     {}

// ExecutionError reports that the agent loop terminated with a fatal error
// (a model call failure, a system constraint violation, a denied turn) and
// cannot continue the turn. Ported from AntigravityExecutionError.
type ExecutionError struct {
	Message string
}

func (e *ExecutionError) Error() string { return e.Message }
func (e *ExecutionError) agyError()     {}

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
func (e *CancelledError) agyError()            {}

// ValidationError reports invalid configuration or input. Ported from
// AntigravityValidationError, which also stands in for the pydantic
// validation errors and ValueErrors upstream raises from its validators.
type ValidationError struct {
	Message string
	// Err is the underlying error, if any.
	Err error
}

func (e *ValidationError) Error() string { return e.Message }
func (e *ValidationError) Unwrap() error { return e.Err }
func (e *ValidationError) agyError()     {}

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

func (e *ToolExecutionError) Error() string { return e.Message }
func (e *ToolExecutionError) agyError()     {}

// connectionErrorFrom maps a harness-package error to a *ConnectionError,
// keeping the harness's stderr and close code. When the process exited on
// its own, the result wraps a *ProcessError instead of err.
func connectionErrorFrom(err error) error {
	if err == nil {
		return nil
	}
	if ce, ok := errors.AsType[*harness.ConnectionError](err); ok {
		return &ConnectionError{Message: ce.Error(), Code: int(ce.Code), Stderr: ce.Stderr, Err: processErrorOr(ce.Exit, ce.Stderr, err)}
	}
	if se, ok := errors.AsType[*harness.StartError](err); ok {
		return &ConnectionError{Message: se.Error(), Stderr: se.Stderr, Err: processErrorOr(se.Exit, se.Stderr, err)}
	}
	return &ConnectionError{Message: err.Error(), Err: err}
}

// processErrorOr returns a *ProcessError for exit, or err when the process
// did not exit on its own.
func processErrorOr(exit *harness.ProcessExit, stderr string, err error) error {
	if exit == nil {
		return err
	}
	return &ProcessError{ExitCode: exit.Code, Stderr: stderr, Err: exit.Err}
}
