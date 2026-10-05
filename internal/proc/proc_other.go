//go:build !unix

package proc

import (
	"os"
	"os/exec"
)

func setGroup(*exec.Cmd) {}

// Signal sends sig to p.
func Signal(p *os.Process, sig os.Signal) error { return p.Signal(sig) }

// Kill kills p.
func Kill(p *os.Process) error { return p.Kill() }

// terminate kills p: there is no SIGTERM, and Python's Popen.terminate is
// TerminateProcess there too.
func terminate(p Stopper) error { return p.Kill() }
