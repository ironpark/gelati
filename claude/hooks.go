package claude

import "context"

// Hook registration. The events are listed in messages_enums.go, the typed
// inputs in hook_inputs.go and the outputs in hook_outputs.go.

// HookCallback runs for a matching hook event. input is the raw hook input dict
// keyed by the CLI's schema (hook_event_name, tool_name, ...); toolUseID is
// empty when the event is not tool-scoped. DecodeHookInput turns input into a
// typed per-event struct, and TypedHook adapts a typed function into a
// HookCallback.
type HookCallback func(ctx context.Context, input map[string]any, toolUseID string, hookCtx HookContext) (HookOutput, error)

// HookContext carries per-invocation context for a hook callback. The abort
// signal of the TypeScript SDK is the callback's ctx, which is cancelled when
// the CLI cancels the request.
type HookContext struct {
	// Raw is the hook input exactly as the CLI sent it, including fields
	// the typed inputs do not model. Typed hooks (see TypedHook) read newer
	// CLI fields from here.
	Raw map[string]any
}

// HookMatcher registers callbacks for one hook event.
type HookMatcher struct {
	// Matcher narrows which invocations fire the hooks, e.g. "Bash" or
	// "Write|Edit" for PreToolUse. Empty matches everything.
	Matcher string
	// Hooks are the callbacks to run. The CLI dispatches matchers for one
	// event concurrently.
	Hooks []HookCallback
	// Timeout bounds all hooks in this matcher, in seconds. Zero uses the
	// CLI default of 60.
	Timeout float64
}
