package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gelati/internal/jsonx"
	"github.com/ironpark/gelati/internal/proc"
	"github.com/ironpark/gelati/internal/tailbuf"
)

// stderrTailLines caps how many stderr lines are retained for error reports.
const stderrTailLines = 100

// Graceful shutdown timings, matching the TypeScript SDK: after stdin is
// closed the CLI gets defaultGracefulExitTimeout to exit on its own, then
// SIGTERM, then defaultForceKillTimeout before it is killed.
const (
	defaultGracefulExitTimeout = 2 * time.Second
	defaultForceKillTimeout    = 5 * time.Second
)

// subprocessTransport runs the Claude Code CLI as a child process and speaks
// stream-json over its stdin and stdout.
type subprocessTransport struct {
	opts *Options
	// launch is opts resolved by resolveLaunch; Connect renders the command
	// line from it.
	launch *launchConfig

	// writeMu serializes frames on stdin. It is separate from mu so that a
	// Write blocked on a full pipe never holds mu: Close and EndInput can
	// still close stdin, which unblocks it.
	writeMu sync.Mutex

	mu      sync.Mutex
	proc    SpawnedProcess
	stdin   io.WriteCloser
	stdout  io.Reader
	ready   bool
	exitErr error
	closed  bool

	cliPath string

	// closeStdin closes the process's stdin exactly once; it is safe to
	// call without holding mu.
	closeStdin func() error

	waitOnce sync.Once
	waitErr  error
	exited   chan struct{}

	stderrTail tailbuf.Buffer
	stderrDone chan struct{}

	termOnce        sync.Once
	gracefulTimeout time.Duration
	killTimeout     time.Duration

	cancel context.CancelFunc
}

// newSubprocessTransport builds a transport for opts, which launch resolves.
// The CLI is located, and its command line and environment rendered, at
// Connect time.
func newSubprocessTransport(opts *Options, launch *launchConfig) *subprocessTransport {
	return &subprocessTransport{
		opts:            opts,
		launch:          launch,
		stderrTail:      tailbuf.Buffer{Max: stderrTailLines},
		gracefulTimeout: defaultGracefulExitTimeout,
		killTimeout:     defaultForceKillTimeout,
	}
}

// Connect locates the CLI, builds its command line and starts it.
func (t *subprocessTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.proc != nil {
		return nil
	}
	if t.opts.User != "" {
		return newConnectionError(
			"Options.User is not supported by the subprocess transport; " +
				"run the process as the desired user instead")
	}

	spawn := t.opts.Spawn
	cliPath := t.opts.CLIPath
	if cliPath == "" {
		if spawn != nil {
			// A custom spawner runs the CLI somewhere else; discovery on
			// this machine says nothing about that environment.
			cliPath = "claude"
		} else {
			found, err := findCLI()
			if err != nil {
				return err
			}
			cliPath = found
		}
	}
	t.cliPath = cliPath

	if t.opts.Cwd != "" && spawn == nil {
		if info, err := os.Stat(t.opts.Cwd); err != nil || !info.IsDir() {
			return newConnectionError("Working directory does not exist: " + t.opts.Cwd)
		}
	}
	if spawn == nil {
		spawn = SpawnLocalProcess
	}

	// The process is bound to a cancellable child of ctx. Both an explicit
	// Close and a cancelled ctx run the graceful shutdown, which cancels
	// runCtx last, so a spawner may tie forced teardown to it.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t.cancel = cancel

	command, argv := resolveCommand(cliPath, t.opts, t.launch.commandArgs(t.opts))
	child, err := spawn(runCtx, SpawnOptions{
		Command: command,
		Args:    argv,
		Cwd:     t.opts.Cwd,
		Env:     buildEnv(t.opts),
	})
	if err == nil && child == nil {
		err = errors.New("Options.Spawn returned no process")
	}
	if err != nil {
		cancel()
		if proc.IsNotFound(err) {
			return newCLINotFoundError("Claude Code not found at", cliPath)
		}
		return newConnectionError("Failed to start Claude Code: " + err.Error())
	}

	stdin := child.Stdin()
	t.proc = child
	t.stdin = stdin
	t.stdout = child.Stdout()
	t.closeStdin = sync.OnceValue(stdin.Close)
	t.exited = make(chan struct{})
	t.ready = true
	t.stderrDone = make(chan struct{})
	if stderr := child.Stderr(); stderr != nil {
		go t.pumpStderr(stderr)
	} else {
		close(t.stderrDone)
	}
	go func() {
		select {
		case <-ctx.Done():
			t.terminate()
		case <-runCtx.Done():
		}
	}()
	return nil
}

// pumpStderr forwards the CLI's stderr to the callback line by line and keeps a
// tail for inclusion in process errors.
func (t *subprocessTransport) pumpStderr(r io.Reader) {
	defer close(t.stderrDone)
	scanner := bufio.NewScanner(r)
	// The initial capacity must not exceed the limit: the scanner accepts
	// lines up to the larger of the two.
	maxSize := t.opts.bufferSize()
	scanner.Buffer(make([]byte, 0, min(64*1024, maxSize)), maxSize)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n \t")
		if line == "" {
			continue
		}
		t.stderrTail.AddLine(line)
		if t.opts.Stderr != nil {
			// Isolated per line: a panicking callback must not stop the
			// pump and silently drop every later line.
			func() {
				defer func() { _ = recover() }()
				t.opts.Stderr(line)
			}()
		}
	}
	// Drain the rest after an oversized line, so the child never blocks on
	// a full stderr pipe.
	_, _ = io.Copy(io.Discard, r)
}

