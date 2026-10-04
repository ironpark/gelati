package claude

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"

	"github.com/ironpark/gelati/internal/jsonx"
)

// HookDecision is the top-level decision a hook may return in
// HookOutput.Decision.
type HookDecision = string

// Hook decisions.
const (
	HookDecisionApprove HookDecision = "approve"
	HookDecisionBlock   HookDecision = "block"
)

// HookPermissionDecision is a PreToolUse hook's verdict on a tool call, sent
// as hookSpecificOutput.permissionDecision.
type HookPermissionDecision = string

// Hook permission decisions. PermissionDecisionDefer hands the call back to
// the host, which surfaces it as ResultMessage.DeferredToolUse.
const (
	PermissionDecisionAllow HookPermissionDecision = "allow"
	PermissionDecisionDeny  HookPermissionDecision = "deny"
	PermissionDecisionAsk   HookPermissionDecision = "ask"
	PermissionDecisionDefer HookPermissionDecision = "defer"
)

// HookOutput is what a hook callback returns. A zero value means "no opinion":
// nothing is sent back beyond an empty object.
//
// Field names on the wire follow the CLI's documented JSON schema; Continue and
// Async are emitted as "continue" and "async".
type HookOutput struct {
	// Continue reports whether Claude should proceed. nil leaves it unset
	// (the CLI defaults to true).
	Continue *bool `json:"continue,omitempty"`
	// SuppressOutput hides stdout from transcript mode.
	SuppressOutput *bool `json:"suppressOutput,omitempty"`
	// StopReason is shown to the user when Continue is false.
	StopReason string `json:"stopReason,omitempty"`
	// Decision is HookDecisionBlock to block the action or
	// HookDecisionApprove to approve it.
	Decision HookDecision `json:"decision,omitempty"`
	// SystemMessage is a warning displayed to the user.
	SystemMessage string `json:"systemMessage,omitempty"`
	// Reason is feedback for Claude about the decision.
	Reason string `json:"reason,omitempty"`
	// TerminalSequence is a terminal escape sequence the CLI writes to its
	// terminal: an OSC 0/1/2 title, OSC 9/99/777 notification or BEL.
	TerminalSequence string `json:"terminalSequence,omitempty"`
	// HookSpecificOutput carries event-specific fields, e.g.
	// {"hookEventName": "PreToolUse", "permissionDecision": "allow"}.
	HookSpecificOutput map[string]any `json:"hookSpecificOutput,omitzero"`
	// Specific is the typed form of HookSpecificOutput (for example
	// *PreToolUseHookSpecificOutput). Its fields, including hookEventName,
	// are merged over HookSpecificOutput on the wire.
	Specific HookSpecific `json:"-"`
	// Async defers hook execution; the CLI continues without waiting.
	Async bool `json:"async,omitempty"`
	// AsyncTimeout is the timeout in milliseconds for an async hook.
	AsyncTimeout *int `json:"asyncTimeout,omitempty"`
	// Extra holds output keys this SDK version does not model. The
	// TypeScript SDK forwards a callback's output verbatim, so Extra is
	// merged in on marshal; modeled fields win.
	Extra map[string]any `json:"-"`
}

// hookOutputFields are the output keys HookOutput has a field for.
var hookOutputFields = jsonMemberNames(reflect.TypeFor[HookOutput]())

// MarshalJSON emits the CLI wire format. An async output carries only async
// and asyncTimeout; otherwise Specific is merged into hookSpecificOutput.
func (h HookOutput) MarshalJSON() ([]byte, error) {
	type alias HookOutput
	out := alias(h)
	if h.Async {
		out = alias{Async: true, AsyncTimeout: h.AsyncTimeout}
	} else {
		out.AsyncTimeout = nil
		specific, err := mergeHookSpecificOutput(h.HookSpecificOutput, h.Specific)
		if err != nil {
			return nil, err
		}
		out.HookSpecificOutput = specific
	}
	return marshalWithExtra(out, h.Extra)
}

// UnmarshalJSON reads the CLI wire format produced by MarshalJSON. The
// event-specific output lands in HookSpecificOutput; Specific stays nil.
func (h *HookOutput) UnmarshalJSON(data []byte) error {
	type alias HookOutput
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	extra, err := jsonx.ExtraFields(data, func(k string) bool { return hookOutputFields[k] })
	if err != nil {
		return err
	}
	*h = HookOutput(a)
	h.Extra = extra
	return nil
}

// ---------------------------------------------------------------------------
// Typed hook-specific outputs
// ---------------------------------------------------------------------------

// HookSpecific is the typed form of HookOutput.HookSpecificOutput: one pointer
// to a per-event struct such as *PreToolUseHookSpecificOutput. The event name
// is supplied by the type, so hookEventName never has to be set by hand. For
// events without a struct here, use the HookSpecificOutput map.
//
// The interface is intentionally open: a type of your own that marshals to
// the event's fields and reports its event name also works, e.g. for an event
// newer than this SDK version.
type HookSpecific interface {
	// HookEventName names the event the output belongs to.
	HookEventName() HookEvent
}

