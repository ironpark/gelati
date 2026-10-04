package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Transcript mirroring: the CLI, started with --session-mirror, interleaves
// {"type":"transcript_mirror","filePath":...,"entries":[...]} frames with its
// normal output. The engine peels them off and hands them to a
// transcriptMirrorBatcher, which accumulates them and appends them to
// Options.SessionStore when a result arrives (explicit flush) or when the
// pending buffer grows past its thresholds (background flush). Ported from
// _internal/transcript_mirror_batcher.py.

const (
	// storeAppendBatchEntries and storeAppendBatchBytes bound the batches
	// sent to SessionStore.Append: they are the batched-mode mirror
	// thresholds past which a background flush starts, and the batch limits
	// of ImportSessionToStore.
	storeAppendBatchEntries = 500
	storeAppendBatchBytes   = 1 << 20
	// mirrorSendTimeout bounds one SessionStore.Append attempt.
	mirrorSendTimeout = 60 * time.Second
	// mirrorAppendMaxAttempts is the total number of Append attempts per batch.
	mirrorAppendMaxAttempts = 3
	// mirrorCloseTimeout bounds how long engine Close waits for the final
	// flush before abandoning whatever is still in flight.
	mirrorCloseTimeout = mirrorSendTimeout
)

// mirrorAppendBackoff holds the delays between Append attempts; its length is
// mirrorAppendMaxAttempts-1.
var mirrorAppendBackoff = []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}

// mirrorItem is one enqueued transcript_mirror frame.
type mirrorItem struct {
	filePath string
	entries  []SessionStoreEntry
}

// transcriptMirrorBatcher buffers transcript_mirror frames and appends them to
// a SessionStore.
//
// enqueue never blocks on the store. Appends run on background goroutines but
// are serialized in enqueue order: each drain waits for the previous one to
// finish its appends before it detaches the pending buffer, so frames that
// arrive while the store is busy are coalesced into the next drain.
//
// Failed appends are retried (mirrorAppendMaxAttempts in total) with backoff;
// a timed-out append is not retried because the abandoned call may still land.
// Only after the last attempt fails is the batch dropped and reported through
// onError. Failures never propagate: the CLI's local transcript is already
// durable, so the session continues. Stores should dedupe by entry "uuid"
// because a retried batch may overlap a partial earlier write.
type transcriptMirrorBatcher struct {
	store       SessionStore
	projectsDir string
	onError     func(key *SessionKey, err string)

	maxPendingEntries int
	maxPendingBytes   int
	sendTimeout       time.Duration
	closeTimeout      time.Duration
	backoff           []time.Duration
	// sleep waits between attempts; tests replace it to avoid real delays.
	sleep func(ctx context.Context, d time.Duration) error

	// ctx is the parent of every Append context. close cancels it once its
	// deadline passes, abandoning whatever is still in flight.
	ctx    context.Context
	cancel context.CancelFunc

	mu             sync.Mutex
	pending        []mirrorItem
	pendingEntries int
	pendingBytes   int
	// tailRelease closes when the most recently scheduled drain has finished
	// its appends; tailDone closes once it (and every earlier drain) has also
	// reported its errors.
	tailRelease chan struct{}
	tailDone    chan struct{}
	// queuedDone is the done channel of a scheduled drain that has not yet
	// detached the pending buffer, or nil. Such a drain will pick up anything
	// enqueued before it runs, so no second drain is needed.
	queuedDone chan struct{}
	closed     bool

	// drains counts drain goroutines. Abandoned (timed-out) Append calls are
	// not counted: they end when the store returns.
	drains sync.WaitGroup
}

// newTranscriptMirrorBatcher builds a batcher that appends to store. Frame
// file paths are resolved to session keys relative to projectsDir. onError,
// which may be nil, is called once per dropped batch. Zero thresholds make
// every enqueue start a background flush.
func newTranscriptMirrorBatcher(store SessionStore, projectsDir string, onError func(key *SessionKey, err string), maxPendingEntries, maxPendingBytes int) *transcriptMirrorBatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &transcriptMirrorBatcher{
		store:             store,
		projectsDir:       projectsDir,
		onError:           onError,
		maxPendingEntries: maxPendingEntries,
		maxPendingBytes:   maxPendingBytes,
		sendTimeout:       mirrorSendTimeout,
		closeTimeout:      mirrorCloseTimeout,
		backoff:           mirrorAppendBackoff,
		sleep:             sleepContext,
		ctx:               ctx,
		cancel:            cancel,
	}
}

