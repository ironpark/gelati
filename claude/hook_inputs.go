package claude

import (
	"context"
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------------------
// Typed hook inputs
// ---------------------------------------------------------------------------

// HookInput is the typed form of a hook callback's input: one pointer to a
// per-event struct such as *PreToolUseHookInput, or *UnknownHookInput for an
// event this SDK version does not model. Build one with DecodeHookInput, or
// register a typed callback with TypedHook.
type HookInput interface {
	// Base returns the fields every hook input carries.
	Base() *BaseHookInput
	isHookInput()
}

// HookEffort is the effort level active when a hook fired.
type HookEffort struct {
	Level string `json:"level"`
}

// BaseHookInput holds the fields common to every hook input. It is embedded
// in each per-event struct.
type BaseHookInput struct {
	// HookEventName names the event, one of HookEvents.
	HookEventName HookEvent `json:"hook_event_name"`
	SessionID     string    `json:"session_id"`
	// TranscriptPath is the session transcript file.
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	// PromptID identifies the user prompt that started the turn.
	PromptID       string         `json:"prompt_id,omitempty"`
	PermissionMode PermissionMode `json:"permission_mode,omitempty"`
	// AgentID and AgentType identify the subagent the hook fired in; they
	// are empty on the main thread. SubagentStart and SubagentStop always
	// set them.
	AgentID   string      `json:"agent_id,omitempty"`
	AgentType string      `json:"agent_type,omitempty"`
	Effort    *HookEffort `json:"effort,omitempty"`
}

// Base returns b, so every per-event struct satisfies HookInput through the
// embedded BaseHookInput.
func (b *BaseHookInput) Base() *BaseHookInput { return b }

func (*BaseHookInput) isHookInput() {}

// BackgroundTaskSummary describes a background task still running when a Stop
// or SubagentStop hook fires.
type BackgroundTaskSummary struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Description string `json:"description"`
	Command     string `json:"command,omitempty"`
	AgentType   string `json:"agent_type,omitempty"`
	Server      string `json:"server,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Name        string `json:"name,omitempty"`
}

// SessionCronSummary describes a scheduled prompt of the session.
type SessionCronSummary struct {
	ID        string `json:"id"`
	Schedule  string `json:"schedule"`
	Recurring bool   `json:"recurring"`
	Prompt    string `json:"prompt"`
}

// PostToolBatchToolCall is one tool call of a PostToolBatch batch.
type PostToolBatchToolCall struct {
	ToolName     string         `json:"tool_name"`
	ToolInput    map[string]any `json:"tool_input"`
	ToolUseID    string         `json:"tool_use_id"`
	ToolResponse any            `json:"tool_response,omitempty"`
}

// ExitReason is why a session ended, reported by SessionEnd hooks.
type ExitReason = string

// Known exit reasons, in the TypeScript SDK's EXIT_REASONS order.
const (
	ExitReasonClear           ExitReason = "clear"
	ExitReasonResume          ExitReason = "resume"
	ExitReasonLogout          ExitReason = "logout"
	ExitReasonPromptInputExit ExitReason = "prompt_input_exit"
	ExitReasonOther           ExitReason = "other"
)

// ExitReasons lists the known exit reasons.
var ExitReasons = []ExitReason{
	ExitReasonClear, ExitReasonResume, ExitReasonLogout,
	ExitReasonPromptInputExit, ExitReasonOther,
}

// PreToolUseHookInput is the input of a PreToolUse hook.
type PreToolUseHookInput struct {
	BaseHookInput
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	ToolUseID string         `json:"tool_use_id"`
	// MCPServer is set for mcp__* tools.
	MCPServer *MCPServerProvenance `json:"mcp_server,omitempty"`
}

// PostToolUseHookInput is the input of a PostToolUse hook.
type PostToolUseHookInput struct {
	BaseHookInput
	ToolName     string               `json:"tool_name"`
	ToolInput    map[string]any       `json:"tool_input"`
	ToolResponse any                  `json:"tool_response"`
	ToolUseID    string               `json:"tool_use_id"`
	DurationMS   *float64             `json:"duration_ms,omitempty"`
	MCPServer    *MCPServerProvenance `json:"mcp_server,omitempty"`
}

// PostToolUseFailureHookInput is the input of a PostToolUseFailure hook.
type PostToolUseFailureHookInput struct {
	BaseHookInput
	ToolName    string               `json:"tool_name"`
	ToolInput   map[string]any       `json:"tool_input"`
	ToolUseID   string               `json:"tool_use_id"`
	Error       string               `json:"error"`
	IsInterrupt bool                 `json:"is_interrupt,omitempty"`
	DurationMS  *float64             `json:"duration_ms,omitempty"`
	MCPServer   *MCPServerProvenance `json:"mcp_server,omitempty"`
}

// PostToolBatchHookInput is the input of a PostToolBatch hook, fired once a
// batch of parallel tool calls has finished.
type PostToolBatchHookInput struct {
	BaseHookInput
	ToolCalls []PostToolBatchToolCall `json:"tool_calls"`
}

// NotificationHookInput is the input of a Notification hook.
type NotificationHookInput struct {
	BaseHookInput
	Message          string `json:"message"`
	Title            string `json:"title,omitempty"`
	NotificationType string `json:"notification_type"`
}

// UserPromptSubmitHookInput is the input of a UserPromptSubmit hook.
type UserPromptSubmitHookInput struct {
	BaseHookInput
	Prompt string `json:"prompt"`
	// Source is user, sdk, system, loop_wakeup, schedule_wakeup or
	// poll_event.
	Source       string `json:"source,omitempty"`
	SessionTitle string `json:"session_title,omitempty"`
}

// UserPromptExpansionHookInput is the input of a UserPromptExpansion hook.
type UserPromptExpansionHookInput struct {
	BaseHookInput
	// ExpansionType is slash_command or mcp_prompt.
	ExpansionType string `json:"expansion_type"`
	CommandName   string `json:"command_name"`
	CommandArgs   string `json:"command_args"`
	CommandSource string `json:"command_source,omitempty"`
	Prompt        string `json:"prompt"`
}

// SessionStartHookInput is the input of a SessionStart hook. AgentType is on
// the embedded base.
type SessionStartHookInput struct {
	BaseHookInput
	// Source is startup, resume, clear, compact or fork.
	Source                   string   `json:"source"`
	Model                    string   `json:"model,omitempty"`
	SessionTitle             string   `json:"session_title,omitempty"`
	SecondsSinceLastResponse *float64 `json:"seconds_since_last_response,omitempty"`
	ContextTokens            *int     `json:"context_tokens,omitempty"`
	PromptCacheLikelyExpired *bool    `json:"prompt_cache_likely_expired,omitempty"`
	EstimatedCacheWriteUSD   *float64 `json:"estimated_cache_write_usd,omitempty"`
}

// SessionEndHookInput is the input of a SessionEnd hook.
type SessionEndHookInput struct {
	BaseHookInput
	Reason ExitReason `json:"reason"`
}

// StopHookInput is the input of a Stop hook.
type StopHookInput struct {
	BaseHookInput
	StopHookActive       bool                    `json:"stop_hook_active"`
	LastAssistantMessage string                  `json:"last_assistant_message,omitempty"`
	BackgroundTasks      []BackgroundTaskSummary `json:"background_tasks,omitempty"`
	SessionCrons         []SessionCronSummary    `json:"session_crons,omitempty"`
}

// StopFailureHookInput is the input of a StopFailure hook, fired when a turn
// ends on an API error.
type StopFailureHookInput struct {
	BaseHookInput
	Error                AssistantMessageError `json:"error"`
	ErrorDetails         string                `json:"error_details,omitempty"`
	LastAssistantMessage string                `json:"last_assistant_message,omitempty"`
}

// SubagentStartHookInput is the input of a SubagentStart hook. AgentID and
// AgentType are on the embedded base.
type SubagentStartHookInput struct {
	BaseHookInput
}

// SubagentStopHookInput is the input of a SubagentStop hook. AgentID and
// AgentType are on the embedded base.
type SubagentStopHookInput struct {
	BaseHookInput
	StopHookActive       bool                    `json:"stop_hook_active"`
	AgentTranscriptPath  string                  `json:"agent_transcript_path"`
	LastAssistantMessage string                  `json:"last_assistant_message,omitempty"`
	BackgroundTasks      []BackgroundTaskSummary `json:"background_tasks,omitempty"`
	SessionCrons         []SessionCronSummary    `json:"session_crons,omitempty"`
}

// PreCompactHookInput is the input of a PreCompact hook.
type PreCompactHookInput struct {
	BaseHookInput
	// Trigger is manual or auto.
	Trigger string `json:"trigger"`
	// CustomInstructions is empty when the CLI sent null.
	CustomInstructions string `json:"custom_instructions"`
}

// PostCompactHookInput is the input of a PostCompact hook.
type PostCompactHookInput struct {
	BaseHookInput
	// Trigger is manual or auto.
	Trigger        string `json:"trigger"`
	CompactSummary string `json:"compact_summary"`
}

// ModelSwitchHookFields are the fields shared by PreModelSwitch and
// PostModelSwitch inputs.
type ModelSwitchHookFields struct {
	FromModel string `json:"from_model"`
	ToModel   string `json:"to_model"`
	// RequestedModel is empty when the CLI sent null.
	RequestedModel string `json:"requested_model"`
	// Source is command, picker or sdk; PostModelSwitch adds auto and
	// resume.
	Source          string `json:"source"`
	ContextTokens   int    `json:"context_tokens"`
	PromptCacheWarm bool   `json:"prompt_cache_warm"`
	// CacheTTL is 5m or 1h.
	CacheTTL               string  `json:"cache_ttl"`
	EstimatedCacheWriteUSD float64 `json:"estimated_cache_write_usd"`
	// Pricing is configured, catalog or default.
	Pricing string `json:"pricing"`
}

// PreModelSwitchHookInput is the input of a PreModelSwitch hook.
type PreModelSwitchHookInput struct {
	BaseHookInput
	ModelSwitchHookFields
}

// PostModelSwitchHookInput is the input of a PostModelSwitch hook.
type PostModelSwitchHookInput struct {
	BaseHookInput
	ModelSwitchHookFields
}

// PermissionRequestHookInput is the input of a PermissionRequest hook.
type PermissionRequestHookInput struct {
	BaseHookInput
	ToolName              string               `json:"tool_name"`
	ToolInput             map[string]any       `json:"tool_input"`
	PermissionSuggestions []PermissionUpdate   `json:"permission_suggestions,omitempty"`
	MCPServer             *MCPServerProvenance `json:"mcp_server,omitempty"`
}

// PermissionDeniedHookInput is the input of a PermissionDenied hook.
type PermissionDeniedHookInput struct {
	BaseHookInput
	ToolName  string               `json:"tool_name"`
	ToolInput map[string]any       `json:"tool_input"`
	ToolUseID string               `json:"tool_use_id"`
	Reason    string               `json:"reason"`
	MCPServer *MCPServerProvenance `json:"mcp_server,omitempty"`
}

// SetupHookInput is the input of a Setup hook.
type SetupHookInput struct {
	BaseHookInput
	// Trigger is init or maintenance.
	Trigger string `json:"trigger"`
}

// TeammateIdleHookInput is the input of a TeammateIdle hook.
type TeammateIdleHookInput struct {
	BaseHookInput
	TeammateName string `json:"teammate_name"`
}

// TaskCreatedHookInput is the input of a TaskCreated hook.
type TaskCreatedHookInput struct {
	BaseHookInput
	TaskID          string `json:"task_id"`
	TaskSubject     string `json:"task_subject"`
	TaskDescription string `json:"task_description,omitempty"`
	TeammateName    string `json:"teammate_name,omitempty"`
}

// TaskCompletedHookInput is the input of a TaskCompleted hook.
type TaskCompletedHookInput struct {
	BaseHookInput
	TaskID          string `json:"task_id"`
	TaskSubject     string `json:"task_subject"`
	TaskDescription string `json:"task_description,omitempty"`
	TeammateName    string `json:"teammate_name,omitempty"`
}

// ElicitationHookInput is the input of an Elicitation hook, fired when an MCP
// server asks the user for input.
type ElicitationHookInput struct {
	BaseHookInput
	MCPServerName string `json:"mcp_server_name"`
	Message       string `json:"message"`
	// Mode is form or url.
	Mode            string         `json:"mode,omitempty"`
	URL             string         `json:"url,omitempty"`
	ElicitationID   string         `json:"elicitation_id,omitempty"`
	RequestedSchema map[string]any `json:"requested_schema,omitempty"`
}

// ElicitationResultHookInput is the input of an ElicitationResult hook.
type ElicitationResultHookInput struct {
	BaseHookInput
	MCPServerName string `json:"mcp_server_name"`
	ElicitationID string `json:"elicitation_id,omitempty"`
	Mode          string `json:"mode,omitempty"`
	// Action is accept, decline or cancel.
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitempty"`
}

