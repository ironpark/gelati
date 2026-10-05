//go:build !(darwin || dragonfly || freebsd || netbsd || openbsd || linux)

package harness

// osVersion is not reported on this platform.
func osVersion() string { return "" }