// newMirrorBatcherForOptions builds the batcher for opts.SessionStore, or
// returns nil when no store is configured. projectsDir is the projects
// directory the CLI subprocess writes under (<CLAUDE_CONFIG_DIR>/projects, or
// the materialized resume directory). SessionStoreFlushEager zeroes the
// thresholds so every frame is flushed in the background as it arrives.
func newMirrorBatcherForOptions(opts *Options, projectsDir string, onError func(key *SessionKey, err string)) *transcriptMirrorBatcher {
	if opts == nil || opts.SessionStore == nil {
		return nil
	}
	maxEntries, maxBytes := storeAppendBatchEntries, storeAppendBatchBytes
	if opts.SessionStoreFlush == SessionStoreFlushEager {
		maxEntries, maxBytes = 0, 0
	}
	return newTranscriptMirrorBatcher(opts.SessionStore, projectsDir, onError, maxEntries, maxBytes)
}

// enqueue buffers one frame and starts a background flush when the pending
// buffer exceeds a threshold. It never blocks on the store. Frames enqueued
// after close are dropped.
func (b *transcriptMirrorBatcher) enqueue(filePath string, entries []SessionStoreEntry) {
	// Approximate wire size: one encode per frame keeps this cheap.
	size := 0
	if raw, err := json.Marshal(entries); err == nil {
		size = len(raw)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.pending = append(b.pending, mirrorItem{filePath: filePath, entries: entries})
	b.pendingEntries += len(entries)
	b.pendingBytes += size
	if b.pendingEntries > b.maxPendingEntries || b.pendingBytes > b.maxPendingBytes {
		b.scheduleLocked()
	}
}

// flush sends everything enqueued so far, after any drain already in flight,
// and waits until it is stored or reported through onError. It returns early
// when ctx ends or the batcher is shut down; the drain itself continues.
func (b *transcriptMirrorBatcher) flush(ctx context.Context) {
	b.wait(ctx, b.flushAsync())
}

// flushAsync schedules a flush of everything enqueued so far and returns a
// channel that closes once it is stored or reported, or nil when there is
// nothing to wait for.
func (b *transcriptMirrorBatcher) flushAsync() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.scheduleLocked()
}

// close performs the final flush and shuts the batcher down. It waits until
// ctx ends at most; anything still in flight then is abandoned, its Append
// context cancelled. Further frames are dropped. close never fails and is
// safe to call more than once.
func (b *transcriptMirrorBatcher) close(ctx context.Context) {
	b.mu.Lock()
	b.closed = true
	done := b.scheduleLocked()
	b.mu.Unlock()
	b.wait(ctx, done)
	b.cancel()
	// Drain goroutines observe the cancelled context and finish promptly,
	// even when an Append that ignores its context is still running.
	b.drains.Wait()
}

func (b *transcriptMirrorBatcher) wait(ctx context.Context, done <-chan struct{}) {
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	case <-b.ctx.Done():
	}
}

// scheduleLocked returns a channel that closes once everything pending now has
// been flushed, starting a drain when needed. It returns nil when there is
// nothing to wait for. b.mu must be held.
func (b *transcriptMirrorBatcher) scheduleLocked() chan struct{} {
	if b.queuedDone != nil {
		return b.queuedDone
	}
	if len(b.pending) == 0 {
		// Nothing to send, but a caller still waits for in-flight drains so
		// the store is up to date when it returns.
		return b.tailDone
	}
	release := make(chan struct{})
	done := make(chan struct{})
	prevRelease, prevDone := b.tailRelease, b.tailDone
	b.tailRelease, b.tailDone, b.queuedDone = release, done, done
	b.drains.Add(1)
	go b.drain(prevRelease, prevDone, release, done)
	return done
}

// drain waits for the previous drain to finish its appends, detaches the
// pending buffer and sends it.
func (b *transcriptMirrorBatcher) drain(prevRelease, prevDone <-chan struct{}, release, done chan struct{}) {
	defer b.drains.Done()
	defer close(done)

	if prevRelease != nil {
		select {
		case <-prevRelease:
		case <-b.ctx.Done():
		}
	}

	b.mu.Lock()
	items := b.pending
	b.pending = nil
	b.pendingEntries = 0
	b.pendingBytes = 0
	b.queuedDone = nil
	b.mu.Unlock()

	errs := b.send(items)
	close(release)

	// Errors are reported after releasing the next drain so a slow onError
	// cannot hold up later appends.
	for _, e := range errs {
		b.reportError(e.key, e.msg)
	}
	if prevDone != nil {
		select {
		case <-prevDone:
		case <-b.ctx.Done():
		}
	}
}

