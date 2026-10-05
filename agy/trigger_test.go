package agy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingConn records trigger notifications.
type recordingConn struct {
	mu   sync.Mutex
	sent []string
}

func (r *recordingConn) SendTriggerNotification(_ context.Context, content string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, content)
	return nil
}

func (r *recordingConn) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sent)
}

func newRunner(conn TriggerConnection, triggers ...Trigger) *TriggerRunner {
	r := NewTriggerRunner(triggers, conn)
	r.logger = quietLogger()
	return r
}

func TestTriggerContextSend(t *testing.T) {
	conn := &recordingConn{}
	if err := NewTriggerContext(conn).Send(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	if got := conn.messages(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("sent %v", got)
	}
}

func TestTriggerRunnerLifecycle(t *testing.T) {
	conn := &recordingConn{}
	var started, stopped atomic.Int32
	blocking := func(ctx context.Context, tc *TriggerContext) error {
		started.Add(1)
		_ = tc.Send(ctx, "started")
		<-ctx.Done()
		stopped.Add(1)
		return nil
	}
	r := newRunner(conn, blocking, blocking)
	if r.IsRunning() {
		t.Fatal("running before start")
	}
	r.Stop() // no-op before start
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err == nil {
		t.Fatal("second Start succeeded")
	}
	waitFor(t, "triggers started", func() bool { return started.Load() == 2 })
	if !r.IsRunning() {
		t.Fatal("not running")
	}
	r.Stop()
	if stopped.Load() != 2 || r.IsRunning() {
		t.Fatalf("stopped %d running %v", stopped.Load(), r.IsRunning())
	}
	r.Stop()
	// The runner can be restarted.
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "restart", func() bool { return started.Load() == 4 })
	r.Stop()
	if got := conn.messages(); len(got) != 4 {
		t.Fatalf("messages %v", got)
	}
	if err := newRunner(conn).Start(); err != nil {
		t.Fatal(err)
	}
}

func TestTriggerFailureDoesNotAffectOthers(t *testing.T) {
	var healthyRuns atomic.Int32
	failing := func(context.Context, *TriggerContext) error { return errors.New("boom") }
	panicking := func(context.Context, *TriggerContext) error { panic("kaboom") }
	healthy := Every(5*time.Millisecond, func(context.Context, *TriggerContext) error {
		healthyRuns.Add(1)
		return nil
	})
	r := newRunner(&recordingConn{}, failing, panicking, healthy)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "healthy trigger keeps running", func() bool { return healthyRuns.Load() >= 3 })
	if !r.IsRunning() {
		t.Fatal("healthy trigger stopped")
	}
	r.Stop()
}

func TestEvery(t *testing.T) {
	var n atomic.Int32
	trigger := Every(10*time.Millisecond, func(ctx context.Context, tc *TriggerContext) error {
		if n.Add(1) == 3 {
			return errors.New("stop after three")
		}
		return nil
	})
	start := time.Now()
	if err := trigger(t.Context(), NewTriggerContext(&recordingConn{})); err == nil || n.Load() != 3 {
		t.Fatalf("Every returned %v after %d calls", err, n.Load())
	}
	if time.Since(start) < 25*time.Millisecond {
		t.Fatal("first call did not wait an interval")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("Every(0) did not panic")
		}
	}()
	Every(0, nil)
}

func TestOnFileChange(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.txt")
	gone := filepath.Join(dir, "gone.txt")
	for _, p := range []string{existing, gone} {
		if err := os.WriteFile(p, []byte("v1"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	batches := make(chan []FileChange, 16)
	trigger := OnFileChangeEvery(dir, 20*time.Millisecond, func(_ context.Context, _ *TriggerContext, changes []FileChange) error {
		batches <- changes
		return nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- trigger(ctx, NewTriggerContext(&recordingConn{})) }()
	time.Sleep(50 * time.Millisecond) // let the first snapshot be taken

	added := filepath.Join(dir, "sub", "new.txt")
	if err := os.MkdirAll(filepath.Dir(added), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(added, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("v2 longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	seen := map[string]FileChangeKind{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 3 {
		select {
		case b := <-batches:
			for _, c := range b {
				seen[c.Path] = c.Kind
			}
		case <-deadline:
			t.Fatalf("changes seen: %v", seen)
		}
	}
	absDir, _ := filepath.Abs(dir)
	want := map[string]FileChangeKind{
		filepath.Join(absDir, "sub", "new.txt"): FileAdded,
		filepath.Join(absDir, "existing.txt"):   FileModified,
		filepath.Join(absDir, "gone.txt"):       FileDeleted,
	}
	for p, k := range want {
		if seen[p] != k {
			t.Errorf("%s: %q, want %q (all: %v)", p, seen[p], k, seen)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
