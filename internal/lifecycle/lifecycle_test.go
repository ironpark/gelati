package lifecycle

import (
	"errors"
	"testing"
)

func TestDone(t *testing.T) {
	var d Done
	ch := d.C()
	select {
	case <-ch:
		t.Fatal("closed before Finish")
	default:
	}
	if d.Err() != nil || d.Ended() {
		t.Fatal("Err or Ended before Finish")
	}
	first := errors.New("first")
	if !d.Finish(first) || d.Finish(errors.New("second")) {
		t.Fatal("only the first Finish should take effect")
	}
	<-ch
	if d.Err() != first || !d.Ended() {
		t.Fatalf("Err = %v", d.Err())
	}
	<-Closed()
}