type mirrorFailure struct {
	key *SessionKey
	msg string
}

// send coalesces items by file path and appends each path's entries, keeping
// first-seen path order and enqueue order within a path.
func (b *transcriptMirrorBatcher) send(items []mirrorItem) []mirrorFailure {
	var order []string
	byPath := map[string][]SessionStoreEntry{}
	for _, item := range items {
		bucket, seen := byPath[item.filePath]
		if !seen {
			order = append(order, item.filePath)
		}
		byPath[item.filePath] = append(bucket, item.entries...)
	}

	var failures []mirrorFailure
	for _, path := range order {
		entries := byPath[path]
		if len(entries) == 0 {
			// Appending nothing could create a phantom key in some stores.
			continue
		}
		key, ok := filePathToSessionKey(path, b.projectsDir)
		if !ok {
			// The subprocess wrote outside projectsDir (its CLAUDE_CONFIG_DIR
			// likely differs from the parent's); there is no key to use.
			continue
		}
		if err := b.appendWithRetry(key, entries); err != nil {
			failures = append(failures, mirrorFailure{key: &key, msg: err.Error()})
		}
	}
	return failures
}

// appendWithRetry appends one batch, retrying failures with backoff. A timeout
// is not retried: the abandoned call may still land, and a retry would race
// it.
func (b *transcriptMirrorBatcher) appendWithRetry(key SessionKey, entries []SessionStoreEntry) error {
	var lastErr error
	for attempt := range mirrorAppendMaxAttempts {
		if attempt > 0 {
			delay := time.Duration(0)
			if i := attempt - 1; i < len(b.backoff) {
				delay = b.backoff[i]
			}
			if err := b.sleep(b.ctx, delay); err != nil {
				return lastErr
			}
		}
		if err := b.ctx.Err(); err != nil {
			if lastErr == nil {
				lastErr = fmt.Errorf("transcript mirror closed before append: %w", err)
			}
			return lastErr
		}
		timedOut, err := b.appendOnce(key, entries)
		if err == nil {
			return nil
		}
		lastErr = err
		if timedOut {
			return lastErr
		}
	}
	return lastErr
}

// appendOnce runs one Append bounded by sendTimeout (see runStoreCall), so a
// store that ignores its context cannot wedge the batcher.
func (b *transcriptMirrorBatcher) appendOnce(key SessionKey, entries []SessionStoreEntry) (timedOut bool, err error) {
	_, status, err := runStoreCall(b.ctx, b.sendTimeout,
		func(r any) error { return fmt.Errorf("SessionStore.Append panicked: %v", r) },
		func(ctx context.Context) (struct{}, error) { return struct{}{}, b.store.Append(ctx, key, entries) })
	switch status {
	case storeCallCanceled:
		return false, fmt.Errorf("transcript mirror closed during append: %w", err)
	case storeCallTimedOut:
		if err == nil {
			err = fmt.Errorf("SessionStore.Append timed out after %s", b.sendTimeout)
		}
		return true, err
	}
	return false, err
}

func (b *transcriptMirrorBatcher) reportError(key *SessionKey, msg string) {
	if b.onError == nil {
		return
	}
	defer func() { _ = recover() }()
	b.onError(key, msg)
}

