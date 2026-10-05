package claude

import (
	"log/slog"
	"maps"
	"slices"
	"strconv"

	"github.com/ironpark/gelati/internal/logx"
)

// engineConfig is the part of Options the engine consults while the session
// runs, resolved once when the engine is built.
type engineConfig struct {
	// Callbacks answering the CLI's inbound control requests.
	canUseTool    CanUseTool
	onElicitation OnElicitation
	onUserDialog  OnUserDialog
	// dialogKinds are the dialog kinds onUserDialog declared it renders;
	// empty means every kind.
	dialogKinds []string

	// hasHooks reports configured hooks, which keep the input open until
	// the run ends.
	hasHooks bool
	// hookCallbacks answers hook_callback requests by callback ID.
	hookCallbacks map[string]HookCallback
	// hooksWire is the hooks field of the initialize request. It is built
	// once, so a re-initialize registers the same callback IDs.
	hooksWire map[string]any
	// initFields are the option-derived initialize fields of the session's
	// launchConfig.
	initFields map[string]any

	// verbatimPrompts stamps every user message as client-composed.
	verbatimPrompts bool

	logger *slog.Logger
}

// newEngineConfig resolves opts for the engine. launch supplies the
// option-derived initialize fields.
func newEngineConfig(opts *Options, launch *launchConfig) engineConfig {
	callbacks, hooksWire := buildHookRegistry(opts.Hooks)
	return engineConfig{
		canUseTool:      opts.CanUseTool,
		onElicitation:   opts.OnElicitation,
		onUserDialog:    opts.OnUserDialog,
		dialogKinds:     opts.SupportedDialogKinds,
		hasHooks:        len(opts.Hooks) > 0,
		hookCallbacks:   callbacks,
		hooksWire:       hooksWire,
		initFields:      launch.initFields,
		verbatimPrompts: opts.VerbatimPrompts,
		logger:          logx.Or(opts.Logger),
	}
}

// buildHookRegistry assigns callback IDs to the configured hooks, in event
// order, and returns the callbacks by ID together with the initialize
// request's hooks field (empty when there are none).
func buildHookRegistry(hooks map[HookEvent][]HookMatcher) (callbacks map[string]HookCallback, wire map[string]any) {
	callbacks = map[string]HookCallback{}
	wire = map[string]any{}
	next := 0
	for _, event := range slices.Sorted(maps.Keys(hooks)) {
		matchers := hooks[event]
		if len(matchers) == 0 {
			continue
		}
		configs := make([]map[string]any, 0, len(matchers))
		for _, matcher := range matchers {
			ids := make([]string, 0, len(matcher.Hooks))
			for _, cb := range matcher.Hooks {
				id := "hook_" + strconv.Itoa(next)
				next++
				callbacks[id] = cb
				ids = append(ids, id)
			}
			config := map[string]any{"matcher": nil, "hookCallbackIds": ids}
			if matcher.Matcher != "" {
				config["matcher"] = matcher.Matcher
			}
			if matcher.Timeout > 0 {
				config["timeout"] = matcher.Timeout
			}
			configs = append(configs, config)
		}
		wire[event] = configs
	}
	return callbacks, wire
}
