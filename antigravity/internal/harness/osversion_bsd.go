//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package harness

import "syscall"

// osVersion returns the kernel release, as Python's platform.release does.
func osVersion() string {
	v, err := syscall.Sysctl("kern.osrelease")
	if err != nil {
		return ""
	}
	return v
}