// sleepContext waits for d or until ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// filePathToSessionKey derives a SessionKey from a transcript path under
// projectsDir:
//
//	<projectsDir>/<projectKey>/<sessionID>.jsonl                     main transcript
//	<projectsDir>/<projectKey>/<sessionID>/subagents/agent-<id>.jsonl subagent
//
// Subagent paths may nest deeper; the subpath is everything after the session
// directory, "/"-joined on every platform, without the .jsonl suffix. It
// reports false for a path outside projectsDir or of an unrecognized shape.
func filePathToSessionKey(filePath, projectsDir string) (SessionKey, bool) {
	if filePath == "" {
		return SessionKey{}, false
	}
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return SessionKey{}, false
	}
	absDir, err := filepath.Abs(projectsDir)
	if err != nil {
		return SessionKey{}, false
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil || filepath.IsAbs(rel) {
		// Different volumes on Windows: not under projectsDir.
		return SessionKey{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 || parts[0] == ".." {
		return SessionKey{}, false
	}
	projectKey, second := parts[0], parts[1]
	if len(parts) == 2 {
		if sessionID, ok := strings.CutSuffix(second, ".jsonl"); ok {
			return SessionKey{ProjectKey: projectKey, SessionID: sessionID}, true
		}
		return SessionKey{}, false
	}
	if len(parts) >= 4 {
		sub := append([]string(nil), parts[2:]...)
		sub[len(sub)-1] = strings.TrimSuffix(sub[len(sub)-1], ".jsonl")
		return SessionKey{ProjectKey: projectKey, SessionID: second, Subpath: strings.Join(sub, "/")}, true
	}
	return SessionKey{}, false
}

// mirrorEntries converts a frame's "entries" array, skipping anything that is
// not a JSON object.
func mirrorEntries(v any) []SessionStoreEntry {
	list, _ := v.([]any)
	out := make([]SessionStoreEntry, 0, len(list))
	for _, item := range list {
		if entry, ok := item.(map[string]any); ok {
			out = append(out, entry)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Engine integration
// ---------------------------------------------------------------------------

// setMirrorBatcher attaches the batcher that receives transcript_mirror
// frames. Call it before Start. Without one the frames are dropped.
func (e *engine) setMirrorBatcher(b *transcriptMirrorBatcher) {
	e.mirror.Store(b)
}

// enableTranscriptMirror attaches a batcher for e.opts.SessionStore, reporting
// failures as MirrorErrorMessage. It does nothing without a store. Call it
// before Start; projectsDir is as for newMirrorBatcherForOptions.
func (e *engine) enableTranscriptMirror(projectsDir string) {
	if b := newMirrorBatcherForOptions(e.opts, projectsDir, e.reportMirrorError); b != nil {
		e.setMirrorBatcher(b)
	}
}

// enqueueMirrorFrame hands one transcript_mirror frame to the batcher.
func (e *engine) enqueueMirrorFrame(frame map[string]any) {
	b := e.mirror.Load()
	if b == nil {
		return
	}
	filePath, ok := frame["filePath"].(string)
	if !ok {
		return
	}
	b.enqueue(filePath, mirrorEntries(frame["entries"]))
}

// flushMirror flushes pending transcript entries, if mirroring is enabled,
// and waits for the store. It stops waiting once the engine is closing: Close
// then owns the final flush, bounded by the batcher's close timeout.
func (e *engine) flushMirror(ctx context.Context) {
	b := e.mirror.Load()
	if b == nil {
		return
	}
	done := b.flushAsync()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-b.ctx.Done():
	case <-e.closed:
	case <-ctx.Done():
	}
}

// reportMirrorError surfaces a dropped mirror batch as a MirrorErrorMessage.
// It never blocks: when the consumer's buffer is full, or the stream has
// ended, the report is dropped rather than back-pressuring the batcher.
func (e *engine) reportMirrorError(key *SessionKey, errMsg string) {
	data := map[string]any{
		"type":       "system",
		"subtype":    "mirror_error",
		"error":      errMsg,
		"key":        nil,
		"uuid":       randomUUID(),
		"session_id": "",
	}
	var keyCopy *SessionKey
	if key != nil {
		k := *key
		keyCopy = &k
		km := map[string]any{"project_key": k.ProjectKey, "session_id": k.SessionID}
		if k.Subpath != "" {
			km["subpath"] = k.Subpath
		}
		data["key"] = km
		data["session_id"] = k.SessionID
	}
	msg := &MirrorErrorMessage{
		SystemMessage: SystemMessage{Subtype: "mirror_error", Data: data},
		Key:           keyCopy,
		Error:         errMsg,
	}
	e.msgMu.Lock()
	defer e.msgMu.Unlock()
	if e.msgClosed {
		return
	}
	select {
	case e.messages <- messageOrError{msg: msg}:
	default:
	}
}

// closeMessages ends the message stream. Only the read loop sends blocking on
// e.messages; other senders go through msgMu and check msgClosed.
func (e *engine) closeMessages() {
	e.msgMu.Lock()
	defer e.msgMu.Unlock()
	e.msgClosed = true
	close(e.messages)
}
