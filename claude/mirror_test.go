package claude

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// mirrorStoreFake is a SessionStore that records every Append call. The zero
// value is ready to use. appendHook, when set before use, runs first and may
// block or fail the call.
type mirrorStoreFake struct {
	appendHook func(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error

	mu    sync.Mutex
	calls []mirrorAppendCall
	data  map[SessionKey][]SessionStoreEntry
}

type mirrorAppendCall struct {
	key     SessionKey
	entries []SessionStoreEntry
}

func (s *mirrorStoreFake) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	if s.appendHook != nil {
		if err := s.appendHook(ctx, key, entries); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, mirrorAppendCall{key: key, entries: slices.Clone(entries)})
	if s.data == nil {
		s.data = map[SessionKey][]SessionStoreEntry{}
	}
	s.data[key] = append(s.data[key], entries...)
	return nil
}

func (s *mirrorStoreFake) Load(_ context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.data[key]), nil
}

func (s *mirrorStoreFake) appendCalls() []mirrorAppendCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// ns returns the "n" field of each entry across calls.
func mirrorNs(calls []mirrorAppendCall) []int {
	var out []int
	for _, c := range calls {
		for _, e := range c.entries {
			n, _ := toInt(e["n"])
			out = append(out, n)
		}
	}
	return out
}

var mirrorProjectsDir = filepath.Join(string(filepath.Separator), "home", "user", ".claude", "projects")

func mirrorPath(parts ...string) string {
	return filepath.Join(append([]string{mirrorProjectsDir}, parts...)...)
}

func mirrorMainPath(project, session string) string {
	return mirrorPath(project, session+".jsonl")
}

// mirrorErrors collects onError reports.
type mirrorErrors struct {
	mu   sync.Mutex
	list []mirrorFailure
}

func (m *mirrorErrors) record(key *SessionKey, err string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.list = append(m.list, mirrorFailure{key: key, msg: err})
}

func (m *mirrorErrors) get() []mirrorFailure {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.list)
}

// sleepRecorder replaces the retry backoff with an instant, recorded wait.
type sleepRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleepRecorder) get() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.waits)
}

func newTestMirrorBatcher(t *testing.T, store SessionStore, errs *mirrorErrors, maxEntries, maxBytes int) (*transcriptMirrorBatcher, *sleepRecorder) {
	t.Helper()
	var onError func(*SessionKey, string)
	if errs != nil {
		onError = errs.record
	}
	b := newTranscriptMirrorBatcher(store, mirrorProjectsDir, onError, maxEntries, maxBytes)
	rec := &sleepRecorder{}
	b.sleep = rec.sleep
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		b.close(ctx)
	})
	return b, rec
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// filePathToSessionKey
// ---------------------------------------------------------------------------

func TestFilePathToSessionKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		dir  string
		want SessionKey
		ok   bool
	}{
		{"main", mirrorPath("-home-user-repo", "abc-123.jsonl"), mirrorProjectsDir,
			SessionKey{ProjectKey: "-home-user-repo", SessionID: "abc-123"}, true},
		{"subagent", mirrorPath("-home-user-repo", "abc-123", "subagents", "agent-xyz.jsonl"), mirrorProjectsDir,
			SessionKey{ProjectKey: "-home-user-repo", SessionID: "abc-123", Subpath: "subagents/agent-xyz"}, true},
		{"nestedSubagent", mirrorPath("proj", "sess", "subagents", "nested", "agent-1.jsonl"), mirrorProjectsDir,
			SessionKey{ProjectKey: "proj", SessionID: "sess", Subpath: "subagents/nested/agent-1"}, true},
		{"subagentWithoutSuffix", mirrorPath("proj", "sess", "subagents", "agent-1.meta"), mirrorProjectsDir,
			SessionKey{ProjectKey: "proj", SessionID: "sess", Subpath: "subagents/agent-1.meta"}, true},
		{"trailingSeparator", mirrorPath("-home-user-repo", "abc-123.jsonl"), mirrorProjectsDir + string(filepath.Separator),
			SessionKey{ProjectKey: "-home-user-repo", SessionID: "abc-123"}, true},
		{"trailingSeparatorSubagent", mirrorPath("p", "s", "subagents", "agent-x.jsonl"), mirrorProjectsDir + string(filepath.Separator),
			SessionKey{ProjectKey: "p", SessionID: "s", Subpath: "subagents/agent-x"}, true},
		{"uncleanPath", mirrorProjectsDir + string(filepath.Separator) + filepath.Join("p", "x", "..", "s.jsonl"), mirrorProjectsDir,
			SessionKey{ProjectKey: "p", SessionID: "s"}, true},
		{"outside", filepath.Join(string(filepath.Separator), "elsewhere", "proj", "sess.jsonl"), mirrorProjectsDir, SessionKey{}, false},
		{"sibling", filepath.Join(mirrorProjectsDir+"-other", "proj", "sess.jsonl"), mirrorProjectsDir, SessionKey{}, false},
		{"escapes", mirrorPath("..", "proj", "sess.jsonl"), mirrorProjectsDir, SessionKey{}, false},
		{"tooFewParts", mirrorPath("proj-only.jsonl"), mirrorProjectsDir, SessionKey{}, false},
		{"projectsDirItself", mirrorProjectsDir, mirrorProjectsDir, SessionKey{}, false},
		{"threeParts", mirrorPath("proj", "sess", "weird.jsonl"), mirrorProjectsDir, SessionKey{}, false},
		{"mainWithoutSuffix", mirrorPath("proj", "sess.txt"), mirrorProjectsDir, SessionKey{}, false},
		{"empty", "", mirrorProjectsDir, SessionKey{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := filePathToSessionKey(tc.path, tc.dir)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("filePathToSessionKey(%q, %q) = %+v, %v; want %+v, %v", tc.path, tc.dir, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// transcriptMirrorBatcher
// ---------------------------------------------------------------------------

func TestMirrorBatcherEnqueueThenFlush(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{}
	b, _ := newTestMirrorBatcher(t, store, nil, storeAppendBatchEntries, storeAppendBatchBytes)
	b.enqueue(mirrorMainPath("proj", "sess"), []SessionStoreEntry{{"type": "user", "n": 1}})
	b.enqueue(mirrorMainPath("proj", "sess"), []SessionStoreEntry{{"type": "assistant", "n": 2}})
	time.Sleep(10 * time.Millisecond)
	if calls := store.appendCalls(); len(calls) != 0 {
		t.Fatalf("appended before flush: %+v", calls)
	}

	b.flush(t.Context())

	calls := store.appendCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1 coalesced append", len(calls))
	}
	if want := (SessionKey{ProjectKey: "proj", SessionID: "sess"}); calls[0].key != want {
		t.Fatalf("key = %+v, want %+v", calls[0].key, want)
	}
	want := []SessionStoreEntry{{"type": "user", "n": 1}, {"type": "assistant", "n": 2}}
	if !reflect.DeepEqual(calls[0].entries, want) {
		t.Fatalf("entries = %v, want %v", calls[0].entries, want)
	}
}

func TestMirrorBatcherFlushWithNothingPending(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{}
	b, _ := newTestMirrorBatcher(t, store, nil, storeAppendBatchEntries, storeAppendBatchBytes)
	b.flush(t.Context())
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{})
	b.flush(t.Context())
	// An empty batch must not reach the store: some adapters would create a
	// phantom key.
	if calls := store.appendCalls(); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none", calls)
	}
}

func TestMirrorBatcherCoalescesPerPathInOrder(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{}
	b, _ := newTestMirrorBatcher(t, store, nil, storeAppendBatchEntries, storeAppendBatchBytes)
	b.enqueue(mirrorMainPath("p", "a"), []SessionStoreEntry{{"n": 1}})
	b.enqueue(mirrorMainPath("p", "b"), []SessionStoreEntry{{"n": 2}})
	b.enqueue(mirrorMainPath("p", "a"), []SessionStoreEntry{{"n": 3}})
	b.flush(t.Context())

	calls := store.appendCalls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0].key.SessionID != "a" || !slices.Equal(mirrorNs(calls[:1]), []int{1, 3}) {
		t.Fatalf("first call = %+v", calls[0])
	}
	if calls[1].key.SessionID != "b" || !slices.Equal(mirrorNs(calls[1:]), []int{2}) {
		t.Fatalf("second call = %+v", calls[1])
	}
}

