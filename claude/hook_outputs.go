package claude

import (
	"encoding/json"
	"fmt"
	"maps"
)

// ---------------------------------------------------------------------------
// Typed hook-specific outputs
// ---------------------------------------------------------------------------

// HookSpecific is the typed form of HookOutput.HookSpecificOutput: one pointer
// to a per-event struct such as *PreToolUseHookSpecificOutput. The event name
// is supplied by the type, so hookEventName never has to be set by hand. For
// events without a struct here, use the HookSpecificOutput map.
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
	switch d := o.Decision.(type) {
	case nil:
	case *PermissionResultAllow:
		decision := map[string]any{"behavior": "allow"}
		if d.UpdatedInput != nil {
			decision["updatedInput"] = d.UpdatedInput
		}
		if d.UpdatedPermissions != nil {
			decision["updatedPermissions"] = d.UpdatedPermissions
		}
		out["decision"] = decision
	case *PermissionResultDeny:
		decision := map[string]any{"behavior": "deny"}
		if d.Message != "" {
			decision["message"] = d.Message
		}
		if d.Interrupt {
			decision["interrupt"] = true
		}
		out["decision"] = decision
	default:
		return nil, fmt.Errorf("claude: PermissionRequest hook decision %T, want *PermissionResultAllow or *PermissionResultDeny", d)
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