// ConfigChangeHookInput is the input of a ConfigChange hook.
type ConfigChangeHookInput struct {
	BaseHookInput
	// Source is user_settings, project_settings, local_settings,
	// policy_settings or skills.
	Source   string `json:"source"`
	FilePath string `json:"file_path,omitempty"`
}

// WorktreeCreateHookInput is the input of a WorktreeCreate hook.
type WorktreeCreateHookInput struct {
	BaseHookInput
	Name string `json:"name"`
}

// WorktreeRemoveHookInput is the input of a WorktreeRemove hook.
type WorktreeRemoveHookInput struct {
	BaseHookInput
	WorktreePath string `json:"worktree_path"`
}

// InstructionsLoadedHookInput is the input of an InstructionsLoaded hook.
type InstructionsLoadedHookInput struct {
	BaseHookInput
	FilePath string `json:"file_path"`
	// MemoryType is User, Project, Local or Managed.
	MemoryType string `json:"memory_type"`
	// LoadReason is session_start, nested_traversal, path_glob_match,
	// include or compact.
	LoadReason      string   `json:"load_reason"`
	Globs           []string `json:"globs,omitempty"`
	TriggerFilePath string   `json:"trigger_file_path,omitempty"`
	ParentFilePath  string   `json:"parent_file_path,omitempty"`
}

