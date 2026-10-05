//go:build unix

package harness

import (
	"os"
	"syscall"
)

// terminate asks the process to exit (SIGTERM), like Popen.terminate.
func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
