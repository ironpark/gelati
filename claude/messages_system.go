package claude

import "slices"

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
type InitPlugin struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
}

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
	PostTokens int    `json:"post_tokens,omitempty"`
	DurationMS int    `json:"duration_ms,omitempty"`
	// PreservedSegment and PreservedMessages describe the messages kept
	// verbatim; both are nil when everything was summarized.
	PreservedSegment  *PreservedSegment  `json:"preserved_segment,omitempty"`
	PreservedMessages *PreservedMessages `json:"preserved_messages,omitempty"`
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
	NoResponse *APIRetryNoResponse `json:"no_response,omitempty"`
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
	Attempt      int    `json:"attempt,omitempty"`
	MaxRetries   int    `json:"max_retries,omitempty"`
	RetryDelayMS int    `json:"retry_delay_ms,omitempty"`
	ErrorStatus  *int   `json:"error_status,omitempty"`
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
	Ambient bool `json:"ambient,omitempty"`
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
	TimeoutMS int    `json:"timeout_ms,omitempty"`
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
	PreventContinuation bool   `json:"prevent_continuation,omitempty"`
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
	Size        *int64         `json:"size,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// systemBase gives decodeSystem access to the embedded SystemMessage.
func (m *SystemMessage) systemBase() *SystemMessage { return m }