func TestMirrorBatcherThresholds(t *testing.T) {
	t.Parallel()
	t.Run("entries", func(t *testing.T) {
		t.Parallel()
		store := &mirrorStoreFake{}
		b, _ := newTestMirrorBatcher(t, store, nil, 5, storeAppendBatchBytes)
		b.enqueue(mirrorMainPath("p", "s"), make([]SessionStoreEntry, 5))
		time.Sleep(10 * time.Millisecond)
		if n := len(store.appendCalls()); n != 0 {
			t.Fatalf("flushed at the threshold: %d calls", n)
		}
		b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"type": "x"}})
		waitUntil(t, "eager flush", func() bool { return len(store.appendCalls()) == 1 })
		if n := len(store.appendCalls()[0].entries); n != 6 {
			t.Fatalf("flushed %d entries, want 6", n)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		t.Parallel()
		store := &mirrorStoreFake{}
		b, _ := newTestMirrorBatcher(t, store, nil, storeAppendBatchEntries, 100)
		b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"type": "x", "blob": strings.Repeat("a", 200)}})
		waitUntil(t, "eager flush", func() bool { return len(store.appendCalls()) == 1 })
	})
}

func TestNewMirrorBatcherForOptions(t *testing.T) {
	t.Parallel()
	if b := newMirrorBatcherForOptions(&Options{}, mirrorProjectsDir, nil); b != nil {
		t.Fatal("batcher built without a SessionStore")
	}
	if b := newMirrorBatcherForOptions(nil, mirrorProjectsDir, nil); b != nil {
		t.Fatal("batcher built for nil options")
	}
	cases := []struct {
		mode               SessionStoreFlushMode
		wantEntries, bytes int
	}{
		{"", storeAppendBatchEntries, storeAppendBatchBytes},
		{SessionStoreFlushBatched, storeAppendBatchEntries, storeAppendBatchBytes},
		{SessionStoreFlushEager, 0, 0},
	}
	for _, tc := range cases {
		b := newMirrorBatcherForOptions(&Options{SessionStore: &mirrorStoreFake{}, SessionStoreFlush: tc.mode}, mirrorProjectsDir, nil)
		if b.maxPendingEntries != tc.wantEntries || b.maxPendingBytes != tc.bytes {
			t.Fatalf("mode %q: thresholds %d/%d, want %d/%d", tc.mode, b.maxPendingEntries, b.maxPendingBytes, tc.wantEntries, tc.bytes)
		}
		if b.projectsDir != mirrorProjectsDir || b.sendTimeout != mirrorSendTimeout {
			t.Fatalf("mode %q: unexpected batcher %+v", tc.mode, b)
		}
		b.close(t.Context())
	}
	if storeAppendBatchEntries != 500 || storeAppendBatchBytes != 1<<20 || mirrorAppendMaxAttempts != 3 ||
		!slices.Equal(mirrorAppendBackoff, []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}) {
		t.Fatal("mirror constants drifted from the Python SDK")
	}
}

func TestMirrorBatcherEagerFlushesPerFrame(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{}
	b, _ := newTestMirrorBatcher(t, store, nil, 0, 0)
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"type": "user", "n": 1}})
	waitUntil(t, "first append", func() bool { return len(store.appendCalls()) == 1 })
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"type": "assistant", "n": 2}})
	waitUntil(t, "second append", func() bool { return len(store.appendCalls()) == 2 })
	if got := mirrorNs(store.appendCalls()); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("order = %v", got)
	}
}

