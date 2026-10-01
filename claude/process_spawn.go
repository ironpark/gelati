package claude

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
)

// SpawnOptions describes the CLI process the SDK wants to start. It is passed
// to Options.Spawn.
type SpawnOptions struct {
	// Command is the program to run: the CLI path, or the JavaScript
	// runtime when the CLI is a script.
	Command string
	// Args are the program arguments, without Command itself.
	Args []string
	// Cwd is the working directory; empty means the current directory.
	Cwd string
	// Env is the complete environment as KEY=VALUE pairs, sorted by key.
	Env []string
}

// SpawnedProcess is a running CLI process returned by Options.Spawn.
//
// The SDK writes stream-json frames to Stdin and reads them from Stdout.
// On close it closes Stdin, waits about two seconds for the process to exit,
// then calls Signal with SIGTERM, and calls Kill five seconds later if the
// process is still running.
type SpawnedProcess interface {
	// Stdin is the process's standard input.
	Stdin() io.WriteCloser
	// Stdout is the process's standard output.
	Stdout() io.Reader
	// Stderr is the process's standard error, or nil when it is not
	// captured. It is read line by line into Options.Stderr.
	Stderr() io.Reader
	// Wait blocks until the process exits. The SDK calls it once, after
	// Stdout and Stderr reach EOF or while shutting down. A non-nil error
	// that has an ExitCode() int method, such as *exec.ExitError, reports
	// a failed exit.
	Wait() error
	// Signal asks the process to stop. An error is ignored.
	Signal(sig os.Signal) error
	// Kill stops the process immediately. An error is ignored.
	Kill() error
}

// SpawnLocalProcess starts opts as a local child process. It is the default
// for Options.Spawn, and a building block for a custom spawner that only
// rewrites the command, e.g. to prefix it with a wrapper. The process is
// killed when ctx is cancelled.
func SpawnLocalProcess(ctx context.Context, opts SpawnOptions) (SpawnedProcess, error) {
	cmd := exec.CommandContext(ctx, opts.Command, opts.Args...)
	cmd.Dir = opts.Cwd
	cmd.Env = opts.Env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &localProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

// localProcess adapts an *exec.Cmd to SpawnedProcess.
type localProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

func (p *localProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *localProcess) Stdout() io.Reader     { return p.stdout }
func (p *localProcess) Stderr() io.Reader     { return p.stderr }
func (p *localProcess) Wait() error           { return p.cmd.Wait() }
func (p *localProcess) Kill() error           { return p.cmd.Process.Kill() }

func (p *localProcess) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }

// jsExtensions mark a CLI path that must run under a JavaScript runtime.
var jsExtensions = []string{".js", ".mjs", ".tsx", ".ts", ".jsx"}

// resolveCommand picks the program and argument list for cliPath, as the
// TypeScript SDK does: a native CLI runs directly with ExecutableArgs before
// the flags; a JavaScript CLI runs under Executable (default "node") with
// ExecutableArgs, then the script path, then the flags.
func resolveCommand(cliPath string, opts *Options, flags []string) (string, []string) {
	args := make([]string, 0, len(opts.ExecutableArgs)+1+len(flags))
	args = append(args, opts.ExecutableArgs...)
	for _, ext := range jsExtensions {
		if strings.HasSuffix(cliPath, ext) {
			runtime := opts.Executable
			if runtime == "" {
				runtime = "node"
			}
			args = append(args, cliPath)
			return runtime, append(args, flags...)
		}
	}
	return cliPath, append(args, flags...)
}

// exitCoder is implemented by process errors that carry an exit status, such
// as *exec.ExitError.
type exitCoder interface {
	error
	ExitCode() int
}
