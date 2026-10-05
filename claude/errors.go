package claude

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
)

// Error is implemented by every error this package originates. It mirrors the
// Python SDK's ClaudeSDKError base class, so callers can write
//
//	var sdkErr claude.Error
//	if errors.As(err, &sdkErr) { ... }
type Error interface {
	error
	claudeSDKError()
}

// baseError carries the message shared by all SDK errors.
type baseError struct {
	Msg string
}

func (e *baseError) Error() string   { return e.Msg }
func (e *baseError) claudeSDKError() {}

// ConnectionError is returned when the SDK cannot talk to the Claude Code CLI.
// Ported from CLIConnectionError.
type ConnectionError struct {
	baseError
	// sentinel is ErrClosed when the error stands for it.
	sentinel error
}

// newConnectionError builds a ConnectionError with the given message.
func newConnectionError(msg string) *ConnectionError {
	return &ConnectionError{baseError: baseError{Msg: msg}}
}

// Unwrap returns the sentinel the error stands for, if any: ErrClosed.
func (e *ConnectionError) Unwrap() error { return e.sentinel }

// ErrClosed matches, with errors.Is, the error of a Client call made after
// Close, and of a TurnStream read after its own Close or after its client's;
// the error itself is a *ConnectionError.
var ErrClosed = errors.New("claude: client is closed")

// closedError reports a call made after Close.
func closedError(what string) *ConnectionError {
	return &ConnectionError{baseError{Msg: what + " is closed"}, ErrClosed}
}

// ErrCLINotFound matches, with errors.Is, every error reporting that the
// claude executable could not be located; the error itself is a
// *CLINotFoundError.
var ErrCLINotFound = errors.New("claude: Claude Code CLI not found")

// CLINotFoundError is returned when the `claude` executable cannot be located.
// It unwraps to a ConnectionError, mirroring the Python class hierarchy, and
// to ErrCLINotFound.
type CLINotFoundError struct {
	baseError
	// CLIPath is the path that was searched for, when one was given.
	CLIPath string
}

// newCLINotFoundError builds a CLINotFoundError. A non-empty cliPath is
// appended to msg.
func newCLINotFoundError(msg, cliPath string) *CLINotFoundError {
	if cliPath != "" {
		msg = msg + ": " + cliPath
	}
	return &CLINotFoundError{baseError{Msg: msg}, cliPath}
}

// Unwrap reports a ConnectionError, so that errors.As with a *ConnectionError
// target matches as `except CLIConnectionError` does in Python, and
// ErrCLINotFound.
func (e *CLINotFoundError) Unwrap() []error {
	return []error{newConnectionError(e.Msg), ErrCLINotFound}
}

// ProcessError is returned when the CLI subprocess fails: it exited with a
// failure status or could not be waited for.
type ProcessError struct {
	baseError
	// ExitCode is the process exit status, or nil when the process did not
	// report one, as when it was ended by a signal.
	ExitCode *int
	// Stderr holds captured standard error output, possibly truncated.
	Stderr string
	// Err is the underlying error, such as the *exec.ExitError of the wait,
	// when there is one.
	Err error
}

// newProcessError builds a ProcessError. exitCode and err may be nil.
func newProcessError(msg string, exitCode *int, stderr string, err error) *ProcessError {
	full := msg
	if exitCode != nil {
		full = fmt.Sprintf("%s (exit code: %d)", full, *exitCode)
	}
	if stderr != "" {
		full = full + "\nError output: " + stderr
	}
	return &ProcessError{baseError{Msg: full}, exitCode, stderr, err}
}

// Unwrap returns the underlying error.
func (e *ProcessError) Unwrap() error { return e.Err }

// ResultError is returned when the CLI ends a run by emitting a result message
// with is_error set and then exits non-zero. It unwraps to a ProcessError.
type ResultError struct {
	baseError
	// ExitCode is the process exit status, when reported.
	ExitCode *int
	// Subtype is the result subtype, e.g. "error_max_turns".
	Subtype string
	// Errors holds the error strings reported by the CLI.
	Errors []string
	// Result is the result text, if any.
	Result string
	// APIErrorStatus is the HTTP status of a failing API call, when reported.
	APIErrorStatus *int
	// TerminalReason explains why the run ended, when reported.
	TerminalReason string
	// SessionID is the session the result belongs to, when reported.
	SessionID string
	// Data is the raw result payload as emitted by the CLI.
	Data map[string]any
	// process is the exit that followed the result, when there was one.
	process *ProcessError
}

// newResultError builds a ResultError from a raw result message payload.
func newResultError(msg string, data map[string]any, exitCode *int) *ResultError {
	full := msg
	if exitCode != nil {
		full = fmt.Sprintf("%s (exit code: %d)", full, *exitCode)
	}
	e := &ResultError{baseError: baseError{Msg: full}, ExitCode: exitCode, Data: data}
	if data == nil {
		return e
	}
	if s, ok := data["subtype"].(string); ok {
		e.Subtype = s
	}
	e.Errors = normalizeResultErrors(data["errors"])
	if s, ok := data["result"].(string); ok {
		e.Result = s
	}
	if n, ok := toInt(data["api_error_status"]); ok {
		e.APIErrorStatus = &n
	}
	if s, ok := data["terminal_reason"].(string); ok {
		e.TerminalReason = s
	}
	if s, ok := data["session_id"].(string); ok {
		e.SessionID = s
	}
	return e
}

// Unwrap reports a ProcessError so that `errors.As` with a *ProcessError target
// matches, as `except ProcessError` does in Python: the CLI's failed exit that
// followed the result, or one built from the result when the CLI has not
// exited.
func (e *ResultError) Unwrap() error {
	if e.process != nil {
		return e.process
	}
	return &ProcessError{baseError{Msg: e.Msg}, e.ExitCode, "", nil}
}

// normalizeResultErrors cleans the `errors` field of a result frame: a bare
// string is promoted to a single-element list, non-strings and blanks dropped.
func normalizeResultErrors(raw any) []string {
	var items []any
	switch v := raw.(type) {
	case string:
		items = []any{v}
	case []any:
		items = v
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
	var out []string
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// JSONDecodeError is returned when a line of CLI output is not valid JSON.
// Ported from CLIJSONDecodeError.
type JSONDecodeError struct {
	baseError
	// Line is the offending output line.
	Line string
	// Err is the underlying decoding error.
	Err error
}

// Unwrap returns the underlying decoding error.
func (e *JSONDecodeError) Unwrap() error { return e.Err }

// MessageParseError is returned when a well-formed JSON payload cannot be
// turned into a typed Message.
type MessageParseError struct {
	baseError
	// Data is the offending payload, when available.
	Data jsontext.Value
}

// newMessageParseError builds a MessageParseError. data may be nil.
func newMessageParseError(msg string, data jsontext.Value) *MessageParseError {
	return &MessageParseError{baseError{Msg: msg}, data}
}