// CwdChangedHookInput is the input of a CwdChanged hook.
type CwdChangedHookInput struct {
	BaseHookInput
	OldCwd string `json:"old_cwd"`
	NewCwd string `json:"new_cwd"`
}

// FileChangedHookInput is the input of a FileChanged hook.
type FileChangedHookInput struct {
	BaseHookInput
	FilePath string `json:"file_path"`
	// Event is change, add or unlink.
	Event string `json:"event"`
}

// DirectoryAddedHookInput is the input of a DirectoryAdded hook.
type DirectoryAddedHookInput struct {
	BaseHookInput
	Directory string `json:"directory"`
	// Source is slash_command or register_repo_root.
	Source string `json:"source"`
}

// MessageDisplayHookInput is the input of a MessageDisplay hook, fired with
// each batch of completed lines while an assistant message streams.
type MessageDisplayHookInput struct {
	BaseHookInput
	TurnID    string `json:"turn_id"`
	MessageID string `json:"message_id"`
	Index     int    `json:"index"`
	Final     bool   `json:"final"`
	Delta     string `json:"delta"`
}

// UnknownHookInput is the input of an event this SDK version does not model.
// Raw holds the complete input.
type UnknownHookInput struct {
	BaseHookInput
	Raw map[string]any `json:"-"`
}