func TestMirrorBatcherAppendFailureReportedOnce(t *testing.T) {
	t.Parallel()
	var attempts int
	var mu sync.Mutex
	store := &mirrorStoreFake{appendHook: func(context.Context, SessionKey, []SessionStoreEntry) error {
		mu.Lock()
		attempts++
		mu.Unlock()
		return errors.New("boom")
	}}
	errs := &mirrorErrors{}
	b, sleeps := newTestMirrorBatcher(t, store, errs, storeAppendBatchEntries, storeAppendBatchBytes)
	b.enqueue(mirrorMainPath("proj", "sess"), []SessionStoreEntry{{"type": "x"}})
	b.flush(t.Context())

	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	got := errs.get()
	if len(got) != 1 {
		t.Fatalf("errors = %+v, want exactly one", got)
	}
	if got[0].key == nil || *got[0].key != (SessionKey{ProjectKey: "proj", SessionID: "sess"}) {
		t.Fatalf("error key = %+v", got[0].key)
	}
	if !strings.Contains(got[0].msg, "boom") {
		t.Fatalf("error = %q", got[0].msg)
	}
	if w := sleeps.get(); !slices.Equal(w, []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}) {
		t.Fatalf("backoff = %v", w)
	}
}

func TestMirrorBatcherRetryThenSucceed(t *testing.T) {
	t.Parallel()
	var attempts int
	var mu sync.Mutex
	store := &mirrorStoreFake{appendHook: func(context.Context, SessionKey, []SessionStoreEntry) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts < 3 {
			return errors.New("transient")
		}
		return nil
	}}
	errs := &mirrorErrors{}
	b, sleeps := newTestMirrorBatcher(t, store, errs, storeAppendBatchEntries, storeAppendBatchBytes)
	b.enqueue(mirrorMainPath("proj", "sess"), []SessionStoreEntry{{"type": "x"}})
	b.flush(t.Context())

	if len(errs.get()) != 0 {
		t.Fatalf("errors reported after a successful retry: %+v", errs.get())
	}
	loaded, _ := store.Load(t.Context(), SessionKey{ProjectKey: "proj", SessionID: "sess"})
	if !reflect.DeepEqual(loaded, []SessionStoreEntry{{"type": "x"}}) {
		t.Fatalf("stored = %v", loaded)
	}
	if w := sleeps.get(); !slices.Equal(w, []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}) {
		t.Fatalf("backoff = %v", w)
	}
}

func TestMirrorBatcherPanickingStoreIsAFailure(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{appendHook: func(context.Context, SessionKey, []SessionStoreEntry) error {
		panic("kaboom")
	}}
	errs := &mirrorErrors{}
	b, _ := newTestMirrorBatcher(t, store, errs, storeAppendBatchEntries, storeAppendBatchBytes)
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"type": "x"}})
	b.flush(t.Context())
	if got := errs.get(); len(got) != 1 || !strings.Contains(got[0].msg, "kaboom") {
		t.Fatalf("errors = %+v", got)
	}
}

