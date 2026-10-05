package lifecycle

import (
	"context"
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
}

func TestCancelIfDone(t *testing.T) {
	calls := 0
	cancel := func(ctx context.Context) error {
		calls++
		if ctx.Err() != nil {
			t.Error("cancel got a done context")
		}
		return nil
	}
	if CancelIfDone(context.Background(), context.Canceled, cancel) {
		t.Fatal("cancelled while ctx is live")
	}
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if CancelIfDone(ctx, errors.New("other"), cancel) {
		t.Fatal("cancelled for an unrelated error")
	}
	if !CancelIfDone(ctx, context.Canceled, cancel) || calls != 1 {
		t.Fatalf("CancelIfDone did not cancel: calls = %d", calls)
	}
}
