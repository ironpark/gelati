package safecall

import "testing"

func TestRecover(t *testing.T) {
	call := func(fn func() error) (err error) {
		defer Recover(&err, "hook")
		return fn()
	}
	if err := call(func() error { panic("boom") }); err == nil || err.Error() != "hook panicked: boom" {
		t.Fatalf("err = %v", err)
	}
	if err := call(func() error { return nil }); err != nil {
		t.Fatalf("err = %v", err)
	}
}
