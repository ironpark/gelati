package harness

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/ironpark/gelati/antigravity/internal/wire"
)

// EnvBinaryPath names the environment variable that points at the
// localharness binary, as upstream honours it.
const EnvBinaryPath = "ANTIGRAVITY_HARNESS_PATH"

// BinaryName is the executable name looked up on PATH.
const BinaryName = "localharness"

// ErrBinaryNotFound reports that no localharness binary could be located.
var ErrBinaryNotFound = errors.New("antigravity harness: localharness binary not found; " +
	"set Options.BinaryPath, the " + EnvBinaryPath + " environment variable, or put " + BinaryName + " on PATH")

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

// sdkModule is the module path whose version is reported in ClientInfo.
const sdkModule = "github.com/ironpark/gelati"

// DefaultClientInfo describes this SDK the way upstream describes itself:
// language "go", the module version ("0.0.0-dev" when unknown), the Go
// version without its "go" prefix, runtime.GOOS (which matches Python's
// lowercased platform.system()) and the kernel release.
func DefaultClientInfo() *wire.ClientInfo {
	return &wire.ClientInfo{
		Language:        new("go"),
		Version:         new(sdkVersion()),
		LanguageVersion: new(strings.TrimPrefix(runtime.Version(), "go")),
		OS:              new(runtime.GOOS),
		OSVersion:       new(osVersion()),
	}
}

func sdkVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "0.0.0-dev"
	}
	mod := &info.Main
	for _, dep := range info.Deps {
		if dep.Path == sdkModule {
			mod = dep
			break
		}
	}
	if mod.Path != sdkModule || mod.Version == "" || mod.Version == "(devel)" {
		return "0.0.0-dev"
	}
	return strings.TrimPrefix(mod.Version, "v")
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
	return fmt.Sprintf("antigravity harness: process exited unexpectedly (WebSocket close code %d).\nHarness stderr:\n%s",
		e.Code, stderr)
}

func (e *ConnectionError) Unwrap() error { return e.Err }

// tailBuffer keeps the last max lines written to it and copies all output
// to tee.
type tailBuffer struct {
	tee io.Writer
	max int

	mu      sync.Mutex
	lines   []string // ring buffer of up to max lines, oldest at start
	start   int
	partial []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tee != nil {
		_, _ = b.tee.Write(p)
	}
	b.partial = append(b.partial, p...)
	for {
		i := bytes.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		b.push(strings.TrimRight(string(b.partial[:i]), "\r"))
		b.partial = b.partial[i+1:]
	}
	// Bound a runaway unterminated line.
	if len(b.partial) > 64<<10 {
		b.push(string(b.partial))
		b.partial = nil
	}
	return len(p), nil
}

func (b *tailBuffer) push(line string) {
	switch {
	case b.max <= 0:
	case len(b.lines) < b.max:
		b.lines = append(b.lines, line)
	default:
		b.lines[b.start] = line
		b.start = (b.start + 1) % b.max
	}
}

// String returns the retained lines, including an unterminated last line.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := make([]string, 0, len(b.lines)+1)
	lines = append(lines, b.lines[b.start:]...)
	lines = append(lines, b.lines[:b.start]...)
	if len(b.partial) > 0 {
		lines = append(lines, string(b.partial))
	}
	return strings.Join(lines, "\n")
}
