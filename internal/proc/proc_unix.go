//go:build unix

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func setGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Signal sends sig to p's process group when p leads one (see NewGroup), and
// to p alone otherwise.
func Signal(p *os.Process, sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return p.Signal(sig)
	}
	// A child that leads its own group has a process group id equal to its
	// pid; any other child is signalled directly.
	if pgid, err := syscall.Getpgid(p.Pid); err == nil && pgid == p.Pid {
		if err := syscall.Kill(-pgid, s); err == nil || !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return p.Signal(sig)
}

// Kill sends SIGKILL to p's process group (see Signal).
func Kill(p *os.Process) error { return Signal(p, syscall.SIGKILL) }

func terminate(p Stopper) error { return p.Signal(syscall.SIGTERM) }