// PreToolUseHookSpecificOutput answers a PreToolUse hook.
type PreToolUseHookSpecificOutput struct {
	PermissionDecision       HookPermissionDecision `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string                 `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             map[string]any         `json:"updatedInput,omitempty"`
	AdditionalContext        string                 `json:"additionalContext,omitempty"`
}

// HookEventName reports PreToolUse.
func (*PreToolUseHookSpecificOutput) HookEventName() HookEvent { return HookPreToolUse }

// UserPromptSubmitHookSpecificOutput answers a UserPromptSubmit hook.
type UserPromptSubmitHookSpecificOutput struct {
	AdditionalContext      string `json:"additionalContext,omitempty"`
	SessionTitle           string `json:"sessionTitle,omitempty"`
	SuppressOriginalPrompt bool   `json:"suppressOriginalPrompt,omitempty"`
}

// HookEventName reports UserPromptSubmit.
func (*UserPromptSubmitHookSpecificOutput) HookEventName() HookEvent { return HookUserPromptSubmit }

// UserPromptExpansionHookSpecificOutput answers a UserPromptExpansion hook.
type UserPromptExpansionHookSpecificOutput struct {
	AdditionalContext      string `json:"additionalContext,omitempty"`
	SuppressOriginalPrompt bool   `json:"suppressOriginalPrompt,omitempty"`
}

// HookEventName reports UserPromptExpansion.
func (*UserPromptExpansionHookSpecificOutput) HookEventName() HookEvent {
	return HookUserPromptExpansion
}

// SessionStartHookSpecificOutput answers a SessionStart hook.
type SessionStartHookSpecificOutput struct {
	AdditionalContext  string   `json:"additionalContext,omitempty"`
	InitialUserMessage string   `json:"initialUserMessage,omitempty"`
	SessionTitle       string   `json:"sessionTitle,omitempty"`
	WatchPaths         []string `json:"watchPaths,omitempty"`
	ReloadSkills       bool     `json:"reloadSkills,omitempty"`
}

// HookEventName reports SessionStart.
func (*SessionStartHookSpecificOutput) HookEventName() HookEvent { return HookSessionStart }