// hookInputFactories builds the empty typed input for each known event.
var hookInputFactories = map[HookEvent]func() HookInput{
	HookPreToolUse:          func() HookInput { return &PreToolUseHookInput{} },
	HookPostToolUse:         func() HookInput { return &PostToolUseHookInput{} },
	HookPostToolUseFailure:  func() HookInput { return &PostToolUseFailureHookInput{} },
	HookPostToolBatch:       func() HookInput { return &PostToolBatchHookInput{} },
	HookNotification:        func() HookInput { return &NotificationHookInput{} },
	HookUserPromptSubmit:    func() HookInput { return &UserPromptSubmitHookInput{} },
	HookUserPromptExpansion: func() HookInput { return &UserPromptExpansionHookInput{} },
	HookSessionStart:        func() HookInput { return &SessionStartHookInput{} },
	HookSessionEnd:          func() HookInput { return &SessionEndHookInput{} },
	HookStop:                func() HookInput { return &StopHookInput{} },
	HookStopFailure:         func() HookInput { return &StopFailureHookInput{} },
	HookSubagentStart:       func() HookInput { return &SubagentStartHookInput{} },
	HookSubagentStop:        func() HookInput { return &SubagentStopHookInput{} },
	HookPreCompact:          func() HookInput { return &PreCompactHookInput{} },
	HookPostCompact:         func() HookInput { return &PostCompactHookInput{} },
	HookPreModelSwitch:      func() HookInput { return &PreModelSwitchHookInput{} },
	HookPostModelSwitch:     func() HookInput { return &PostModelSwitchHookInput{} },
	HookPermissionRequest:   func() HookInput { return &PermissionRequestHookInput{} },
	HookPermissionDenied:    func() HookInput { return &PermissionDeniedHookInput{} },
	HookSetup:               func() HookInput { return &SetupHookInput{} },
	HookTeammateIdle:        func() HookInput { return &TeammateIdleHookInput{} },
	HookTaskCreated:         func() HookInput { return &TaskCreatedHookInput{} },
	HookTaskCompleted:       func() HookInput { return &TaskCompletedHookInput{} },
	HookElicitation:         func() HookInput { return &ElicitationHookInput{} },
	HookElicitationResult:   func() HookInput { return &ElicitationResultHookInput{} },
	HookConfigChange:        func() HookInput { return &ConfigChangeHookInput{} },
	HookWorktreeCreate:      func() HookInput { return &WorktreeCreateHookInput{} },
	HookWorktreeRemove:      func() HookInput { return &WorktreeRemoveHookInput{} },
	HookInstructionsLoaded:  func() HookInput { return &InstructionsLoadedHookInput{} },
	HookCwdChanged:          func() HookInput { return &CwdChangedHookInput{} },
	HookFileChanged:         func() HookInput { return &FileChangedHookInput{} },
	HookDirectoryAdded:      func() HookInput { return &DirectoryAddedHookInput{} },
	HookMessageDisplay:      func() HookInput { return &MessageDisplayHookInput{} },
}

