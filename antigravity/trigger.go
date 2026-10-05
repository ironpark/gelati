package antigravity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gelati/internal/logx"
)

// Trigger is a long-running function that runs alongside a session and
// reacts to external events (timers, file changes, webhooks) by pushing
// messages to the agent through tc. It should return when ctx is
// cancelled, which happens when the session ends. A returned error (or a
// panic) is logged and stops that trigger only; triggers are not
// restarted.
type Trigger func(ctx context.Context, tc *TriggerContext) error

// TriggerConnection is what a trigger sends through; *Connection
// implements it.
type TriggerConnection interface {
	SendTriggerNotification(ctx context.Context, content string) error
}

// TriggerContext is the handle each trigger receives.
type TriggerContext struct {
	conn TriggerConnection
}

// NewTriggerContext returns a context sending through conn, for running a
// trigger outside an Agent (in tests, for example).
func NewTriggerContext(conn TriggerConnection) *TriggerContext { return &TriggerContext{conn: conn} }

// Send pushes a message to the agent as an automated trigger. It works
// while a turn is running.
func (tc *TriggerContext) Send(ctx context.Context, content string) error {
	return tc.conn.SendTriggerNotification(ctx, content)
}

// TriggerRunner starts triggers as goroutines and stops them. An Agent
// runs its Config.Triggers with one.
type TriggerRunner struct {
	triggers []Trigger
	conn     TriggerConnection
	logger   *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	active int
}

// NewTriggerRunner returns a runner for triggers sending through conn.
func NewTriggerRunner(triggers []Trigger, conn TriggerConnection) *TriggerRunner {
	return &TriggerRunner{triggers: slices.Clone(triggers), conn: conn, logger: logx.Or(nil)}
}

// Start runs every trigger on its own goroutine, each with its own
// TriggerContext, in no particular order. It fails if the runner is
// already started.
func (r *TriggerRunner) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return errors.New("antigravity: TriggerRunner is already started")
	}
	if len(r.triggers) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	done := make(chan struct{})
	r.done = done
	r.active = len(r.triggers)
	var wg sync.WaitGroup
	for _, t := range r.triggers {
		wg.Go(func() {
			r.run(ctx, t)
			r.mu.Lock()
			r.active--
			r.mu.Unlock()
		})
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	return nil
}

func (r *TriggerRunner) run(ctx context.Context, t Trigger) {
	name := triggerName(t)
	defer func() {
		if v := recover(); v != nil {
			r.logger.Error("trigger panicked", "trigger", name, "panic", v)
		}
	}()
	err := t(ctx, &TriggerContext{conn: r.conn})
	switch {
	case ctx.Err() != nil:
		r.logger.Info("trigger cancelled", "trigger", name)
	case err != nil:
		r.logger.Error("trigger failed", "trigger", name, "error", err)
	}
}

func triggerName(t Trigger) string {
	if f := runtime.FuncForPC(reflect.ValueOf(t).Pointer()); f != nil {
		return f.Name()
	}
	return "unknown"
}

// Stop cancels every trigger and waits for them to return. It is safe to
// call more than once, and the runner can be started again afterwards.
func (r *TriggerRunner) Stop() {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel, r.done = nil, nil
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// IsRunning reports whether any trigger is still running.
func (r *TriggerRunner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active > 0
}

// Every returns a trigger that calls callback every interval, the first
// time one interval after the session starts. It panics if interval is not
// positive.
func Every(interval time.Duration, callback func(ctx context.Context, tc *TriggerContext) error) Trigger {
	if interval <= 0 {
		panic(fmt.Sprintf("antigravity: Every interval must be positive, got %v", interval))
	}
	return func(ctx context.Context, tc *TriggerContext) error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				if err := callback(ctx, tc); err != nil {
					return err
				}
			}
		}
	}
}

// FileChangeKind is the kind of a filesystem change.
type FileChangeKind string

// FileChangeKind values.
const (
	FileAdded    FileChangeKind = "added"
	FileModified FileChangeKind = "modified"
	FileDeleted  FileChangeKind = "deleted"
)

// FileChange is one filesystem change seen by an OnFileChange trigger.
type FileChange struct {
	Kind FileChangeKind
	// Path is the absolute path of the changed file.
	Path string
}

// DefaultFilePollInterval is how often OnFileChange polls.
const DefaultFilePollInterval = 500 * time.Millisecond

// OnFileChange returns a trigger that calls callback with the changes to
// path: the file itself, or every file under the directory. Upstream uses
// the watchfiles package; this implementation polls modification times and
// sizes every DefaultFilePollInterval, so it needs no OS-specific
// notification API. The changes of one poll are delivered as one batch.
func OnFileChange(path string, callback func(ctx context.Context, tc *TriggerContext, changes []FileChange) error) Trigger {
	return OnFileChangeEvery(path, DefaultFilePollInterval, callback)
}

// OnFileChangeEvery is OnFileChange with a custom poll interval. It panics
// if interval is not positive.
func OnFileChangeEvery(path string, interval time.Duration, callback func(ctx context.Context, tc *TriggerContext, changes []FileChange) error) Trigger {
	if interval <= 0 {
		panic(fmt.Sprintf("antigravity: OnFileChangeEvery interval must be positive, got %v", interval))
	}
	return func(ctx context.Context, tc *TriggerContext) error {
		root, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		prev := snapshotFiles(root)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
			cur := snapshotFiles(root)
			if changes := diffSnapshots(prev, cur); len(changes) > 0 {
				if err := callback(ctx, tc, changes); err != nil {
					return err
				}
			}
			prev = cur
		}
	}
}

type fileState struct {
	modTime time.Time
	size    int64
	mode    fs.FileMode
}

// snapshotFiles records the state of root, or of every file under it when
// it is a directory. A missing root yields an empty snapshot.
func snapshotFiles(root string) map[string]fileState {
	snap := map[string]fileState{}
	fi, err := os.Stat(root)
	if err != nil {
		return snap
	}
	if !fi.IsDir() {
		snap[root] = fileState{fi.ModTime(), fi.Size(), fi.Mode()}
		return snap
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			snap[p] = fileState{info.ModTime(), info.Size(), info.Mode()}
		}
		return nil
	})
	return snap
}

// diffSnapshots lists the changes from prev to cur, sorted by path.
func diffSnapshots(prev, cur map[string]fileState) []FileChange {
	var out []FileChange
	for p, s := range cur {
		old, ok := prev[p]
		switch {
		case !ok:
			out = append(out, FileChange{FileAdded, p})
		case old != s:
			out = append(out, FileChange{FileModified, p})
		}
	}
	for p := range prev {
		if _, ok := cur[p]; !ok {
			out = append(out, FileChange{FileDeleted, p})
		}
	}
	slices.SortFunc(out, func(a, b FileChange) int { return strings.Compare(a.Path, b.Path) })
	return out
}
