package claude

import (
	"maps"
	"slices"
)

// SystemMessage is a metadata message. Data holds the full raw payload,
// including fields not modeled by the specialized subtypes.
type SystemMessage struct {
	Subtype string `json:"subtype"`
	// Data is the raw frame. It is left out of the JSON encoding, which
	// carries the modeled fields only.
	Data map[string]any `json:"-"`
}

func (*SystemMessage) isMessage() {}

// systemBase gives decodeSystem access to the embedded SystemMessage.
func (m *SystemMessage) systemBase() *SystemMessage { return m }

// Typed system messages. Each embeds SystemMessage, so Subtype and the raw
// payload in Data stay available, and a type switch on *SystemMessage does not
// match them: switch on the concrete type. A system subtype without a struct
// here arrives as a plain *SystemMessage.

// InitMessage (subtype "init") reports the session's configuration. The CLI
// emits one at the start of each turn, normally ahead of every other message
// of that turn.
type InitMessage struct {
	SystemMessage
	// Agents names the available subagents.
	Agents []string `json:"agents,omitempty"`
	// APIKeySource is one of the APIKeySource* constants.
	APIKeySource      APIKeySource `json:"apiKeySource"`
	Betas             []string     `json:"betas,omitempty"`
	ClaudeCodeVersion string       `json:"claude_code_version"`
	Cwd               string       `json:"cwd"`
	Tools             []string     `json:"tools"`
	// MCPServers lists the configured MCP servers and their connection
	// status.
	MCPServers     []InitMCPServer `json:"mcp_servers"`
	Model          string          `json:"model"`
	PermissionMode PermissionMode  `json:"permissionMode"`
	SlashCommands  []string        `json:"slash_commands"`
	// TerminalSlashCommands is the subset of SlashCommands bound to the local
	// terminal, which remote UIs should hide.
	TerminalSlashCommands []string `json:"terminal_slash_commands,omitempty"`
	OutputStyle           string   `json:"output_style"`
	Skills                []string `json:"skills"`
	// Plugins lists the loaded plugins; PluginErrors the load failures.
	Plugins                []InitPlugin           `json:"plugins"`
	PluginErrors           []InitPluginError      `json:"plugin_errors,omitempty"`
	FastModeState          FastModeState          `json:"fast_mode_state,omitempty"`
	FastModeDisabledReason FastModeDisabledReason `json:"fast_mode_disabled_reason,omitempty"`
	// Effort is the effort level the next request will send (low, medium,
	// high, xhigh, max); empty when none or not reported.
	Effort string `json:"effort,omitempty"`
	// ViewMode is "focus" or "default", when reported.
	ViewMode string `json:"view_mode,omitempty"`
	// Capabilities lists the protocol features the CLI supports (see the
	// Capability* constants). Use HasCapability to feature-detect.
	Capabilities []string `json:"capabilities,omitempty"`
	UUID         string   `json:"uuid"`
	SessionID    string   `json:"session_id"`
}

// HasCapability reports whether the CLI advertised capability c.
func (m *InitMessage) HasCapability(c string) bool {
	return slices.Contains(m.Capabilities, c)
}

// InitMCPServer is one MCP server on an InitMessage.
type InitMCPServer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Source is where the server definition came from (sdk, plugin or a
	// config scope), when reported.
	Source string `json:"source,omitempty"`
}

// InitPlugin is one loaded plugin on an InitMessage.
type InitPlugin = PluginInfo

// InitPluginError is one plugin load failure on an InitMessage.
type InitPluginError struct {
	// Plugin is "name@marketplace", or a positional tag such as "inline[0]".
	Plugin string `json:"plugin"`
	// Type is a failure category from an open set, e.g. "path-not-found".
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
}

// CompactBoundaryMessage (subtype "compact_boundary") marks where the
// conversation was compacted.
type CompactBoundaryMessage struct {
	SystemMessage
	CompactMetadata CompactMetadata `json:"compact_metadata"`
	UUID            string          `json:"uuid"`
	SessionID       string          `json:"session_id"`
}

// CompactMetadata describes one compaction.
type CompactMetadata struct {
	// Trigger is "manual" or "auto".
	Trigger    string `json:"trigger"`
	PreTokens  int    `json:"pre_tokens"`
	PostTokens int    `json:"post_tokens,omitzero"`
	DurationMS int    `json:"duration_ms,omitzero"`
	// PreservedSegment and PreservedMessages describe the messages kept
	// verbatim; both are nil when everything was summarized.
	PreservedSegment  *PreservedSegment  `json:"preserved_segment,omitzero"`
	PreservedMessages *PreservedMessages `json:"preserved_messages,omitzero"`
}