func TestMirrorBatcherTimeoutNotRetried(t *testing.T) {
	t.Parallel()
	for _, honorsCtx := range []bool{true, false} {
		name := "honorsContext"
		if !honorsCtx {
			name = "ignoresContext"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			release := make(chan struct{})
			defer close(release)
			var mu sync.Mutex
			calls, inFlight, maxInFlight := 0, 0, 0
			store := &mirrorStoreFake{appendHook: func(ctx context.Context, _ SessionKey, _ []SessionStoreEntry) error {
				mu.Lock()
				calls++
				inFlight++
				maxInFlight = max(maxInFlight, inFlight)
				mu.Unlock()
				defer func() {
					mu.Lock()
					inFlight--
					mu.Unlock()
				}()
				if honorsCtx {
					<-ctx.Done()
					return ctx.Err()
				}
				<-release
				return nil
			}}
			errs := &mirrorErrors{}
			b, sleeps := newTestMirrorBatcher(t, store, errs, storeAppendBatchEntries, storeAppendBatchBytes)
			b.sendTimeout = 20 * time.Millisecond
			b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"type": "x"}})
			b.flush(t.Context())

			mu.Lock()
			if calls != 1 || maxInFlight != 1 {
				t.Errorf("calls = %d, max in flight = %d; want a single attempt", calls, maxInFlight)
			}
			mu.Unlock()
			if got := errs.get(); len(got) != 1 {
				t.Fatalf("errors = %+v, want one", got)
			}
			if w := sleeps.get(); len(w) != 0 {
				t.Fatalf("backoff slept %v after a timeout", w)
			}
		})
	}
}

func TestMirrorBatcherUnmappedPathDropped(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{}
	errs := &mirrorErrors{}
	b, _ := newTestMirrorBatcher(t, store, errs, storeAppendBatchEntries, storeAppendBatchBytes)
	b.enqueue(filepath.Join(string(filepath.Separator), "elsewhere", "x.jsonl"), []SessionStoreEntry{{"type": "x"}})
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"type": "y"}})
	b.flush(t.Context())
	calls := store.appendCalls()
	if len(calls) != 1 || calls[0].key.SessionID != "s" {
		t.Fatalf("calls = %+v", calls)
	}
	if len(errs.get()) != 0 {
		t.Fatalf("unmapped path reported as error: %+v", errs.get())
	}
}

func TestMirrorBatcherOnErrorPanicIsContained(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{appendHook: func(context.Context, SessionKey, []SessionStoreEntry) error {
		return errors.New("boom")
	}}
	b := newTranscriptMirrorBatcher(store, mirrorProjectsDir, func(*SessionKey, string) { panic("callback") }, 10, 1<<20)
	b.sleep = func(context.Context, time.Duration) error { return nil }
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"type": "x"}})
	b.flush(t.Context())
	b.close(t.Context())
}

// gatedStore blocks every Append until the gate opens, recording entries once
// released.
func gatedStore(gate <-chan struct{}, entered chan<- struct{}) *mirrorStoreFake {
	return &mirrorStoreFake{appendHook: func(ctx context.Context, _ SessionKey, _ []SessionStoreEntry) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
}

func TestMirrorBatcherEagerFlushesDoNotInterleave(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	store := gatedStore(gate, entered)
	b, _ := newTestMirrorBatcher(t, store, nil, 0, 0)
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"n": 1}})
	<-entered // first drain is mid-append
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"n": 2}})
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"n": 3}})
	close(gate)
	b.flush(t.Context())

	calls := store.appendCalls()
	if got := mirrorNs(calls); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("appended %v, want [1 2 3] once each in order", got)
	}
	// Frames that arrived while the store was busy were coalesced into one
	// follow-up append.
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
}

func TestMirrorBatcherFlushWaitsForInFlightEagerFlush(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	store := gatedStore(gate, entered)
	b, _ := newTestMirrorBatcher(t, store, nil, 1, storeAppendBatchBytes)
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"n": 1}, {"n": 2}})
	<-entered
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"n": 3}})

	flushed := make(chan struct{})
	go func() {
		b.flush(context.Background())
		close(flushed)
	}()
	select {
	case <-flushed:
		t.Fatal("flush returned while an earlier append was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	<-flushed
	if got := mirrorNs(store.appendCalls()); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("order = %v", got)
	}
}

func TestMirrorBatcherFlushReturnsOnContext(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	store := gatedStore(gate, entered)
	b, _ := newTestMirrorBatcher(t, store, nil, storeAppendBatchEntries, storeAppendBatchBytes)
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"n": 1}})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	b.flush(ctx)
	close(gate)
	b.flush(t.Context())
	if got := mirrorNs(store.appendCalls()); !slices.Equal(got, []int{1}) {
		t.Fatalf("appended %v", got)
	}
}

