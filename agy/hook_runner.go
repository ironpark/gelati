package agy

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"reflect"
	"strings"

	"github.com/ironpark/gelati/agy/internal/wire"
	"github.com/ironpark/gelati/internal/safecall"
)

// hookRunner holds the registered hooks by kind and dispatches lifecycle
// events to them (upstream HookRunner). Its hook lists are fixed at
// construction, so dispatching needs no locking.
type hookRunner struct {
	hooks   [numHookKinds][]Hook
	session *HookContext
}

// lifecycleHooks maps the hook kinds to the lifecycle hooks the harness
// calls back for. Interaction hooks have none: questions arrive as steps.
var lifecycleHooks = [numHookKinds]wire.LifecycleHook{
	hookSessionStart: wire.LifecycleHook_LIFECYCLE_HOOK_ON_SESSION_START,
	hookSessionEnd:   wire.LifecycleHook_LIFECYCLE_HOOK_ON_SESSION_END,
	hookPreTurn:      wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TURN,
	hookPostTurn:     wire.LifecycleHook_LIFECYCLE_HOOK_POST_TURN,
	hookPreToolCall:  wire.LifecycleHook_LIFECYCLE_HOOK_PRE_TOOL,
	hookPostToolCall: wire.LifecycleHook_LIFECYCLE_HOOK_POST_TOOL,
	hookToolError:    wire.LifecycleHook_LIFECYCLE_HOOK_ON_TOOL_ERROR,
	hookCompaction:   wire.LifecycleHook_LIFECYCLE_HOOK_ON_COMPACTION,
	hookStop:         wire.LifecycleHook_LIFECYCLE_HOOK_STOP,
}

func newHookRunner(hooks []Hook) (*hookRunner, error) {
	r := &hookRunner{session: newHookContext(ScopeSession, nil)}
	for i, h := range hooks {
		// Every Hook is a func type, so a nil hook is a nil func or a nil
		// interface.
		if h == nil || reflect.ValueOf(h).IsNil() {
			return nil, fmt.Errorf("hook %d: %w", i, validationErrorf("unknown or nil hook %T", h))
		}
		r.hooks[h.hookKind()] = append(r.hooks[h.hookKind()], h)
	}
	return r, nil
}

// has reports whether a hook of the kind is registered.
func (r *hookRunner) has(kind hookKind) bool { return len(r.hooks[kind]) > 0 }

func (r *hookRunner) newTurnContext() *HookContext { return newHookContext(ScopeTurn, r.session) }

// each yields the registered hooks of type H in registration order.
func each[H Hook](r *hookRunner) iter.Seq[H] {
	var zero H
	return func(yield func(H) bool) {
		for _, h := range r.hooks[zero.hookKind()] {
			if !yield(h.(H)) {
				return
			}
		}
	}
}

func callInspect(fn func() error) (err error) {
	defer safecall.Recover(&err, "agy: hook")
	return fn()
}

// runAll calls the hooks of type H in order, through call, until one fails.
func runAll[H Hook](r *hookRunner, call func(H) error) error {
	for h := range each[H](r) {
		if err := callInspect(func() error { return call(h) }); err != nil {
			return err
		}
	}
	return nil
}

func (r *hookRunner) dispatchSessionStart(ctx context.Context) error {
	return runAll(r, func(h OnSessionStartHook) error { return h(ctx, r.session) })
}

func (r *hookRunner) dispatchSessionEnd(ctx context.Context) error {
	return runAll(r, func(h OnSessionEndHook) error { return h(ctx, r.session) })
}

// dispatchPreTurn runs the pre-turn hooks and returns the first denial, or
// an allowing result.
func (r *hookRunner) dispatchPreTurn(ctx context.Context, turn *HookContext, prompt []Content) (HookResult, error) {
	if len(prompt) == 0 {
		prompt = []Content{Text("")}
	}
	for h := range each[PreTurnHook](r) {
		var res HookResult
		err := callInspect(func() (err error) { res, err = h(ctx, turn, prompt); return })
		if err != nil {
			return HookResult{}, err
		}
		if res.Deny {
			return res, nil
		}
	}
	return HookResult{}, nil
}

