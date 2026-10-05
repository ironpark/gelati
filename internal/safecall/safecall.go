// Package safecall turns panics in user callbacks into errors, so that a
// buggy hook, tool or handler fails its own call instead of the process.
package safecall

import "fmt"

// Recover, deferred directly, stops a panic and stores it in *err as
// "<what> panicked: <value>":
//
//	defer safecall.Recover(&err, "hook")
func Recover(err *error, what string) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("%s panicked: %v", what, r)
	}
}