// PreservedSegment is the relink info of a partial compaction.
type PreservedSegment struct {
	HeadUUID   string `json:"head_uuid"`
	AnchorUUID string `json:"anchor_uuid"`
	TailUUID   string `json:"tail_uuid"`
}

// PreservedMessages lists the kept messages of a partial compaction in order;
// it supersedes PreservedSegment.
type PreservedMessages struct {
	AnchorUUID string   `json:"anchor_uuid"`
	UUIDs      []string `json:"uuids"`
}

// StatusMessage (subtype "status") reports a session status change.
type StatusMessage struct {
	SystemMessage
	// Status is StatusCompacting, StatusRequesting, or empty for none.
	Status         string         `json:"status"`
	PermissionMode PermissionMode `json:"permissionMode,omitempty"`
	// CompactResult is "success" or "failed" after a compaction.
	CompactResult string `json:"compact_result,omitempty"`
	CompactError  string `json:"compact_error,omitempty"`
	UUID          string `json:"uuid"`
	SessionID     string `json:"session_id"`
}

// APIRetryMessage (subtype "api_retry") reports a retryable API failure that
// will be retried after RetryDelayMS.
type APIRetryMessage struct {
	SystemMessage
	Attempt      int `json:"attempt"`
	MaxRetries   int `json:"max_retries"`
	RetryDelayMS int `json:"retry_delay_ms"`
	// ErrorStatus is the HTTP status; nil for connection errors.
	ErrorStatus *int                  `json:"error_status"`
	Error       AssistantMessageError `json:"error"`
	// NoResponse is set when the API sent no response headers in time.
	NoResponse *APIRetryNoResponse `json:"no_response,omitzero"`
	UUID       string              `json:"uuid"`
	SessionID  string              `json:"session_id"`
}

// APIRetryNoResponse details a retry caused by missing response headers.
type APIRetryNoResponse struct {
	WaitedMS    int `json:"waited_ms"`
	RetryWaitMS int `json:"retry_wait_ms"`
}

// ControlRequestProgressMessage (subtype "control_request_progress") reports
// progress of a long-running control request, correlated by RequestID.
type ControlRequestProgressMessage struct {
	SystemMessage
	RequestID string `json:"request_id"`
	// Status is "started" or "api_retry"; the retry counters are set only
	// for the latter.
	Status       string `json:"status"`
	Attempt      int    `json:"attempt,omitzero"`
	MaxRetries   int    `json:"max_retries,omitzero"`
	RetryDelayMS int    `json:"retry_delay_ms,omitzero"`
	ErrorStatus  *int   `json:"error_status,omitzero"`
	UUID         string `json:"uuid"`
	SessionID    string `json:"session_id"`
}

// ModelRefusalFallbackMessage (subtype "model_refusal_fallback") reports that
// the model refused and the turn was retried on FallbackModel.
type ModelRefusalFallbackMessage struct {
	SystemMessage
	// Trigger is "refusal".
	Trigger string `json:"trigger"`
	// Direction is "retry" (current CLIs), "revert" or "sticky".
	Direction string `json:"direction"`
	// Scope is "session" (the session model was swapped) or "local" (only a
	// subagent or side question fell back); empty means session.
	Scope         string `json:"scope,omitempty"`
	OriginalModel string `json:"original_model"`
	FallbackModel string `json:"fallback_model"`
	RequestID     string `json:"request_id"`
	// APIRefusalCategory and APIRefusalExplanation come from the refused
	// response; the explanation is display-only.
	APIRefusalCategory    string `json:"api_refusal_category,omitempty"`
	APIRefusalExplanation string `json:"api_refusal_explanation,omitempty"`
	// RetractedMessageUUIDs lists messages to evict from the transcript.
	RetractedMessageUUIDs []string `json:"retracted_message_uuids,omitempty"`
	// RefusedUserMessageUUID is the user message the refused request was
	// for, when it can be edited and retried.
	RefusedUserMessageUUID string `json:"refused_user_message_uuid,omitempty"`
	Content                string `json:"content"`
	UUID                   string `json:"uuid"`
	SessionID              string `json:"session_id"`
}