func (r *hookRunner) dispatchPostTurn(ctx context.Context, turn *HookContext, response string) error {
	return runAll(r, func(h PostTurnHook) error { return h(ctx, turn, response) })
}

// dispatchPreToolCall runs the decide hooks in a new operation context. Each
// hook sees the arguments as modified by the previous ones; the first denial
// wins. When any hook modified the arguments, the allowing result carries
// the fully merged arguments.
func (r *hookRunner) dispatchPreToolCall(ctx context.Context, turn *HookContext, call *ToolCall) (HookResult, error) {
	op := newHookContext(ScopeOperation, turn)
	current := call.clone()
	var last HookResult
	modified := false
	for h := range each[PreToolCallHook](r) {
		var res HookResult
		err := callInspect(func() (err error) { res, err = h(ctx, op, current.clone()); return })
		if err != nil {
			return HookResult{}, err
		}
		if res.Deny {
			return res, nil
		}
		last = res
		if res.ModifiedArgs != nil {
			merged := maps.Clone(current.Args)
			if merged == nil {
				merged = make(map[string]any, len(res.ModifiedArgs))
			}
			maps.Copy(merged, res.ModifiedArgs)
			current.Args = merged
			modified = true
		}
	}
	if modified {
		last.ModifiedArgs = maps.Clone(current.Args)
	}
	return last, nil
}

func (r *hookRunner) dispatchPostToolCall(ctx context.Context, op *HookContext, result *ToolResult) error {
	return runAll(r, func(h PostToolCallHook) error { return h(ctx, op, result) })
}

// dispatchToolError returns the first non-empty recovery message, or "". A
// failing hook ends the dispatch with no message.
func (r *hookRunner) dispatchToolError(ctx context.Context, op *HookContext, toolErr *ToolExecutionError) string {
	for h := range each[OnToolErrorHook](r) {
		var msg string
		if err := callInspect(func() (err error) { msg, err = h(ctx, op, toolErr); return }); err != nil {
			return ""
		}
		if msg != "" {
			return msg
		}
	}
	return ""
}

// dispatchInteraction returns the first non-nil answer, or nil when no hook
// answered.
func (r *hookRunner) dispatchInteraction(ctx context.Context, turn *HookContext, spec AskQuestionInteractionSpec) (*QuestionHookResult, error) {
	op := newHookContext(ScopeOperation, turn)
	for h := range each[OnInteractionHook](r) {
		var res *QuestionHookResult
		if err := callInspect(func() (err error) { res, err = h(ctx, op, spec); return }); err != nil {
			return nil, err
		}
		if res != nil {
			return res, nil
		}
	}
	return nil, nil
}

func (r *hookRunner) dispatchCompaction(ctx context.Context, turn *HookContext, step *Step) error {
	op := newHookContext(ScopeOperation, turn)
	return runAll(r, func(h OnCompactionHook) error { return h(ctx, op, step) })
}

// dispatchStop returns the first StopDecisionContinue result, or
// StopDecisionAllowStop.
func (r *hookRunner) dispatchStop(ctx context.Context, turn *HookContext, args StopArgs) (StopHookResult, error) {
	for h := range each[StopHook](r) {
		var res StopHookResult
		err := callInspect(func() (err error) { res, err = h(ctx, turn, args); return })
		if err != nil {
			return StopHookResult{}, err
		}
		switch res.Decision {
		case "", StopDecisionAllowStop:
		case StopDecisionContinue:
			if strings.TrimSpace(res.Reason) == "" {
				return StopHookResult{}, validationErrorf("StopHookResult with decision=CONTINUE requires a non-empty reason.")
			}
			return res, nil
		default:
			return StopHookResult{}, validationErrorf("unknown stop decision %q", res.Decision)
		}
	}
	return StopHookResult{Decision: StopDecisionAllowStop}, nil
}
