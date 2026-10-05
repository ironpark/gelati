package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultRunEndCeiling is how long the run stays open after a result while
// the CLI still reports work, unless CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS says
// otherwise (the CLI's own background-wait ceiling).
const defaultRunEndCeiling = 600 * time.Second

// maxRunEndCeiling caps the ceiling, as the TypeScript SDK's timers do.
const maxRunEndCeiling = (1<<31 - 1) * time.Millisecond

// deferringTaskTypes are the task types whose completion runs a follow-up turn,
// so the input stream must stay open past the turn's result frame. Background
// shells and other open-ended tasks are deliberately excluded: they may never
// reach a terminal status, and holding input open for one would withhold it
// forever.
var deferringTaskTypes = map[string]bool{"local_agent": true, "local_workflow": true}

// ---------------------------------------------------------------------------
// Run end
// ---------------------------------------------------------------------------

// runTracker is the run-end state machine, mirroring the TypeScript SDK. A
// streamed query holds the CLI's input open while the session may still send
// control requests that need a reply; the run ends on a result once the CLI
// reports "idle" (or never reports state at all), and the input can then be
// closed. Work taken up after the end reopens the run until the next idle,
// and a ceiling bounds the wait for an "idle" that never comes.
type runTracker struct {
	// ceiling bounds the wait for "idle" after a result; zero disables it.
	ceiling time.Duration
	// closed is closed when the engine stops: it releases waiters and
	// keeps the ceiling from being armed.
	closed <-chan struct{}

	mu             sync.Mutex
	sessionState   string
	resultReceived bool
	// endCh is closed when the run ends, so the run is over while it is
	// closed, and replaced when work reopens it.
	endCh chan struct{}
	// final is set once nothing can reopen the run.
	final        bool
	ceilingTimer *time.Timer
	ceilingGen   int
	// tasks holds the delegated tasks still running whose completion runs
	// a follow-up turn.
	tasks map[string]bool
}

// newRunTracker builds a tracker for an open run.
func newRunTracker(ceiling time.Duration, closed <-chan struct{}) *runTracker {
	return &runTracker{
		ceiling: ceiling,
		closed:  closed,
		endCh:   make(chan struct{}),
		tasks:   map[string]bool{},
	}
}

// onResult records a result frame. The run ends unless the CLI still reports
// work and holdsInput says the session may yet need its input.
func (r *runTracker) onResult(holdsInput bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resultReceived = true
	if r.sessionState == "" || r.sessionState == SessionStateIdle || !holdsInput {
		r.maybeEndLocked()
	} else {
		// The CLI still reports work (a background agent, a follow-up turn
		// it owes): wait for "idle", but not forever.
		r.armCeilingLocked()
	}
}

// onSessionState tracks the CLI's session_state_changed reports.
func (r *runTracker) onSessionState(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionState = state
	if state == SessionStateIdle {
		if r.resultReceived {
			r.maybeEndLocked()
		}
		return
	}
	if r.final {
		return
	}
	// Work taken up after the run ended reopens it until the next idle.
	r.reopenLocked()
	if state == SessionStateRequiresAction {
		// This host is answering a request; the input must outlast it.
		r.clearCeilingLocked()
	} else if r.resultReceived {
		r.armCeilingLocked()
	}
}

// onMainThreadActivity records a main-thread turn under way: the ceiling
// counts only the wait between turns, and the run reopens.
func (r *runTracker) onMainThreadActivity() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.final {
		r.reopenLocked()
		r.clearCeilingLocked()
	}
}

// onTaskFrame keeps the set of delegated tasks that are still running, from a
// system frame's task lifecycle fields.
func (r *runTracker) onTaskFrame(frame map[string]any) {
	taskID := str(frame["task_id"])
	if taskID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch str(frame["subtype"]) {
	case "task_started":
		if deferringTaskTypes[str(frame["task_type"])] {
			r.tasks[taskID] = true
		}
	case "task_notification":
		delete(r.tasks, taskID)
	case "task_updated":
		patch, _ := frame["patch"].(map[string]any)
		if isTerminalTaskStatus(str(patch["status"])) {
			delete(r.tasks, taskID)
		}
	}
}

// startTurn records a user message written to the CLI: that prompt owes a
// run of its own, result included.
func (r *runTracker) startTurn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.final {
		return
	}
	r.reopenLocked()
	r.resultReceived = false
	r.clearCeilingLocked()
}

// finalize ends the run for good: the input is closed or the reader is gone,
// so nothing can wait on a reopened run.
func (r *runTracker) finalize() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.final = true
	r.endLocked()
}