func TestMirrorBatcherCloseFlushesPending(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{}
	b := newTranscriptMirrorBatcher(store, mirrorProjectsDir, nil, storeAppendBatchEntries, storeAppendBatchBytes)
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"n": 1}})
	b.close(t.Context())
	if n := len(store.appendCalls()); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	// Closed: further frames are dropped and close is idempotent.
	b.enqueue(mirrorMainPath("p", "s"), []SessionStoreEntry{{"n": 2}})
	b.flush(t.Context())
	b.close(t.Context())
	if n := len(store.appendCalls()); n != 1 {
		t.Fatalf("calls after close = %d, want 1", n)
	}
}

func TestMirrorBatcherCloseIsBounded(t *testing.T) {
	t.Parallel()
	// A store that ignores its context and never returns on its own.
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	store := &mirrorStoreFake{appendHook: func(context.Context, SessionKey, []SessionStoreEntry) error {
		entered <- struct{}{}
		<-release
		return nil
	}}
	errs := &mirrorErrors{}
	b := newTranscriptMirrorBatcher(store, mirrorProjectsDir, errs.record, 0, 0)
	b.enqueue(mirrorMainPath("p", "a"), []SessionStoreEntry{{"n": 1}})
	<-entered
	b.enqueue(mirrorMainPath("p", "b"), []SessionStoreEntry{{"n": 2}}) // queued behind the stuck append

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	b.close(ctx) // returns only after every drain goroutine has exited
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("close took %v", d)
	}
	// Both the abandoned batch and the one that never got its turn are
	// reported.
	if got := errs.get(); len(got) != 2 {
		t.Fatalf("errors = %+v, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// Engine integration
// ---------------------------------------------------------------------------

func mirrorFrame(path string, entries ...map[string]any) map[string]any {
	list := make([]any, len(entries))
	for i, e := range entries {
		list[i] = e
	}
	return map[string]any{"type": "transcript_mirror", "filePath": path, "entries": list}
}

func startMirrorEngine(t *testing.T, opts *Options) (*engine, *fakeTransport) {
	t.Helper()
	ft := newFakeTransport()
	if err := ft.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	eng := newTestEngine(t, ft, opts)
	eng.enableTranscriptMirror(opts, mirrorProjectsDir)
	if b := eng.mirror.Load(); b != nil {
		b.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	}
	eng.start(t.Context())
	t.Cleanup(func() { _ = eng.close() })
	return eng, ft
}

func TestEngineMirrorFramesReachStoreBeforeResult(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{}
	eng, ft := startMirrorEngine(t, &Options{SessionStore: store})
	path := mirrorMainPath("myproj", "mysess")
	ft.push(mirrorFrame(path, map[string]any{"type": "user", "uuid": "u1"}))
	ft.push(assistantFrame("hi"))
	ft.push(mirrorFrame(path, map[string]any{"type": "user", "uuid": "u2"}))
	ft.push(resultFrame())
	ft.finish(nil)

	var kinds []string
	appendsAtResult := -1
	for msg, err := range eng.receive(context.Background()) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		switch msg.(type) {
		case *AssistantMessage:
			kinds = append(kinds, "assistant")
		case *ResultMessage:
			kinds = append(kinds, "result")
			appendsAtResult = len(store.appendCalls())
		default:
			t.Fatalf("unexpected message %T", msg)
		}
	}
	if !slices.Equal(kinds, []string{"assistant", "result"}) {
		t.Fatalf("kinds = %v; mirror frames must not surface", kinds)
	}
	if appendsAtResult != 1 {
		t.Fatalf("appends when the result surfaced = %d, want 1", appendsAtResult)
	}
	calls := store.appendCalls()
	if calls[0].key != (SessionKey{ProjectKey: "myproj", SessionID: "mysess"}) {
		t.Fatalf("key = %+v", calls[0].key)
	}
	want := []SessionStoreEntry{{"type": "user", "uuid": "u1"}, {"type": "user", "uuid": "u2"}}
	if !reflect.DeepEqual(calls[0].entries, want) {
		t.Fatalf("entries = %v", calls[0].entries)
	}
}

func TestEngineMirrorLateFramesFlushedAtEnd(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{}
	eng, ft := startMirrorEngine(t, &Options{SessionStore: store})
	ft.push(resultFrame())
	ft.push(mirrorFrame(mirrorMainPath("late", "sess"), map[string]any{"type": "user", "uuid": "late-u1"}))
	ft.finish(nil)
	for _, err := range eng.receive(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
	}
	// The stream ends only after the read loop's final flush.
	calls := store.appendCalls()
	if len(calls) != 1 || calls[0].key != (SessionKey{ProjectKey: "late", SessionID: "sess"}) {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestEngineMirrorEagerMode(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{}
	eng, ft := startMirrorEngine(t, &Options{SessionStore: store, SessionStoreFlush: SessionStoreFlushEager})
	path := mirrorMainPath("p", "s")
	ft.push(mirrorFrame(path, map[string]any{"type": "user", "uuid": "u1"}))
	waitUntil(t, "first append", func() bool { return len(store.appendCalls()) == 1 })
	ft.push(mirrorFrame(path, map[string]any{"type": "assistant", "uuid": "a1"}))
	waitUntil(t, "second append", func() bool { return len(store.appendCalls()) == 2 })
	ft.push(resultFrame())
	ft.finish(nil)
	for _, err := range eng.receive(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
	}
	calls := store.appendCalls()
	if len(calls) != 2 || calls[0].entries[0]["uuid"] != "u1" || calls[1].entries[0]["uuid"] != "a1" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestEngineMirrorFramesDroppedWithoutStore(t *testing.T) {
	t.Parallel()
	eng, ft := startMirrorEngine(t, nil)
	if eng.mirror.Load() != nil {
		t.Fatal("batcher attached without a SessionStore")
	}
	ft.push(mirrorFrame(mirrorMainPath("p", "s"), map[string]any{"type": "user"}))
	ft.push(map[string]any{"type": "transcript_mirror"}) // malformed: ignored
	ft.push(assistantFrame("hi"))
	ft.push(resultFrame())
	ft.finish(nil)
	n := 0
	for _, err := range eng.receive(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("messages = %d, want 2", n)
	}
}

func TestEngineMirrorErrorSurfaces(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{appendHook: func(context.Context, SessionKey, []SessionStoreEntry) error {
		return errors.New("disk full")
	}}
	eng, ft := startMirrorEngine(t, &Options{SessionStore: store})
	ft.push(mirrorFrame(mirrorMainPath("proj", "sess"), map[string]any{"type": "user"}))
	ft.push(resultFrame())
	ft.finish(nil)

	var mirrorErrs []*MirrorErrorMessage
	sawResult := false
	for msg, err := range eng.receive(context.Background()) {
		if err != nil {
			t.Fatalf("mirror failure must not be fatal: %v", err)
		}
		switch m := msg.(type) {
		case *MirrorErrorMessage:
			if sawResult {
				t.Fatal("mirror error surfaced after the result it was flushed for")
			}
			mirrorErrs = append(mirrorErrs, m)
		case *ResultMessage:
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatal("result missing")
	}
	if len(mirrorErrs) != 1 {
		t.Fatalf("mirror errors = %d, want 1", len(mirrorErrs))
	}
	m := mirrorErrs[0]
	if m.Subtype != "mirror_error" || !strings.Contains(m.Error, "disk full") {
		t.Fatalf("message = %+v", m)
	}
	if m.Key == nil || *m.Key != (SessionKey{ProjectKey: "proj", SessionID: "sess"}) {
		t.Fatalf("key = %+v", m.Key)
	}
}

func TestEngineReportMirrorError(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t, newFakeTransport(), nil)
	eng.reportMirrorError(&SessionKey{ProjectKey: "p", SessionID: "s", Subpath: "subagents/agent-1"}, "boom")
	eng.reportMirrorError(nil, "no key")

	first := (<-eng.messages.ch).msg.(*MirrorErrorMessage)
	if first.Error != "boom" || first.Key == nil || first.Key.Subpath != "subagents/agent-1" {
		t.Fatalf("message = %+v", first)
	}
	d := first.Data
	if d["type"] != "system" || d["subtype"] != "mirror_error" || d["error"] != "boom" || d["session_id"] != "s" {
		t.Fatalf("data = %v", d)
	}
	if u, _ := d["uuid"].(string); len(u) != 36 || u[14] != '4' {
		t.Fatalf("uuid = %q", u)
	}
	// The payload parses back to the same message.
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	pm, ok := parsed.(*MirrorErrorMessage)
	if !ok || pm.Error != "boom" || pm.Key == nil || *pm.Key != *first.Key {
		t.Fatalf("parsed = %#v", parsed)
	}

	bare, err := ParseMessage([]byte(`{"type":"system","subtype":"mirror_error","error":"e"}`))
	if err != nil {
		t.Fatal(err)
	}
	if bm, ok := bare.(*MirrorErrorMessage); !ok || bm.Key != nil || bm.Error != "e" {
		t.Fatalf("parsed keyless = %#v", bare)
	}

	second := (<-eng.messages.ch).msg.(*MirrorErrorMessage)
	if second.Key != nil || second.Data["key"] != nil || second.Data["session_id"] != "" {
		t.Fatalf("keyless message = %+v", second)
	}

	// A full buffer drops the report instead of blocking, and a closed
	// stream ignores it.
	for range messageBufferSize + 5 {
		eng.reportMirrorError(nil, "x")
	}
	eng.closeMessages()
	eng.reportMirrorError(nil, "after close")
}

func TestEngineCloseFlushesMirror(t *testing.T) {
	t.Parallel()
	store := &mirrorStoreFake{}
	eng, ft := startMirrorEngine(t, &Options{SessionStore: store})
	ft.push(mirrorFrame(mirrorMainPath("p", "s"), map[string]any{"type": "user", "uuid": "u1"}))
	ft.push(assistantFrame("hi"))
	for msg, err := range eng.receive(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := msg.(*AssistantMessage); ok {
			break // the mirror frame before it has been routed
		}
	}
	if n := len(store.appendCalls()); n != 0 {
		t.Fatalf("appended before close: %d", n)
	}
	if err := eng.close(); err != nil {
		t.Fatal(err)
	}
	if n := len(store.appendCalls()); n != 1 {
		t.Fatalf("calls after close = %d, want 1", n)
	}
}

func TestEngineCloseBoundedWithStuckStore(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	store := &mirrorStoreFake{appendHook: func(context.Context, SessionKey, []SessionStoreEntry) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // ignores its context
		return nil
	}}
	ft := newFakeTransport()
	opts := &Options{SessionStore: store}
	eng := newTestEngine(t, ft, opts)
	eng.enableTranscriptMirror(opts, mirrorProjectsDir)
	b := eng.mirror.Load()
	b.closeTimeout = 50 * time.Millisecond
	eng.start(t.Context())

	ft.push(mirrorFrame(mirrorMainPath("p", "s"), map[string]any{"type": "user"}))
	ft.push(resultFrame()) // the read loop blocks flushing for this result
	<-entered

	start := time.Now()
	if err := eng.close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v with a stuck store", d)
	}
	select {
	case <-eng.readerDone:
	default:
		t.Fatal("read loop still running after Close")
	}
	for range eng.receive(context.Background()) {
	}
}
