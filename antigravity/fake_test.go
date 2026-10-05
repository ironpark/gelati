package antigravity

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gelati/antigravity/internal/harness"
	"github.com/ironpark/gelati/antigravity/internal/wire"
)

// fakeTransport is an in-memory harness: tests emit OutputEvents for the
// connection to read and inspect the InputEvents it sends.
type fakeTransport struct {
	events chan *wire.OutputEvent // nil entries end the stream with io.EOF
	sent   chan *wire.InputEvent
	errs   chan error // a value ends the stream with that error

	closeOnce sync.Once
	closed    chan struct{}
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		events: make(chan *wire.OutputEvent, 1024),
		sent:   make(chan *wire.InputEvent, 1024),
		errs:   make(chan error, 1),
		closed: make(chan struct{}),
	}
}

func (f *fakeTransport) Send(ctx context.Context, ev *wire.InputEvent) error {
	select {
	case <-f.closed:
		return harness.ErrClosed
	default:
	}
	select {
	case f.sent <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeTransport) Receive(ctx context.Context) (*wire.OutputEvent, error) {
	select {
	case ev := <-f.events:
		if ev == nil {
			return nil, io.EOF
		}
		return ev, nil
	case err := <-f.errs:
		return nil, err
	case <-f.closed:
		return nil, harness.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeTransport) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

// emit delivers events to the connection, in order.
func (f *fakeTransport) emit(evs ...*wire.OutputEvent) {
	for _, ev := range evs {
		f.events <- ev
	}
}

// hangUp ends the event stream after the events already emitted, as a
// dropped connection does.
func (f *fakeTransport) hangUp() { f.events <- nil }

// next returns the next event the connection sent.
func (f *fakeTransport) next(t *testing.T) *wire.InputEvent {
	t.Helper()
	select {
	case ev := <-f.sent:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the connection to send an event")
		return nil
	}
}

// expectNothingSent fails if the connection sends an event within d.
func (f *fakeTransport) expectNothingSent(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case ev := <-f.sent:
		b, _ := wire.Marshal(ev)
		t.Fatalf("unexpected event sent: %s", b)
	case <-time.After(d):
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newTestConnection returns a connection over a fake transport, closed when
// the test ends.
func newTestConnection(t *testing.T, o connectionOptions) (*Connection, *fakeTransport) {
	t.Helper()
	tr := newFakeTransport()
	if o.logger == nil {
		o.logger = quietLogger()
	}
	c := newConnection(tr, o)
	t.Cleanup(func() { _ = c.Close() })
	return c, tr
}

// mustHooks builds a hook runner or fails the test.
func mustHooks(t *testing.T, hooks ...Hook) *hookRunner {
	t.Helper()
	r, err := newHookRunner(hooks)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// mustTools builds a tool runner or fails the test.
func mustTools(t *testing.T, tools ...*Tool) *toolRunner {
	t.Helper()
	r, err := newToolRunner(tools)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// startTurn puts the connection in a running turn without sending.
func startTurn(c *Connection) {
	c.mu.Lock()
	c.setBusyLocked()
	c.startTurnLocked()
	c.mu.Unlock()
}

// collectSteps drains ReceiveSteps with a timeout.
func collectSteps(t *testing.T, seq func(yield func(*Step, error) bool)) ([]*Step, error) {
	t.Helper()
	type result struct {
		steps []*Step
		err   error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		for s, err := range seq {
			if err != nil {
				r.err = err
				break
			}
			r.steps = append(r.steps, s)
		}
		done <- r
	}()
	select {
	case r := <-done:
		return r.steps, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out receiving steps")
		return nil, nil
	}
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Event builders.

func stepEvent(su *wire.StepUpdate) *wire.OutputEvent { return &wire.OutputEvent{StepUpdate: su} }

func idleEvent(traj, errMsg string) *wire.OutputEvent {
	tsu := &wire.TrajectoryStateUpdate{TrajectoryID: new(traj), State: new(wire.TrajectoryStateUpdateStateFullyIdle)}
	if errMsg != "" {
		tsu.Error = new(errMsg)
	}
	return &wire.OutputEvent{TrajectoryStateUpdate: tsu}
}

func modelText(traj string, idx uint32, state wire.StepUpdateState, target wire.StepUpdateTarget, text string) *wire.OutputEvent {
	return stepEvent(&wire.StepUpdate{
		TrajectoryID: new(traj),
		StepIndex:    new(idx),
		State:        new(state),
		Source:       new(wire.StepUpdateSourceModel),
		Target:       new(target),
		Text:         new(text),
		TextDelta:    new(text),
	})
}

func hookRequest(id string, typ wire.LifecycleHook, fill func(*wire.CallHookRequest)) *wire.OutputEvent {
	req := &wire.CallHookRequest{RequestID: new(id), Type: new(typ), Name: new(string(typ))}
	if fill != nil {
		fill(req)
	}
	return &wire.OutputEvent{CallHookRequest: req}
}

// lockedWriter serializes writes to w with mu.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// slogTo returns a logger writing text records to w under mu.
func slogTo(w io.Writer, mu *sync.Mutex) *slog.Logger {
	return slog.New(slog.NewTextHandler(&lockedWriter{mu, w}, nil))
}