// DecodeHookInput converts a raw hook input into its typed per-event struct,
// switching on hook_event_name. An event this SDK does not model decodes to
// *UnknownHookInput rather than failing; an error means a field had an
// unexpected JSON type.
func DecodeHookInput(raw map[string]any) (HookInput, error) {
	payload, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("claude: encoding hook input: %w", err)
	}
	event, _ := raw["hook_event_name"].(string)
	factory, ok := hookInputFactories[event]
	if !ok {
		unknown := &UnknownHookInput{Raw: raw}
		if err := json.Unmarshal(payload, &unknown.BaseHookInput); err != nil {
			return nil, fmt.Errorf("claude: decoding %q hook input: %w", event, err)
		}
		return unknown, nil
	}
	in := factory()
	if err := json.Unmarshal(payload, in); err != nil {
		return nil, fmt.Errorf("claude: decoding %s hook input: %w", event, err)
	}
	return in, nil
}

// TypedHook adapts a function taking a typed input into a HookCallback. T is
// the pointer type of one per-event struct (for example *PreToolUseHookInput)
// or HookInput itself to receive every event. An input of another event, or
// one that fails to decode, makes the callback return an error instead of
// calling fn. The raw input stays reachable through HookContext.Raw.
//
//	hook := claude.TypedHook(func(ctx context.Context, in *claude.PreToolUseHookInput, toolUseID string, _ claude.HookContext) (claude.HookOutput, error) {
//		if in.ToolName == "Bash" {
//			return claude.HookOutput{Specific: &claude.PreToolUseHookSpecificOutput{
//				PermissionDecision:       claude.PermissionDecisionDeny,
//				PermissionDecisionReason: "no shell",
//			}}, nil
//		}
//		return claude.HookOutput{}, nil
//	})
func TypedHook[T HookInput](fn func(ctx context.Context, input T, toolUseID string, hookCtx HookContext) (HookOutput, error)) HookCallback {
	return func(ctx context.Context, raw map[string]any, toolUseID string, hookCtx HookContext) (HookOutput, error) {
		decoded, err := DecodeHookInput(raw)
		if err != nil {
			return HookOutput{}, err
		}
		typed, ok := decoded.(T)
		if !ok {
			var want T
			return HookOutput{}, fmt.Errorf("claude: hook expecting %T received a %q event", want, decoded.Base().HookEventName)
		}
		if hookCtx.Raw == nil {
			hookCtx.Raw = raw
		}
		return fn(ctx, typed, toolUseID, hookCtx)
	}
}