// SetupHookSpecificOutput answers a Setup hook.
type SetupHookSpecificOutput struct {
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// HookEventName reports Setup.
func (*SetupHookSpecificOutput) HookEventName() HookEvent { return HookSetup }

// PreModelSwitchHookSpecificOutput answers a PreModelSwitch hook.
// PermissionDecision is allow, deny or ask (defer is not accepted here).
type PreModelSwitchHookSpecificOutput struct {
	PermissionDecision       HookPermissionDecision `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string                 `json:"permissionDecisionReason,omitempty"`
}

// HookEventName reports PreModelSwitch.
func (*PreModelSwitchHookSpecificOutput) HookEventName() HookEvent { return HookPreModelSwitch }

// PostModelSwitchHookSpecificOutput answers a PostModelSwitch hook.
type PostModelSwitchHookSpecificOutput struct {
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// HookEventName reports PostModelSwitch.
func (*PostModelSwitchHookSpecificOutput) HookEventName() HookEvent { return HookPostModelSwitch }

// SubagentStartHookSpecificOutput answers a SubagentStart hook.
type SubagentStartHookSpecificOutput struct {
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// HookEventName reports SubagentStart.
func (*SubagentStartHookSpecificOutput) HookEventName() HookEvent { return HookSubagentStart }

// PostToolUseHookSpecificOutput answers a PostToolUse hook.
type PostToolUseHookSpecificOutput struct {
	AdditionalContext string `json:"additionalContext,omitempty"`
	ClassifierContext string `json:"classifierContext,omitempty"`
	// UpdatedToolOutput replaces a built-in tool's output.
	UpdatedToolOutput any `json:"updatedToolOutput,omitempty"`
	// UpdatedMCPToolOutput replaces an MCP tool's output.
	UpdatedMCPToolOutput any `json:"updatedMCPToolOutput,omitempty"`
}

// HookEventName reports PostToolUse.
func (*PostToolUseHookSpecificOutput) HookEventName() HookEvent { return HookPostToolUse }

// PostToolUseFailureHookSpecificOutput answers a PostToolUseFailure hook.
type PostToolUseFailureHookSpecificOutput struct {
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// HookEventName reports PostToolUseFailure.
func (*PostToolUseFailureHookSpecificOutput) HookEventName() HookEvent {
	return HookPostToolUseFailure
}

// PostToolBatchHookSpecificOutput answers a PostToolBatch hook.
type PostToolBatchHookSpecificOutput struct {
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// HookEventName reports PostToolBatch.
func (*PostToolBatchHookSpecificOutput) HookEventName() HookEvent { return HookPostToolBatch }

// StopHookSpecificOutput answers a Stop hook.
type StopHookSpecificOutput struct {
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// HookEventName reports Stop.
func (*StopHookSpecificOutput) HookEventName() HookEvent { return HookStop }

// SubagentStopHookSpecificOutput answers a SubagentStop hook.
type SubagentStopHookSpecificOutput struct {
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// HookEventName reports SubagentStop.
func (*SubagentStopHookSpecificOutput) HookEventName() HookEvent { return HookSubagentStop }

// PermissionDeniedHookSpecificOutput answers a PermissionDenied hook. Retry
// asks the CLI to retry the denied call.
type PermissionDeniedHookSpecificOutput struct {
	Retry bool `json:"retry,omitempty"`
}

// HookEventName reports PermissionDenied.
func (*PermissionDeniedHookSpecificOutput) HookEventName() HookEvent { return HookPermissionDenied }

// NotificationHookSpecificOutput answers a Notification hook.
type NotificationHookSpecificOutput struct {
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// HookEventName reports Notification.
func (*NotificationHookSpecificOutput) HookEventName() HookEvent { return HookNotification }

// PermissionRequestHookSpecificOutput answers a PermissionRequest hook.
// Decision is a *PermissionResultAllow or *PermissionResultDeny; nil leaves
// the request to the normal permission flow.
type PermissionRequestHookSpecificOutput struct {
	Decision PermissionResult
}

// HookEventName reports PermissionRequest.
func (*PermissionRequestHookSpecificOutput) HookEventName() HookEvent {
	return HookPermissionRequest
}

// MarshalJSON emits {"decision": {"behavior": ...}}.
func (o *PermissionRequestHookSpecificOutput) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	if o.Decision != nil {
		decision, ok := permissionDecisionWire(o.Decision, nil)
		if !ok {
			return nil, fmt.Errorf("claude: PermissionRequest hook decision %T, want *PermissionResultAllow or *PermissionResultDeny", o.Decision)
		}
		out["decision"] = decision
	}
	return json.Marshal(out)
}

// ElicitationHookSpecificOutput answers an Elicitation hook on the user's
// behalf. Action is accept, decline or cancel.
type ElicitationHookSpecificOutput struct {
	Action  string         `json:"action,omitempty"`
	Content map[string]any `json:"content,omitempty"`
}

// HookEventName reports Elicitation.
func (*ElicitationHookSpecificOutput) HookEventName() HookEvent { return HookElicitation }

// ElicitationResultHookSpecificOutput overrides an elicitation's result.
type ElicitationResultHookSpecificOutput struct {
	Action  string         `json:"action,omitempty"`
	Content map[string]any `json:"content,omitempty"`
}

// HookEventName reports ElicitationResult.
func (*ElicitationResultHookSpecificOutput) HookEventName() HookEvent {
	return HookElicitationResult
}

// CwdChangedHookSpecificOutput answers a CwdChanged hook.
type CwdChangedHookSpecificOutput struct {
	WatchPaths []string `json:"watchPaths,omitempty"`
}

// HookEventName reports CwdChanged.
func (*CwdChangedHookSpecificOutput) HookEventName() HookEvent { return HookCwdChanged }

// FileChangedHookSpecificOutput answers a FileChanged hook.
type FileChangedHookSpecificOutput struct {
	WatchPaths []string `json:"watchPaths,omitempty"`
}

// HookEventName reports FileChanged.
func (*FileChangedHookSpecificOutput) HookEventName() HookEvent { return HookFileChanged }

// WorktreeCreateHookSpecificOutput answers a WorktreeCreate hook with the
// path of the worktree the hook created.
type WorktreeCreateHookSpecificOutput struct {
	WorktreePath string `json:"worktreePath"`
}

// HookEventName reports WorktreeCreate.
func (*WorktreeCreateHookSpecificOutput) HookEventName() HookEvent { return HookWorktreeCreate }

// MessageDisplayHookSpecificOutput answers a MessageDisplay hook with the
// text to display in place of the streamed lines.
type MessageDisplayHookSpecificOutput struct {
	DisplayContent string `json:"displayContent,omitempty"`
}

// HookEventName reports MessageDisplay.
func (*MessageDisplayHookSpecificOutput) HookEventName() HookEvent { return HookMessageDisplay }

// mergeHookSpecificOutput lays the typed output over the map form, stamping
// hookEventName from the type. With no typed output the map is returned as is.
func mergeHookSpecificOutput(base map[string]any, typed HookSpecific) (map[string]any, error) {
	if typed == nil {
		return base, nil
	}
	fields, err := toWireMap(typed, "hook-specific output")
	if err != nil {
		return nil, err
	}
	if fields == nil {
		// A typed nil pointer.
		return base, nil
	}
	out := make(map[string]any, len(base)+len(fields)+1)
	maps.Copy(out, base)
	maps.Copy(out, fields)
	out["hookEventName"] = typed.HookEventName()
	return out, nil
}
