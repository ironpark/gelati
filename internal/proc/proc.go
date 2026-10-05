// Package proc holds the child-process handling shared by the SDK packages:
// locating the CLI, building its environment, starting it in a process group
// of its own, and the graceful stop sequence.
package proc

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/ironpark/gelati/internal/lifecycle"
)

// Stopper is a running child the stop sequence can signal. *os.Process
// values started with NewGroup should be wrapped with Group.
type Stopper interface {
	Signal(sig os.Signal) error
	Kill() error
}

// Stop waits up to grace for exited to close, then asks the process to exit
// (SIGTERM; a kill on Windows) and waits up to killAfter, then kills it and
// waits up to killAfter again. Callers close the child's stdin first, which
// is how the CLIs are asked to exit. It fails only when the process is still
// running at the end.
func Stop(p Stopper, exited <-chan struct{}, grace, killAfter time.Duration) error {
	if lifecycle.WaitClosed(exited, grace) {
		return nil
	}
	_ = terminate(p)
	if lifecycle.WaitClosed(exited, killAfter) {
		return nil
	}
	_ = p.Kill()
	if lifecycle.WaitClosed(exited, killAfter) {
		return nil
	}
	return errors.New("process did not exit after kill")
}

// Group returns the Stopper of a process started after NewGroup, which
// signals its whole process group: whatever the CLI started (shells, MCP
// servers) is stopped with it.
func Group(p *os.Process) Stopper { return group{p} }

type group struct{ p *os.Process }

func (g group) Signal(sig os.Signal) error { return Signal(g.p, sig) }
func (g group) Kill() error                { return Kill(g.p) }

// NewGroup makes cmd start in a process group of its own, so that Signal and
// Kill reach the processes it starts too, and so that a terminal's Ctrl-C
// reaches only this program, which then stops the child itself. For a
// command made with exec.CommandContext, cancelling the context then kills
// the whole group. It has no effect on Windows.
func NewGroup(cmd *exec.Cmd) {
	setGroup(cmd)
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { return Kill(cmd.Process) }
	}
}

// Environ returns this process's environment followed by overrides, sorted
// by key. exec.Cmd keeps the last value of a repeated key (ignoring case on
// Windows, as the system does), so the overrides win.
func Environ(overrides map[string]string) []string {
	env := os.Environ()
	for _, k := range slices.Sorted(maps.Keys(overrides)) {
		env = append(env, k+"="+overrides[k])
	}
	return env
}