// wait blocks until the run ends, the engine closes or ctx is done.
func (r *runTracker) wait(ctx context.Context) error {
	r.mu.Lock()
	ch := r.endCh
	r.mu.Unlock()
	if isDone(ch) {
		// Ended runs win over a done ctx.
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-r.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// maybeEndLocked ends the run, unless the CLI reports no session state and a
// delegated task is still in flight: such a task wakes the session for a
// follow-up turn whose control requests still need the input stream.
func (r *runTracker) maybeEndLocked() {
	if r.sessionState == "" && len(r.tasks) > 0 {
		return
	}
	r.endLocked()
}

func (r *runTracker) endLocked() {
	r.clearCeilingLocked()
	if !isDone(r.endCh) {
		close(r.endCh)
	}
}

// reopenLocked makes a later wait for the run end wait for new work. A
// waiter the ended run already woke is unaffected.
func (r *runTracker) reopenLocked() {
	if isDone(r.endCh) && !r.final {
		r.endCh = make(chan struct{})
	}
}

// armCeilingLocked ends the run anyway once the ceiling passes without the
// CLI reporting "idle" or starting a new turn.
func (r *runTracker) armCeilingLocked() {
	r.clearCeilingLocked()
	if isDone(r.endCh) || r.final || r.ceiling <= 0 || isDone(r.closed) {
		return
	}
	gen := r.ceilingGen
	r.ceilingTimer = time.AfterFunc(min(r.ceiling, maxRunEndCeiling), func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if gen == r.ceilingGen {
			r.ceilingTimer = nil
			r.endLocked()
		}
	})
}

func (r *runTracker) clearCeilingLocked() {
	r.ceilingGen++
	if r.ceilingTimer != nil {
		r.ceilingTimer.Stop()
		r.ceilingTimer = nil
	}
}

// runEndCeiling reads CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS as the CLI will see
// it: Options.Env first, then the process environment. Zero disables the
// ceiling; a value that is not a non-negative integer means the default.
func runEndCeiling(env map[string]string) time.Duration {
	const key = "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS"
	raw, ok := env[key]
	if !ok {
		raw, ok = os.LookupEnv(key)
	}
	if !ok {
		return defaultRunEndCeiling
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || ms < 0 {
		return defaultRunEndCeiling
	}
	if ms > int64(maxRunEndCeiling/time.Millisecond) {
		return maxRunEndCeiling
	}
	return time.Duration(ms) * time.Millisecond
}

// isDone reports whether ch is closed.
func isDone(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Error results
// ---------------------------------------------------------------------------

// errorResultTracker remembers the error result a non-zero CLI exit is
// expected to follow, mirroring the TypeScript SDK: the last error result,
// kept across frames that do not move the conversation on, and the error
// result before a run of model-less turns.
type errorResultTracker struct {
	mu                 sync.Mutex
	last               map[string]any
	beforeModelLess    map[string]any
	modelLessTurnEnded bool
}

// onResult records a result frame.
func (t *errorResultTracker) onResult(frame map[string]any) {
	isErr, _ := frame["is_error"].(bool)
	numTurns, _ := toInt(frame["num_turns"])
	result, hasResult := frame["result"].(string)

	t.mu.Lock()
	defer t.mu.Unlock()
	if isErr {
		t.last = frame
	} else {
		t.last = nil
	}
	// A model-less turn (a local command, say) keeps the error that came
	// before it, should the CLI exit non-zero after it.
	t.modelLessTurnEnded = str(frame["subtype"]) == "success" && !isErr &&
		numTurns == 0 && hasResult && result == ""
	if !t.modelLessTurnEnded {
		t.beforeModelLess = t.last
	}
}

// onActivity records a frame that moves the conversation on: a later non-zero
// exit is then a fresh failure rather than the expected exit after an error
// result.
func (t *errorResultTracker) onActivity(typ, subtype string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = nil
	if typ == "system" && subtype == "init" {
		t.modelLessTurnEnded = false
	} else if typ != "system" || subtype == "compact_boundary" {
		t.beforeModelLess = nil
		t.modelLessTurnEnded = false
	}
}

// translate turns a ProcessError that follows an error result into a
// ResultError carrying that result; any other error is returned as is.
func (t *errorResultTracker) translate(err error) error {
	perr, ok := errors.AsType[*ProcessError](err)
	if !ok {
		return err
	}
	t.mu.Lock()
	last := t.last
	if last == nil && t.modelLessTurnEnded {
		last = t.beforeModelLess
	}
	t.mu.Unlock()
	if last == nil {
		return err
	}
	// The CLI exits non-zero on purpose after reporting an error result;
	// the generic exit-code error carries nothing the result does not
	// already say.
	return newErrorResultError(last, perr.ExitCode)
}

// newErrorResultError reports a failed result frame as a ResultError.
func newErrorResultError(frame map[string]any, exitCode *int) *ResultError {
	return NewResultError("Claude Code returned an error result: "+errorResultText(frame), frame, exitCode)
}

// errorResultText picks the most informative text out of a failed result frame.
func errorResultText(frame map[string]any) string {
	if errs := normalizeResultErrors(frame["errors"]); len(errs) > 0 {
		return strings.Join(errs, "; ")
	}
	if result := strings.TrimSpace(str(frame["result"])); result != "" {
		return result
	}
	if subtype := str(frame["subtype"]); subtype != "" && subtype != "success" {
		return subtype
	}
	if status, ok := toInt(frame["api_error_status"]); ok {
		return fmt.Sprintf("API error (HTTP %d)", status)
	}
	return "unknown error"
}