// ModelRefusalNoFallbackMessage (subtype "model_refusal_no_fallback") reports
// a refusal that was not retried.
type ModelRefusalNoFallbackMessage struct {
	SystemMessage
	OriginalModel          string `json:"original_model"`
	RequestID              string `json:"request_id"`
	APIRefusalCategory     string `json:"api_refusal_category,omitempty"`
	APIRefusalExplanation  string `json:"api_refusal_explanation,omitempty"`
	RefusedUserMessageUUID string `json:"refused_user_message_uuid,omitempty"`
	Content                string `json:"content"`
	UUID                   string `json:"uuid"`
	SessionID              string `json:"session_id"`
}

// LocalCommandOutputMessage (subtype "local_command_output") carries the
// output of a local slash command such as /usage.
type LocalCommandOutputMessage struct {
	SystemMessage
	Content   string `json:"content"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

// PluginInstallMessage (subtype "plugin_install") reports headless plugin
// installation progress.
type PluginInstallMessage struct {
	SystemMessage
	// Status is started, installed, failed or completed.
	Status    string `json:"status"`
	Name      string `json:"name,omitempty"`
	Error     string `json:"error,omitempty"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

// BackgroundTasksChangedMessage (subtype "background_tasks_changed") carries
// the full set of live background tasks. Replace your set with Tasks rather
// than merging.
type BackgroundTasksChangedMessage struct {
	SystemMessage
	Tasks     []BackgroundTask `json:"tasks"`
	UUID      string           `json:"uuid"`
	SessionID string           `json:"session_id"`
}

// BackgroundTask is one live background task.
type BackgroundTask struct {
	TaskID      string `json:"task_id"`
	TaskType    string `json:"task_type"`
	Description string `json:"description"`
	// Ambient marks a task that is not activity.
	Ambient bool `json:"ambient,omitzero"`
}

// ThinkingTokensMessage (subtype "thinking_tokens") is a live estimate of
// thinking tokens, for progress display only.
type ThinkingTokensMessage struct {
	SystemMessage
	EstimatedTokens      int    `json:"estimated_tokens"`
	EstimatedTokensDelta int    `json:"estimated_tokens_delta"`
	UserMessageUUID      string `json:"user_message_uuid,omitempty"`
	UUID                 string `json:"uuid"`
	SessionID            string `json:"session_id"`
}

// SessionStateChangedMessage (subtype "session_state_changed") reports the
// session's run state.
type SessionStateChangedMessage struct {
	SystemMessage
	// State is one of the SessionState* constants.
	State     SessionState `json:"state"`
	UUID      string       `json:"uuid"`
	SessionID string       `json:"session_id"`
}

// WorkerShuttingDownMessage (subtype "worker_shutting_down") reports a
// graceful remote worker teardown.
type WorkerShuttingDownMessage struct {
	SystemMessage
	Reason    string `json:"reason"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

// CommandsChangedMessage (subtype "commands_changed") carries the full slash
// command list after it changed mid-session. Replace your cached list.
type CommandsChangedMessage struct {
	SystemMessage
	Commands  []SlashCommand `json:"commands"`
	UUID      string         `json:"uuid"`
	SessionID string         `json:"session_id"`
}

// NotificationMessage (subtype "notification") is a text notification.
type NotificationMessage struct {
	SystemMessage
	Key  string `json:"key"`
	Text string `json:"text"`
	// Priority is one of the NotificationPriority* constants.
	Priority  string `json:"priority"`
	Color     string `json:"color,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitzero"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

// FilesPersistedMessage (subtype "files_persisted") reports files uploaded
// for the session. TypeScript calls it SDKFilesPersistedEvent.
type FilesPersistedMessage struct {
	SystemMessage
	Files       []PersistedFile `json:"files"`
	Failed      []FailedFile    `json:"failed"`
	ProcessedAt string          `json:"processed_at"`
	UUID        string          `json:"uuid"`
	SessionID   string          `json:"session_id"`
}

// PersistedFile is one uploaded file.
type PersistedFile struct {
	Filename string `json:"filename"`
	FileID   string `json:"file_id"`
}

// FailedFile is one file that failed to upload.
type FailedFile struct {
	Filename string `json:"filename"`
	Error    string `json:"error"`
}

// MemoryRecallMessage (subtype "memory_recall") reports memories surfaced into
// the turn.
type MemoryRecallMessage struct {
	SystemMessage
	// Mode is "select" or "synthesize".
	Mode      string           `json:"mode"`
	Memories  []RecalledMemory `json:"memories"`
	UUID      string           `json:"uuid"`
	SessionID string           `json:"session_id"`
}

// RecalledMemory is one surfaced memory.
type RecalledMemory struct {
	// Path is a file path, a synthesis sentinel, or an https URL.
	Path string `json:"path"`
	// Scope is personal, team or organization.
	Scope string `json:"scope"`
	// Content is the memory body, when sent inline.
	Content string `json:"content,omitempty"`
}

// ElicitationCompleteMessage (subtype "elicitation_complete") reports that an
// MCP server confirmed a URL-mode elicitation.
type ElicitationCompleteMessage struct {
	SystemMessage
	MCPServerName string `json:"mcp_server_name"`
	ElicitationID string `json:"elicitation_id"`
	UUID          string `json:"uuid"`
	SessionID     string `json:"session_id"`
}

// PermissionDeniedMessage (subtype "permission_denied") reports a tool call
// that was denied without a prompt. ResultMessage.PermissionDenials is the
// authoritative record.
type PermissionDeniedMessage struct {
	SystemMessage
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
	// AgentID is set when the call originated inside a subagent.
	AgentID string `json:"agent_id,omitempty"`
	// DecisionReasonType is e.g. classifier, asyncAgent, mode or rule.
	DecisionReasonType string `json:"decision_reason_type,omitempty"`
	DecisionReason     string `json:"decision_reason,omitempty"`
	// Message is the rejection text returned to the model.
	Message   string `json:"message"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

// InformationalMessage (subtype "informational") is a plain text banner.
type InformationalMessage struct {
	SystemMessage
	Content string `json:"content"`
	// Level is one of the Informational* constants.
	Level     string `json:"level"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	// PreventContinuation reports that execution stops after this message.
	PreventContinuation bool   `json:"prevent_continuation,omitzero"`
	UUID                string `json:"uuid"`
	SessionID           string `json:"session_id"`
}

// MCPResourceLink is an MCP resource_link returned by a tool.
type MCPResourceLink struct {
	URI         string         `json:"uri"`
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	MimeType    string         `json:"mimeType,omitempty"`
	Size        *int64         `json:"size,omitzero"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// ---------------------------------------------------------------------------
// Tasks, hooks and mirroring
// ---------------------------------------------------------------------------

// TaskUsage reports usage statistics on task progress and notification
// messages.
type TaskUsage struct {
	TotalTokens int `json:"total_tokens"`
	ToolUses    int `json:"tool_uses"`
	DurationMS  int `json:"duration_ms"`
}

// TerminalTaskStatuses lists the task statuses that mean the task has finished.
// It spans both lifecycle vocabularies: task_notification reports "stopped"
// while task_updated reports the raw "killed". The SDK checks statuses against
// a private copy, so modifying it affects only the caller.
var TerminalTaskStatuses = map[string]bool{
	"completed": true,
	"failed":    true,
	"stopped":   true,
	"killed":    true,
}

// terminalTaskStatuses is the private copy of TerminalTaskStatuses.
var terminalTaskStatuses = maps.Clone(TerminalTaskStatuses)

// isTerminalTaskStatus reports whether status means the task has finished.
func isTerminalTaskStatus(status string) bool { return terminalTaskStatuses[status] }

// TaskStartedMessage is emitted when a task starts. It embeds SystemMessage, so
// a type switch on *SystemMessage does not match it — switch on the concrete
// type or use Data for a uniform view.
type TaskStartedMessage struct {
	SystemMessage
	TaskID      string `json:"task_id"`
	Description string `json:"description"`
	UUID        string `json:"uuid"`
	SessionID   string `json:"session_id"`
	ToolUseID   string `json:"tool_use_id,omitempty"`
	TaskType    string `json:"task_type,omitempty"`
	// SubagentType is the subagent type of an Agent tool task.
	SubagentType string `json:"subagent_type,omitempty"`
	// IsBackgrounded reports whether the task started in the background.
	IsBackgrounded bool `json:"is_backgrounded,omitzero"`
	// SpawnDepth is the nesting depth of a subagent task (1 = top level).
	SpawnDepth int `json:"spawn_depth,omitzero"`
	// WorkflowName is the workflow script name ("local_workflow" tasks).
	WorkflowName string `json:"workflow_name,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	// SkipTranscript marks a housekeeping task to hide from the transcript.
	SkipTranscript bool `json:"skip_transcript,omitzero"`
	// Ambient marks a task that is not activity; exclude it from activity
	// indicators.
	Ambient bool `json:"ambient,omitzero"`
}

// TaskProgressMessage is emitted while a task is in progress.
type TaskProgressMessage struct {
	SystemMessage
	TaskID       string    `json:"task_id"`
	Description  string    `json:"description"`
	Usage        TaskUsage `json:"usage"`
	UUID         string    `json:"uuid"`
	SessionID    string    `json:"session_id"`
	ToolUseID    string    `json:"tool_use_id,omitempty"`
	LastToolName string    `json:"last_tool_name,omitempty"`
	// SubagentType is the subagent type of an Agent tool task.
	SubagentType string `json:"subagent_type,omitempty"`
	// Summary is a one-line status for the task's row, when available.
	Summary string `json:"summary,omitempty"`
}

// TaskNotificationMessage is emitted when a task completes, fails or is
// stopped. Not every terminal task emits one: a background task may report
// completion only through a TaskUpdatedMessage with a terminal patch status.
type TaskNotificationMessage struct {
	SystemMessage
	TaskID     string     `json:"task_id"`
	Status     string     `json:"status"`
	OutputFile string     `json:"output_file"`
	Summary    string     `json:"summary"`
	UUID       string     `json:"uuid"`
	SessionID  string     `json:"session_id"`
	ToolUseID  string     `json:"tool_use_id,omitempty"`
	Usage      *TaskUsage `json:"usage,omitzero"`
	// Reason is set when the task did not end normally, e.g.
	// "worker_restart".
	Reason string `json:"reason,omitempty"`
	// ResourceLinks are the resource_link blocks of a backgrounded MCP task's
	// final result.
	ResourceLinks  []MCPResourceLink `json:"resource_links,omitempty"`
	SkipTranscript bool              `json:"skip_transcript,omitzero"`
	Ambient        bool              `json:"ambient,omitzero"`
}

// TaskUpdatedMessage is emitted when a background task's state changes. Patch
// carries the changed fields; Data["patch"] holds them verbatim.
type TaskUpdatedMessage struct {
	SystemMessage
	TaskID    string    `json:"task_id"`
	Patch     TaskPatch `json:"patch"`
	Status    string    `json:"-"`
	SessionID string    `json:"session_id"`
	UUID      string    `json:"uuid"`
}

// TaskPatch is the typed view of a task_updated patch. A field the patch does
// not carry keeps its zero value; consult Data["patch"] to tell "absent" from
// "zero".
type TaskPatch struct {
	// Status is pending, running, completed, failed, killed or paused.
	Status        string `json:"status,omitempty"`
	Description   string `json:"description,omitempty"`
	EndTime       int64  `json:"end_time,omitzero"`
	TotalPausedMS int64  `json:"total_paused_ms,omitzero"`
	Error         string `json:"error,omitempty"`
	// IsBackgrounded is set when the task moved to (or from) the background.
	IsBackgrounded *bool `json:"is_backgrounded,omitzero"`
}

// MirrorErrorMessage reports that a batch of transcript entries could not be
// mirrored to Options.SessionStore: SessionStore.Append failed on every retry,
// or a single attempt timed out. The SDK synthesizes it; the CLI never sends
// it. It is not fatal: the local transcript is already durable and the session
// continues, but the store may be missing the batch (a timed-out Append may
// still land). Subtype is "mirror_error" and Data carries the raw payload.
type MirrorErrorMessage struct {
	SystemMessage
	// Key identifies the transcript whose batch was dropped. It may be nil.
	Key *SessionKey `json:"key"`
	// Error describes the last failure.
	Error string `json:"error"`
}

// HookEventMessage is a hook lifecycle event (subtype hook_started,
// hook_progress or hook_response), emitted when Options.IncludeHookEvents is
// set.
type HookEventMessage struct {
	SystemMessage
	// HookEventName is the hook event, e.g. "PreToolUse". Older CLIs send it
	// as hook_event_name or only in hook_name.
	HookEventName string `json:"hook_event"`
	SessionID     string `json:"session_id"`
	UUID          string `json:"uuid"`
	// HookID identifies one hook execution across its started, progress and
	// response messages.
	HookID string `json:"hook_id"`
	// HookName names the hook, e.g. "PreToolUse:Bash".
	HookName string `json:"hook_name"`
	// Stdout, Stderr and Output carry the hook's output so far
	// (hook_progress) or in full (hook_response).
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Output string `json:"output,omitempty"`
	// ExitCode is the hook process's exit status (hook_response), when
	// reported.
	ExitCode *int `json:"exit_code,omitzero"`
	// Outcome is success, error or cancelled (hook_response).
	Outcome HookOutcome `json:"outcome,omitempty"`
}