// wait reaps the child process exactly once and reports its status. It first
// drains the stderr pump, since exec closes the pipes when Wait returns.
func (t *subprocessTransport) wait() error {
	t.waitOnce.Do(func() {
		if t.stderrDone != nil {
			<-t.stderrDone
		}
		t.waitErr = t.proc.Wait()
		close(t.exited)
	})
	return t.waitErr
}

// terminate shuts the process down once, in the background, the way the
// TypeScript SDK does: close stdin, give the CLI a grace period to exit,
// send SIGTERM, and kill it if it is still running after killTimeout. On
// Windows, where SIGTERM cannot be delivered, the process is killed after
// both periods.
func (t *subprocessTransport) terminate() {
	t.termOnce.Do(func() {
		go func() {
			defer t.cancel()
			// Closing stdin first also unblocks a Write stuck on a full
			// pipe.
			_ = t.closeStdin()
			t.mu.Lock()
			t.ready = false
			t.mu.Unlock()
			go func() { _ = t.wait() }()
			_ = proc.Stop(t.proc, t.exited, t.gracefulTimeout, t.killTimeout)
		}()
	})
}

// Write sends one frame to the CLI's stdin. mu is not held while writing,
// so a write stuck on a full pipe cannot block Close or EndInput.
func (t *subprocessTransport) Write(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	stdin, ready, exitErr := t.stdin, t.ready, t.exitErr
	t.mu.Unlock()
	if !ready || stdin == nil {
		return newConnectionError("transport is not ready for writing")
	}
	if exitErr != nil {
		return newConnectionError("Cannot write to process that exited with error: " + exitErr.Error())
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		// One write per frame, newline included.
		data = append(slices.Clip(data), '\n')
	}
	if _, err := stdin.Write(data); err != nil {
		t.mu.Lock()
		t.ready = false
		if t.exitErr == nil {
			t.exitErr = err
		}
		t.mu.Unlock()
		return newConnectionError("Failed to write to process stdin: " + err.Error())
	}
	return nil
}

// EndInput closes the CLI's stdin.
func (t *subprocessTransport) EndInput() error {
	t.mu.Lock()
	if t.stdin == nil {
		t.mu.Unlock()
		return nil
	}
	t.stdin = nil
	t.ready = false
	t.mu.Unlock()
	if err := t.closeStdin(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

// Ready reports whether the CLI is running and accepting writes.
func (t *subprocessTransport) Ready() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ready
}

// Close terminates the CLI, gracefully first (see terminate), and releases
// its resources. It returns once the process has exited.
func (t *subprocessTransport) Close() error {
	t.mu.Lock()
	if t.closed || t.proc == nil {
		t.closed = true
		t.ready = false
		cancel := t.cancel
		t.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
	t.closed = true
	t.ready = false
	t.stdin = nil
	t.mu.Unlock()

	t.terminate()
	err := t.wait()
	if _, ok := errors.AsType[exitCoder](err); ok {
		// A non-zero status after an explicit Close is expected.
		return nil
	}
	return err
}

// ReadMessages yields the CLI's newline-delimited JSON output.
func (t *subprocessTransport) ReadMessages() iter.Seq2[jsontext.Value, error] {
	return func(yield func(jsontext.Value, error) bool) {
		t.mu.Lock()
		stdout, child := t.stdout, t.proc
		t.mu.Unlock()
		if stdout == nil || child == nil {
			yield(nil, newConnectionError("not connected"))
			return
		}

		maxSize := t.opts.bufferSize()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, min(64*1024, maxSize)), maxSize)
		for scanner.Scan() {
			// The scanner reuses its buffer, so the line is copied once,
			// after it is known to be a frame.
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 || line[0] != '{' {
				// Some CLI builds write diagnostics such as
				// "[SandboxDebug] ..." to stdout; they carry no message.
				continue
			}
			if !jsontext.Value(line).IsValid(jsonx.Foreign) {
				// Like the TypeScript SDK, a line that is not valid JSON is
				// skipped rather than ending the session.
				continue
			}
			if !yield(jsontext.Value(bytes.Clone(line)), nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				msg := fmt.Sprintf(
					"JSON message exceeded maximum buffer size of %d bytes", maxSize)
				yield(nil, &JSONDecodeError{baseError{Msg: msg}, "", err})
				return
			}
			if !errors.Is(err, os.ErrClosed) {
				yield(nil, newConnectionError("Failed to read from process stdout: "+err.Error()))
				return
			}
		}

		// Output is exhausted: reap the process and report a failure exit.
		waitErr := t.wait()
		t.mu.Lock()
		t.ready = false
		t.mu.Unlock()
		if exitErr, ok := errors.AsType[exitCoder](waitErr); ok {
			// A negative status means the process was ended by a signal
			// and has no exit code.
			var exitCode *int
			msg := "Command was terminated by a signal"
			if code := exitErr.ExitCode(); code >= 0 {
				exitCode = &code
				msg = fmt.Sprintf("Command failed with exit code %d", code)
			}
			perr := newProcessError(msg, exitCode, strings.TrimSpace(t.stderrTail.String()), waitErr)
			t.mu.Lock()
			t.exitErr = perr
			t.mu.Unlock()
			yield(nil, perr)
		}
	}
}
