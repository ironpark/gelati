package harness

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/coder/websocket"
	"github.com/ironpark/gelati/agy/internal/wire"
	"github.com/ironpark/gelati/internal/buildinfo"
)

// EnvBinaryPath names the environment variable that points at the
// localharness binary, as upstream honours it.
const EnvBinaryPath = "ANTIGRAVITY_HARNESS_PATH"

// BinaryName is the executable name looked up on PATH.
const BinaryName = "localharness"

// ErrBinaryNotFound reports that no localharness binary could be located.
var ErrBinaryNotFound = errors.New("agy harness: localharness binary not found; " +
	"set Config.CLIPath, the " + EnvBinaryPath + " environment variable, or put " + BinaryName + " on PATH")

// FindBinary locates the localharness binary in upstream's order, minus the
// Python-wheel lookups: EnvBinaryPath in env (the extra variables passed to
// the harness), then EnvBinaryPath in the process environment, then
// BinaryName on PATH.
func FindBinary(env map[string]string) (string, error) {
	if p, ok := env[EnvBinaryPath]; ok && p != "" {
		return p, nil
	}
	if p := os.Getenv(EnvBinaryPath); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath(BinaryName); err == nil {
		return p, nil
	}
	return "", ErrBinaryNotFound
}

// DefaultClientInfo describes this SDK the way upstream describes itself:
// language "go", the module version ("0.0.0-dev" when unknown), the Go
// version without its "go" prefix, runtime.GOOS (which matches Python's
// lowercased platform.system()) and the kernel release.
func DefaultClientInfo() *wire.ClientInfo {
	return wire.ClientInfo_builder{
		Language:        new("go"),
		Version:         new(buildinfo.Version()),
		LanguageVersion: new(strings.TrimPrefix(runtime.Version(), "go")),
		Os:              new(runtime.GOOS),
		OsVersion:       new(osVersion()),
	}.Build()
}

// StartError reports a failed launch, handshake, connection or conversation
// initialization. The process has been killed.
type StartError struct {
	Err error
	// Stderr is the tail of the harness's stderr output.
	Stderr string
}

func (e *StartError) Error() string {
	if _, ok := errors.AsType[*ConnectionError](e.Err); ok {
		return e.Err.Error() // already carries the stderr
	}
	stderr := e.Stderr
	if stderr == "" {
		stderr = "(no stderr output)"
	}
	return fmt.Sprintf("%v\nHarness stderr:\n%s", e.Err, stderr)
}

func (e *StartError) Unwrap() error { return e.Err }

// ConnectionError reports that the WebSocket connection to the harness
// dropped while the Harness was open, usually because the process exited.
type ConnectionError struct {
	// Code is the WebSocket close code; StatusAbnormalClosure (1006) when
	// the connection dropped without a close frame.
	Code websocket.StatusCode
	// Stderr is the tail of the harness's stderr output.
	Stderr string
	Err    error
}

func (e *ConnectionError) Error() string {
	stderr := e.Stderr
	if stderr == "" {
		stderr = "(no stderr output)"
	}
	return fmt.Sprintf("agy harness: process exited unexpectedly (WebSocket close code %d).\nHarness stderr:\n%s",
		e.Code, stderr)
}

func (e *ConnectionError) Unwrap() error { return e.Err }
