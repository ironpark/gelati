package claude

import (
	"context"
	"encoding/json"
)

// ---------------------------------------------------------------------------
// Content blocks
// ---------------------------------------------------------------------------

// ContentBlock is one block inside a message's content array. The set of
// implementations is closed: TextBlock, ThinkingBlock, RedactedThinkingBlock,
// ToolUseBlock, ToolResultBlock, ServerToolUseBlock, ServerToolResultBlock,
// MCPToolUseBlock, MCPToolResultBlock, MCPToolListingBlock,
// ContainerUploadBlock, CompactionBlock, FallbackBlock, ImageBlock,
// DocumentBlock and UnknownBlock. A block kind this SDK version does not model
// arrives as an UnknownBlock carrying the raw payload.
//
// A block's JSON encoding is its wire object without the "type" key, which
// BlockType reports.
type ContentBlock interface {
	isContentBlock()
	// BlockType reports the wire discriminator of the block.
	BlockType() string
}

// TextBlock is a plain text content block.
type TextBlock struct {
	Text string `json:"text"`
	// Citations lists the sources the text cites, when the API attached any.
	Citations []TextCitation `json:"citations,omitempty"`
}

func (*TextBlock) isContentBlock()   {}
func (*TextBlock) BlockType() string { return "text" }

// ThinkingBlock is an extended-thinking content block.
type ThinkingBlock struct {
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

func (*ThinkingBlock) isContentBlock()   {}
func (*ThinkingBlock) BlockType() string { return "thinking" }

// ToolUseBlock records a tool invocation requested by the model.
type ToolUseBlock struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
	// Caller identifies who invoked the tool (direct, or a server tool such as
	// code execution), when the API reports it.
	Caller map[string]any `json:"caller,omitempty"`
	// ToolsetName names the toolset the tool belongs to, when it has one.
	ToolsetName string `json:"toolset_name,omitempty"`
}

func (*ToolUseBlock) isContentBlock()   {}
func (*ToolUseBlock) BlockType() string { return "tool_use" }

// ToolResultBlock carries the result of a tool invocation. The CLI sends the
// content either as a plain string or as a list of nested content dicts, so
// exactly one of ContentText and ContentList is set (both may be nil/empty when
// the CLI omitted the field).
type ToolResultBlock struct {
	ToolUseID   string           `json:"tool_use_id"`
	ContentText *string          `json:"-"`
	ContentList []map[string]any `json:"-"`
	IsError     *bool            `json:"is_error,omitempty"`
}

func (*ToolResultBlock) isContentBlock()   {}
func (*ToolResultBlock) BlockType() string { return "tool_result" }

// MarshalJSON writes the wire shape, with "content" holding whichever of
// ContentText and ContentList is set.
func (b ToolResultBlock) MarshalJSON() ([]byte, error) {
	type alias ToolResultBlock
	return marshalWithContent(alias(b), contentValue(b.ContentText, b.ContentList))
}

// UnmarshalJSON reads the wire shape produced by MarshalJSON.
func (b *ToolResultBlock) UnmarshalJSON(data []byte) error {
	type alias ToolResultBlock
	var a alias
	err := json.Unmarshal(data, &a)
	if fatalDecodeErr(err) {
		return err
	}
	*b = ToolResultBlock(a)
	var c struct {
		Content any `json:"content"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	b.ContentText, b.ContentList = splitContent(c.Content)
	return err
}

// ServerToolName enumerates the server-side tools the API may run on the
// model's behalf. Newer CLI versions may report names not listed here.
type ServerToolName = string

// Known server-side tool names.
const (
	ServerToolAdvisor                 ServerToolName = "advisor"
	ServerToolWebSearch               ServerToolName = "web_search"
	ServerToolWebFetch                ServerToolName = "web_fetch"
	ServerToolCodeExecution           ServerToolName = "code_execution"
	ServerToolBashCodeExecution       ServerToolName = "bash_code_execution"
	ServerToolTextEditorCodeExecution ServerToolName = "text_editor_code_execution"
	ServerToolSearchToolRegex         ServerToolName = "tool_search_tool_regex"
	ServerToolSearchToolBM25          ServerToolName = "tool_search_tool_bm25"
)

// ServerToolUseBlock is a server-side tool invocation. The caller never needs
// to return a result for one.
type ServerToolUseBlock struct {
	ID    string         `json:"id"`
	Name  ServerToolName `json:"name"`
	Input map[string]any `json:"input"`
	// Caller identifies who invoked the tool, when the API reports it.
	Caller map[string]any `json:"caller,omitempty"`
}

func (*ServerToolUseBlock) isContentBlock()   {}
func (*ServerToolUseBlock) BlockType() string { return "server_tool_use" }

// Wire types of the server-side tool result blocks, all decoded as
// ServerToolResultBlock.
const (
	BlockAdvisorToolResult                 = "advisor_tool_result"
	BlockWebSearchToolResult               = "web_search_tool_result"
	BlockWebFetchToolResult                = "web_fetch_tool_result"
	BlockCodeExecutionToolResult           = "code_execution_tool_result"
	BlockBashCodeExecutionToolResult       = "bash_code_execution_tool_result"
	BlockTextEditorCodeExecutionToolResult = "text_editor_code_execution_tool_result"
	BlockToolSearchToolResult              = "tool_search_tool_result"
)

// ServerToolResultBlock is the result of a server-side tool call: one of the
// Block*ToolResult wire types, which share this shape. Content is passed
// through from the API verbatim; it is an object for every kind except a
// successful web search, whose result list arrives in ContentList instead.
type ServerToolResultBlock struct {
	// Type is the wire block type, e.g. BlockWebSearchToolResult. Empty
	// means BlockAdvisorToolResult.
	Type        string           `json:"type,omitempty"`
	ToolUseID   string           `json:"tool_use_id"`
	Content     map[string]any   `json:"-"`
	ContentList []map[string]any `json:"-"`
	// Caller identifies who invoked the tool, when the API reports it.
	Caller map[string]any `json:"caller,omitempty"`
}

func (*ServerToolResultBlock) isContentBlock() {}

// BlockType reports Type, defaulting to BlockAdvisorToolResult.
func (b *ServerToolResultBlock) BlockType() string {
	if b.Type == "" {
		return BlockAdvisorToolResult
	}
	return b.Type
}

// MarshalJSON writes the wire shape, with "content" holding whichever of
// Content and ContentList is set.
func (b ServerToolResultBlock) MarshalJSON() ([]byte, error) {
	type alias ServerToolResultBlock
	var content any
	switch {
	case b.Content != nil:
		content = b.Content
	case b.ContentList != nil:
		content = b.ContentList
	}
	return marshalWithContent(alias(b), content)
}

// UnmarshalJSON reads the wire shape produced by MarshalJSON.
func (b *ServerToolResultBlock) UnmarshalJSON(data []byte) error {
	type alias ServerToolResultBlock
	var a alias
	err := json.Unmarshal(data, &a)
	if fatalDecodeErr(err) {
		return err
	}
	*b = ServerToolResultBlock(a)
	var c struct {
		Content any `json:"content"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	b.Content, b.ContentList = splitObjectContent(c.Content)
	return err
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// Message is one item of the stream produced by Query or Client. The set of
// implementations is closed: UserMessage, AssistantMessage, SystemMessage (and
// its specializations, such as InitMessage, TaskStartedMessage or
// HookEventMessage), ResultMessage, StreamEvent, RateLimitEvent,
// ConversationResetMessage, ToolProgressMessage, ToolUseSummaryMessage,
// AuthStatusMessage, PromptSuggestionMessage and ActiveGoalMessage.
//
// Parsing is lenient, like the TypeScript SDK: a field the CLI leaves out is
// left at its zero value instead of failing the stream. Unknown system subtypes
// arrive as a plain *SystemMessage; unknown top-level types are skipped.
type Message interface {
	isMessage()
}

// MessageOriginKind enumerates the known values of MessageOrigin.Kind. Newer
// CLI versions may emit kinds not listed here; treat anything unrecognized as
// "not human".
type MessageOriginKind = string

// Known message origin kinds.
const (
	OriginHuman            MessageOriginKind = "human"
	OriginChannel          MessageOriginKind = "channel"
	OriginPeer             MessageOriginKind = "peer"
	OriginTaskNotification MessageOriginKind = "task-notification"
	OriginCoordinator      MessageOriginKind = "coordinator"
	OriginUnclassified     MessageOriginKind = "unclassified"
	OriginObserver         MessageOriginKind = "observer"
	OriginAutoContinuation MessageOriginKind = "auto-continuation"
	OriginObserverActivity MessageOriginKind = "observer-activity"
)

// MessageOrigin describes the provenance of a user-role message and, on a
// ResultMessage, of the message that triggered the turn. Only Kind is always
// present; which of the remaining fields are set depends on Kind, and Extra
// carries any keys this SDK version does not model.
type MessageOrigin struct {
	// Kind classifies the sender. It falls back to OriginUnclassified when the
	// CLI sends a kind this SDK cannot read as a string.
	Kind MessageOriginKind `json:"kind"`
	// Server names the MCP server that delivered the message ("channel").
	Server string `json:"server,omitempty"`
	// From is the sender's address, such as "agent://name" ("channel", "peer").
	From string `json:"from,omitempty"`
	// Name is the sender's display name, when it has one.
	Name string `json:"name,omitempty"`
	// FromSession is the sender's session ID ("peer", "coordinator").
	FromSession string `json:"fromSession,omitempty"`
	// SenderTaskID is the task that sent the message ("peer",
	// "task-notification").
	SenderTaskID string `json:"senderTaskId,omitempty"`
	// Body is the original message text, when the delivered content wraps it
	// ("channel", "peer").
	Body string `json:"body,omitempty"`
	// VerifiedPeerPID is the OS process ID of the peer, as verified by the CLI
	// ("peer"). It is 0 when the CLI reported no verified PID.
	VerifiedPeerPID int `json:"verifiedPeerPid,omitempty"`
	// Subkind narrows Kind, for kinds that classify further.
	Subkind string `json:"subkind,omitempty"`
	// Extra holds origin keys this SDK version does not model, so a newer CLI
	// loses nothing. It is merged back in on marshal; modeled fields win.
	Extra map[string]any `json:"-"`
}

// originModeledKeys are the origin keys MessageOrigin has a field for.
// Anything else goes to Extra.
var originModeledKeys = map[string]bool{
	"kind": true, "server": true, "from": true, "name": true,
	"fromSession": true, "senderTaskId": true, "body": true,
	"verifiedPeerPid": true, "subkind": true,
}

// MarshalJSON writes the modeled fields with any Extra keys merged alongside.
func (o MessageOrigin) MarshalJSON() ([]byte, error) {
	type alias MessageOrigin
	b, err := json.Marshal(alias(o))
	if err != nil || len(o.Extra) == 0 {
		return b, err
	}
	var merged map[string]any
	if err := json.Unmarshal(b, &merged); err != nil {
		return nil, err
	}
	for k, v := range o.Extra {
		if _, taken := merged[k]; !taken {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}

// UnmarshalJSON reads the modeled fields and collects the rest into Extra.
func (o *MessageOrigin) UnmarshalJSON(b []byte) error {
	type alias MessageOrigin
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*o = MessageOrigin(a)
	for k, v := range raw {
		if !originModeledKeys[k] {
			o.putExtra(k, v)
		}
	}
	return nil
}

// putExtra records an unmodeled origin key, allocating Extra on first use.
func (o *MessageOrigin) putExtra(key string, value any) {
	if o.Extra == nil {
		o.Extra = make(map[string]any)
	}
	o.Extra[key] = value
}

// UserMessage is a user-role message. The CLI delivers its content either as a
// plain string or as a block array; ContentText is set for the former and
// Content for the latter.
type UserMessage struct {
	ContentText     *string
	Content         []ContentBlock
	UUID            string
	ParentToolUseID string
	// ToolUseResult is the tool's structured output (any JSON value; usually
	// an object) on a message carrying a tool_result block.
	ToolUseResult any
	Origin        *MessageOrigin
	SessionID     string
	// IsReplay marks the CLI's echo of a user message it was sent
	// (Options.ReplayUserMessages).
	IsReplay bool
	// IsSynthetic marks a message the CLI generated rather than a user typed.
	IsSynthetic bool
	// Priority is the queue priority the message was sent with (now, next or
	// later), when set.
	Priority MessagePriority
	// ShouldQuery is false when the message was appended without starting a
	// turn; nil means the CLI did not say (a turn was started).
	ShouldQuery *bool
	// Timestamp is the ISO 8601 creation time on the originating process,
	// for display only.
	Timestamp string
	// ClientComposed reports the message was sent with client_composed.
	ClientComposed bool
	// FileAttachments are the attachments of a replayed message, verbatim.
	FileAttachments []any
	// PastedContent and InlinePastes echo the pastes sent with the message.
	PastedContent []any
	InlinePastes  []string
	// SubagentType and TaskDescription identify the subagent that produced
	// the message, when one did.
	SubagentType    string
	TaskDescription string
}

func (*UserMessage) isMessage() {}

// AssistantMessageError enumerates the known error kinds on an assistant
// message.
type AssistantMessageError = string

// Known assistant message error kinds.
const (
	AssistantErrorAuthenticationFailed AssistantMessageError = "authentication_failed"
	AssistantErrorOAuthOrgNotAllowed   AssistantMessageError = "oauth_org_not_allowed"
	AssistantErrorAccountOnHold        AssistantMessageError = "account_on_hold"
	AssistantErrorVerificationRequired AssistantMessageError = "verification_required"
	AssistantErrorBilling              AssistantMessageError = "billing_error"
	AssistantErrorRateLimit            AssistantMessageError = "rate_limit"
	AssistantErrorOverloaded           AssistantMessageError = "overloaded"
	AssistantErrorInvalidRequest       AssistantMessageError = "invalid_request"
	AssistantErrorModelNotFound        AssistantMessageError = "model_not_found"
	AssistantErrorServer               AssistantMessageError = "server_error"
	AssistantErrorUnknown              AssistantMessageError = "unknown"
	AssistantErrorMaxOutputTokens      AssistantMessageError = "max_output_tokens"
	AssistantErrorCloudCredential      AssistantMessageError = "cloud_credential_error"
)

// AssistantMessage is an assistant-role message with its content blocks.
// While a response streams, the CLI emits one assistant message per completed
// block, so consecutive messages can share MessageID; on those StopReason is
// empty and Usage is not final.
type AssistantMessage struct {
	Content         []ContentBlock
	Model           string
	ParentToolUseID string
	Error           AssistantMessageError
	Usage           map[string]any
	MessageID       string
	StopReason      string
	SessionID       string
	UUID            string
	// StopSequence is the custom stop sequence that ended the response.
	StopSequence string
	// StopDetails explains a "refusal" stop, when the API reported it.
	StopDetails *StopDetails
	// Container and ContextManagement are the API message's container and
	// context_management objects, verbatim.
	Container         map[string]any
	ContextManagement map[string]any
	// RequestID is the API request ID.
	RequestID string
	// UserMessageUUID is the client UUID (UserInput.UUID) of the user message
	// this turn answers. It is stamped only on the turn's first assistant
	// message (and after a queued message is folded in); UserMessageUUIDs
	// lists every user message the turn has consumed so far.
	UserMessageUUID  string
	UserMessageUUIDs []string
	// ResumeReason is set on the automatic re-run of an interrupted turn.
	ResumeReason string
	// ResumedFromIncompleteThinking marks a turn that continued a truncated
	// thinking block.
	ResumedFromIncompleteThinking bool
	// Supersedes lists the UUIDs of earlier messages this one replaces
	// (refusal fallback); evict them on arrival.
	Supersedes []string
	// Aborted marks a message truncated by an interrupt before the stream
	// completed.
	Aborted bool
	// SubagentType and TaskDescription identify the subagent that produced
	// the message, when one did.
	SubagentType    string
	TaskDescription string
	// Timestamp is the ISO 8601 time the block finished on the originating
	// process, for display only.
	Timestamp string
	// ContextUsage is the structured twin of a /context report.
	ContextUsage *ContextUsageReport
	// UsageReport is the structured twin of a /usage report, verbatim.
	// Experimental: the shape may change.
	UsageReport map[string]any
}

func (*AssistantMessage) isMessage() {}

// SystemMessage is a metadata message. Data holds the full raw payload,
// including fields not modeled by the specialized subtypes.
type SystemMessage struct {
	Subtype string         `json:"subtype"`
	Data    map[string]any `json:"data"`
}

func (*SystemMessage) isMessage() {}

// TaskUsage reports usage statistics on task progress and notification
// messages.
type TaskUsage struct {
	TotalTokens int `json:"total_tokens"`
	ToolUses    int `json:"tool_uses"`
	DurationMS  int `json:"duration_ms"`
}

// TerminalTaskStatuses lists the task statuses that mean the task has finished.
// It spans both lifecycle vocabularies: task_notification reports "stopped"
// while task_updated reports the raw "killed".
var TerminalTaskStatuses = map[string]bool{
	"completed": true,
	"failed":    true,
	"stopped":   true,
	"killed":    true,
}

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
	IsBackgrounded bool `json:"is_backgrounded,omitempty"`
	// SpawnDepth is the nesting depth of a subagent task (1 = top level).
	SpawnDepth int `json:"spawn_depth,omitempty"`
	// WorkflowName is the workflow script name ("local_workflow" tasks).
	WorkflowName string `json:"workflow_name,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	// SkipTranscript marks a housekeeping task to hide from the transcript.
	SkipTranscript bool `json:"skip_transcript,omitempty"`
	// Ambient marks a task that is not activity; exclude it from activity
	// indicators.
	Ambient bool `json:"ambient,omitempty"`
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
	Usage      *TaskUsage `json:"usage,omitempty"`
	// Reason is set when the task did not end normally, e.g.
	// "worker_restart".
	Reason string `json:"reason,omitempty"`
	// ResourceLinks are the resource_link blocks of a backgrounded MCP task's
	// final result.
	ResourceLinks  []MCPResourceLink `json:"resource_links,omitempty"`
	SkipTranscript bool              `json:"skip_transcript,omitempty"`
	Ambient        bool              `json:"ambient,omitempty"`
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
	EndTime       int64  `json:"end_time,omitempty"`
	TotalPausedMS int64  `json:"total_paused_ms,omitempty"`
	Error         string `json:"error,omitempty"`
	// IsBackgrounded is set when the task moved to (or from) the background.
	IsBackgrounded *bool `json:"is_backgrounded,omitempty"`
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
	Key *SessionKey
	// Error describes the last failure.
	Error string
}

// HookEventMessage is a hook lifecycle event (subtype hook_started,
// hook_progress or hook_response), emitted when Options.IncludeHookEvents is
// set.
type HookEventMessage struct {
	SystemMessage
	// HookEventName is the hook event, e.g. "PreToolUse".
	HookEventName string
	SessionID     string
	UUID          string
	// HookID identifies one hook execution across its started, progress and
	// response messages.
	HookID string
	// HookName names the hook, e.g. "PreToolUse:Bash".
	HookName string
	// Stdout, Stderr and Output carry the hook's output so far
	// (hook_progress) or in full (hook_response).
	Stdout string
	Stderr string
	Output string
	// ExitCode is the hook process's exit status (hook_response), when
	// reported.
	ExitCode *int
	// Outcome is success, error or cancelled (hook_response).
	Outcome HookOutcome
}

// DeferredToolUse is a tool call deferred by a PreToolUse hook that returned
// the "defer" permission decision.
type DeferredToolUse struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ModelUsage is the per-model token and cost breakdown reported on a result
// message. Field names follow the CLI's camelCase wire format.
type ModelUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	// ThinkingTokens is the share of OutputTokens spent thinking, when the
	// CLI recorded it.
	ThinkingTokens           int     `json:"thinkingTokens,omitempty"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	WebSearchRequests        int     `json:"webSearchRequests"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int     `json:"contextWindow"`
	MaxOutputTokens          int     `json:"maxOutputTokens"`
	CanonicalModel           string  `json:"canonicalModel,omitempty"`
	Provider                 string  `json:"provider,omitempty"`
	// CostBasis is the price table CostUSD used: list, managed or unknown.
	// Empty means list.
	CostBasis string `json:"costBasis,omitempty"`
}

// PermissionDenial is one tool call denied during the turn.
type PermissionDenial struct {
	ToolName  string         `json:"tool_name"`
	ToolUseID string         `json:"tool_use_id"`
	ToolInput map[string]any `json:"tool_input"`
}

// ResultMessage terminates a turn and reports its cost, usage and outcome.
type ResultMessage struct {
	// Subtype is one of the ResultSubtype* constants.
	Subtype           string
	DurationMS        int
	DurationAPIMS     int
	IsError           bool
	NumTurns          int
	SessionID         string
	StopReason        string
	TotalCostUSD      *float64
	Usage             map[string]any
	Result            string
	StructuredOutput  json.RawMessage
	ModelUsage        map[string]ModelUsage
	PermissionDenials []PermissionDenial
	DeferredToolUse   *DeferredToolUse
	Errors            []string
	APIErrorStatus    *int
	UUID              string
	// TerminalReason is one of the TerminalReason* constants, when reported.
	TerminalReason TerminalReason
	Origin         *MessageOrigin
	// QueuedTurnCount is the number of user sends still queued, when
	// reported.
	QueuedTurnCount *int
	// UserMessageUUID is the client UUID of the user message that triggered
	// the turn; UserMessageUUIDs lists every user message it consumed.
	UserMessageUUID  string
	UserMessageUUIDs []string
	// ResumeReason is set on the automatic re-run of an interrupted turn.
	ResumeReason string
	// ResultIndex is the delivery sequence number of this result within the
	// run, when reported; a gap means a result was lost.
	ResultIndex            *int
	FastModeState          FastModeState
	FastModeDisabledReason FastModeDisabledReason
	// StartupFailureReason is set on the error result written for a known
	// startup failure.
	StartupFailureReason StartupFailureReason
	// LocalCommand names the local slash command the turn ran, if any.
	LocalCommand string
	// Timing carries the latency measurements of a success result, when the
	// CLI reported any.
	Timing *ResultTiming
	// Data is the raw result payload, retained so ResultError can report it.
	Data map[string]any
}

func (*ResultMessage) isMessage() {}

// StreamEvent carries a partial-message update from the streaming API. Event is
// the raw Anthropic API stream event.
type StreamEvent struct {
	UUID            string
	SessionID       string
	Event           map[string]any
	ParentToolUseID string
	// TTFTMS is the time to first token in milliseconds, when reported.
	TTFTMS *float64
	// UserMessageUUID and UserMessageUUIDs bind the stream to the sends it
	// answers; they are stamped on the turn's first non-ping event only.
	UserMessageUUID  string
	UserMessageUUIDs []string
	// ResumeReason is set on the automatic re-run of an interrupted turn.
	ResumeReason string
}

func (*StreamEvent) isMessage() {}

// RateLimitStatus enumerates the rate limit states the CLI reports.
type RateLimitStatus = string

// Known rate limit statuses.
const (
	RateLimitAllowed        RateLimitStatus = "allowed"
	RateLimitAllowedWarning RateLimitStatus = "allowed_warning"
	RateLimitRejected       RateLimitStatus = "rejected"
)

// RateLimitInfo describes the rate limit state at the moment it changed.
type RateLimitInfo struct {
	Status   RateLimitStatus
	ResetsAt *int64
	// RateLimitType is one of the RateLimitType* constants.
	RateLimitType   string
	Utilization     *float64
	OverageStatus   RateLimitStatus
	OverageResetsAt *int64
	// OverageDisabledReason is one of the OverageDisabled* constants.
	OverageDisabledReason string
	IsUsingOverage        bool
	OverageInUse          bool
	SurpassedThreshold    *float64
	// LimitScope says which spend limit blocked the request when it is not
	// the member's own: service, channel or group_pool.
	LimitScope string
	// ErrorCode is "credits_required" when credits must be bought.
	ErrorCode                       string
	CanUserPurchaseCredits          bool
	HasChargeableSavedPaymentMethod bool
	// Raw is the full dict from the CLI, including unmodeled fields.
	Raw map[string]any
}

// RateLimitEvent is emitted when the rate limit status transitions.
type RateLimitEvent struct {
	RateLimitInfo RateLimitInfo
	UUID          string
	SessionID     string
}

func (*RateLimitEvent) isMessage() {}

// ConversationResetMessage is emitted when the session's conversation is
// replaced without ending the connection, e.g. after /clear. Running totals on
// subsequent result messages restart from zero.
type ConversationResetMessage struct {
	NewConversationID string
	UUID              string
	SessionID         string
	// Trigger is clear, plan_mode_exit, fresh_session or onboarding; empty
	// or unrecognized means an unspecified reset.
	Trigger ConversationResetTrigger
	// UserMessageUUID is the user message whose /clear ran (Trigger clear).
	UserMessageUUID string
	// Timestamp is the ISO 8601 reset time, for display only.
	Timestamp string
}

func (*ConversationResetMessage) isMessage() {}

// ---------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------

// PermissionMode selects how the session handles permission prompts.
type PermissionMode = string

// Supported permission modes.
const (
	PermissionModeDefault           PermissionMode = "default"
	PermissionModeAcceptEdits       PermissionMode = "acceptEdits"
	PermissionModePlan              PermissionMode = "plan"
	PermissionModeBypassPermissions PermissionMode = "bypassPermissions"
	PermissionModeDontAsk           PermissionMode = "dontAsk"
	PermissionModeAuto              PermissionMode = "auto"
)

// PermissionUpdateDestination selects where a permission update is persisted.
type PermissionUpdateDestination = string

// Supported permission update destinations.
const (
	DestinationUserSettings    PermissionUpdateDestination = "userSettings"
	DestinationProjectSettings PermissionUpdateDestination = "projectSettings"
	DestinationLocalSettings   PermissionUpdateDestination = "localSettings"
	DestinationSession         PermissionUpdateDestination = "session"
	// DestinationCLIArg is the in-memory layer of rules passed on the
	// command line (--allowedTools and friends).
	DestinationCLIArg PermissionUpdateDestination = "cliArg"
)

// PermissionBehavior is the effect of a permission rule.
type PermissionBehavior = string

// Supported permission behaviors.
const (
	BehaviorAllow PermissionBehavior = "allow"
	BehaviorDeny  PermissionBehavior = "deny"
	BehaviorAsk   PermissionBehavior = "ask"
)

// PermissionRuleValue names a tool and, optionally, the rule content that
// narrows the match (e.g. a Bash command prefix).
type PermissionRuleValue struct {
	ToolName    string  `json:"toolName"`
	RuleContent *string `json:"ruleContent,omitempty"`
}

// Permission update kinds.
const (
	PermissionUpdateAddRules          = "addRules"
	PermissionUpdateReplaceRules      = "replaceRules"
	PermissionUpdateRemoveRules       = "removeRules"
	PermissionUpdateSetMode           = "setMode"
	PermissionUpdateAddDirectories    = "addDirectories"
	PermissionUpdateRemoveDirectories = "removeDirectories"
)

// PermissionUpdate is one change to the session's permission configuration.
// Only the fields relevant to Type are put on the wire.
type PermissionUpdate struct {
	Type        string
	Rules       []PermissionRuleValue
	Behavior    PermissionBehavior
	Mode        PermissionMode
	Directories []string
	Destination PermissionUpdateDestination
}

// MarshalJSON emits the control-protocol shape, matching the TypeScript SDK.
func (u PermissionUpdate) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": u.Type}
	if u.Destination != "" {
		out["destination"] = u.Destination
	}
	switch u.Type {
	case PermissionUpdateAddRules, PermissionUpdateReplaceRules, PermissionUpdateRemoveRules:
		if u.Rules != nil {
			rules := make([]map[string]any, 0, len(u.Rules))
			for _, r := range u.Rules {
				rule := map[string]any{"toolName": r.ToolName}
				// Like the TypeScript SDK, a rule without content omits the
				// key rather than sending null.
				if r.RuleContent != nil {
					rule["ruleContent"] = *r.RuleContent
				}
				rules = append(rules, rule)
			}
			out["rules"] = rules
		}
		if u.Behavior != "" {
			out["behavior"] = u.Behavior
		}
	case PermissionUpdateSetMode:
		if u.Mode != "" {
			out["mode"] = u.Mode
		}
	case PermissionUpdateAddDirectories, PermissionUpdateRemoveDirectories:
		if u.Directories != nil {
			out["directories"] = u.Directories
		}
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads the control-protocol shape produced by MarshalJSON.
func (u *PermissionUpdate) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type        string                `json:"type"`
		Rules       []PermissionRuleValue `json:"rules"`
		Behavior    string                `json:"behavior"`
		Mode        string                `json:"mode"`
		Directories []string              `json:"directories"`
		Destination string                `json:"destination"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*u = PermissionUpdate{
		Type:        raw.Type,
		Rules:       raw.Rules,
		Behavior:    raw.Behavior,
		Mode:        raw.Mode,
		Directories: raw.Directories,
		Destination: raw.Destination,
	}
	return nil
}

// ToolPermissionContext is the context handed to a CanUseTool callback.
type ToolPermissionContext struct {
	// Suggestions holds permission updates the CLI proposes.
	Suggestions []PermissionUpdate
	// ToolUseID identifies this specific tool call. Always non-empty.
	ToolUseID string
	// AgentID is set when the call originates inside a sub-agent.
	AgentID string
	// BlockedPath is the file path that triggered the request, if any.
	BlockedPath string
	// DecisionReason explains why the request was triggered.
	DecisionReason string
	// Title is the full permission prompt sentence, when provided.
	Title string
	// DisplayName is a short noun phrase for the action.
	DisplayName string
	// Description is a human-readable subtitle for the permission UI.
	Description string
	// MCPServer is set for mcp__* tools: the server serving the tool and
	// where its definition came from. Source "sdk" means one of the
	// in-process servers this host registered; any other value is a server
	// from configuration whose name is untrusted text. Key trust decisions
	// on Source, never on the name or the tool-name prefix. nil for
	// non-MCP tools and on CLIs that predate the field.
	MCPServer *MCPServerProvenance
	// DefaultToNo asks the host to open the prompt on its decline option and
	// offer no one-key approve shortcut.
	DefaultToNo bool
	// SuppressAlwaysAllowRule asks the host not to offer a persistent
	// "don't ask again" choice: the rule it would write grants more than
	// this request's own action.
	SuppressAlwaysAllowRule bool
	// MatchedAskRule is set when a user-configured ask rule forced this
	// prompt. Host-side auto-approval should treat such requests as
	// rule-forced: the user asked for a human prompt.
	MatchedAskRule *MatchedAskRule
	// RequiresUserInteraction reports whether the CLI considers a human
	// answer necessary; nil when the CLI did not say.
	RequiresUserInteraction *bool
	// RequestID is the control request's request_id. A reply sent out of
	// band (see ErrRespondedOutOfBand) must echo it.
	RequestID string
	// Raw is the complete request payload, including fields not modeled
	// above (decision_reason_type, classifier_approvable, ...).
	Raw map[string]any
}

// MCPServerProvenance identifies the MCP server behind a tool and where its
// definition came from.
type MCPServerProvenance struct {
	// Name is the server name as registered or configured.
	Name string `json:"name"`
	// Source is "sdk" for an in-process server this host registered, or a
	// configuration source (plugin, user, project, local, dynamic, managed,
	// enterprise, claudeai, agent, ...). The set is open: treat unknown
	// values as an unrecognized configured source, never as "sdk".
	Source string `json:"source"`
}

// MatchedAskRule is the user-configured ask rule that forced a permission
// prompt.
type MatchedAskRule struct {
	Source   string `json:"source"`
	ToolName string `json:"toolName"`
	// RuleContent narrows the rule, when it has content.
	RuleContent *string `json:"ruleContent,omitempty"`
}

// PermissionDecisionClassification classifies a permission decision for
// telemetry. Hosts that prompt users should report what actually happened;
// when unset the CLI infers it conservatively (temporary for allow, reject
// for deny).
type PermissionDecisionClassification = string

// Permission decision classifications.
const (
	// DecisionUserTemporary is an allow-once decision.
	DecisionUserTemporary PermissionDecisionClassification = "user_temporary"
	// DecisionUserPermanent is an always-allow decision, both the click and
	// later cache hits.
	DecisionUserPermanent PermissionDecisionClassification = "user_permanent"
	// DecisionUserReject is a denial.
	DecisionUserReject PermissionDecisionClassification = "user_reject"
)

// PermissionResult is the answer to a permission request: either
// *PermissionResultAllow or *PermissionResultDeny.
type PermissionResult interface {
	isPermissionResult()
}

// PermissionResultAllow allows the tool call, optionally rewriting its input or
// adding permission rules.
type PermissionResultAllow struct {
	UpdatedInput       map[string]any
	UpdatedPermissions []PermissionUpdate
	// DecisionClassification reports how the user decided; optional.
	DecisionClassification PermissionDecisionClassification
}

func (*PermissionResultAllow) isPermissionResult() {}

// PermissionResultDeny denies the tool call. Interrupt additionally aborts the
// turn.
type PermissionResultDeny struct {
	Message   string
	Interrupt bool
	// DecisionClassification reports how the user decided; optional.
	DecisionClassification PermissionDecisionClassification
}

func (*PermissionResultDeny) isPermissionResult() {}

// CanUseTool decides whether a tool call that would otherwise prompt the user
// may proceed. Returning an error surfaces as a control-protocol error to the
// CLI. Returning an error wrapping ErrRespondedOutOfBand sends no reply at
// all, for hosts that already answered the request some other way; the tool
// stays blocked if nobody did, since permission prompts have no deadline.
type CanUseTool func(ctx context.Context, toolName string, input map[string]any, permCtx ToolPermissionContext) (PermissionResult, error)

// ---------------------------------------------------------------------------
// Hooks
// ---------------------------------------------------------------------------

// HookEvent names a point in the agent lifecycle a hook can observe.
type HookEvent = string

// Supported hook events.
const (
	HookPreToolUse         HookEvent = "PreToolUse"
	HookPostToolUse        HookEvent = "PostToolUse"
	HookPostToolUseFailure HookEvent = "PostToolUseFailure"
	HookUserPromptSubmit   HookEvent = "UserPromptSubmit"
	HookStop               HookEvent = "Stop"
	HookSubagentStop       HookEvent = "SubagentStop"
	HookPreCompact         HookEvent = "PreCompact"
	HookNotification       HookEvent = "Notification"
	HookSubagentStart      HookEvent = "SubagentStart"
	HookPermissionRequest  HookEvent = "PermissionRequest"
)

// Hook events the TypeScript SDK adds to the set above.
const (
	HookPostToolBatch       HookEvent = "PostToolBatch"
	HookUserPromptExpansion HookEvent = "UserPromptExpansion"
	HookSessionStart        HookEvent = "SessionStart"
	HookSessionEnd          HookEvent = "SessionEnd"
	HookStopFailure         HookEvent = "StopFailure"
	HookPostCompact         HookEvent = "PostCompact"
	HookPreModelSwitch      HookEvent = "PreModelSwitch"
	HookPostModelSwitch     HookEvent = "PostModelSwitch"
	HookPermissionDenied    HookEvent = "PermissionDenied"
	HookSetup               HookEvent = "Setup"
	HookTeammateIdle        HookEvent = "TeammateIdle"
	HookTaskCreated         HookEvent = "TaskCreated"
	HookTaskCompleted       HookEvent = "TaskCompleted"
	HookElicitation         HookEvent = "Elicitation"
	HookElicitationResult   HookEvent = "ElicitationResult"
	HookConfigChange        HookEvent = "ConfigChange"
	HookWorktreeCreate      HookEvent = "WorktreeCreate"
	HookWorktreeRemove      HookEvent = "WorktreeRemove"
	HookInstructionsLoaded  HookEvent = "InstructionsLoaded"
	HookCwdChanged          HookEvent = "CwdChanged"
	HookFileChanged         HookEvent = "FileChanged"
	HookDirectoryAdded      HookEvent = "DirectoryAdded"
	HookMessageDisplay      HookEvent = "MessageDisplay"
)

// HookEvents lists every hook event this SDK version knows, in the order the
// TypeScript SDK's HOOK_EVENTS uses. Options.Hooks accepts any event name, so
// events newer than this list still reach the CLI.
var HookEvents = []HookEvent{
	HookPreToolUse, HookPostToolUse, HookPostToolUseFailure, HookPostToolBatch,
	HookNotification, HookUserPromptSubmit, HookUserPromptExpansion,
	HookSessionStart, HookSessionEnd, HookStop, HookStopFailure,
	HookSubagentStart, HookSubagentStop, HookPreCompact, HookPostCompact,
	HookPreModelSwitch, HookPostModelSwitch, HookPermissionRequest,
	HookPermissionDenied, HookSetup, HookTeammateIdle, HookTaskCreated,
	HookTaskCompleted, HookElicitation, HookElicitationResult,
	HookConfigChange, HookWorktreeCreate, HookWorktreeRemove,
	HookInstructionsLoaded, HookCwdChanged, HookFileChanged,
	HookDirectoryAdded, HookMessageDisplay,
}

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

// HookContext carries per-invocation context for a hook callback. The abort
// signal of the TypeScript SDK is the callback's ctx, which is cancelled when
// the CLI cancels the request.
type HookContext struct {
	// Raw is the hook input exactly as the CLI sent it, including fields
	// the typed inputs do not model. Typed hooks (see TypedHook) read newer
	// CLI fields from here.
	Raw map[string]any
}

// HookOutput is what a hook callback returns. A zero value means "no opinion":
// nothing is sent back beyond an empty object.
//
// Field names on the wire follow the CLI's documented JSON schema; Continue and
// Async are emitted as "continue" and "async".
type HookOutput struct {
	// Continue reports whether Claude should proceed. nil leaves it unset
	// (the CLI defaults to true).
	Continue *bool
	// SuppressOutput hides stdout from transcript mode.
	SuppressOutput *bool
	// StopReason is shown to the user when Continue is false.
	StopReason string
	// Decision is HookDecisionBlock to block the action or
	// HookDecisionApprove to approve it.
	Decision HookDecision
	// SystemMessage is a warning displayed to the user.
	SystemMessage string
	// Reason is feedback for Claude about the decision.
	Reason string
	// TerminalSequence is a terminal escape sequence the CLI writes to its
	// terminal: an OSC 0/1/2 title, OSC 9/99/777 notification or BEL.
	TerminalSequence string
	// HookSpecificOutput carries event-specific fields, e.g.
	// {"hookEventName": "PreToolUse", "permissionDecision": "allow"}.
	HookSpecificOutput map[string]any
	// Specific is the typed form of HookSpecificOutput (for example
	// *PreToolUseHookSpecificOutput). Its fields, including hookEventName,
	// are merged over HookSpecificOutput on the wire.
	Specific HookSpecific
	// Async defers hook execution; the CLI continues without waiting.
	Async bool
	// AsyncTimeout is the timeout in milliseconds for an async hook.
	AsyncTimeout *int
	// Extra holds output keys this SDK version does not model. The
	// TypeScript SDK forwards a callback's output verbatim, so Extra is
	// merged in on marshal; modeled fields win.
	Extra map[string]any
}

// hookOutputModeledKeys are the output keys HookOutput has a field for.
var hookOutputModeledKeys = map[string]bool{
	"continue": true, "suppressOutput": true, "stopReason": true,
	"decision": true, "systemMessage": true, "reason": true,
	"terminalSequence": true, "hookSpecificOutput": true, "async": true,
	"asyncTimeout": true,
}

// MarshalJSON emits the CLI wire format, translating Continue and Async to
// their reserved-word key names.
func (h HookOutput) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(h.Extra)+4)
	for k, v := range h.Extra {
		out[k] = v
	}
	if h.Async {
		out["async"] = true
		if h.AsyncTimeout != nil {
			out["asyncTimeout"] = *h.AsyncTimeout
		}
		return json.Marshal(out)
	}
	if h.Continue != nil {
		out["continue"] = *h.Continue
	}
	if h.SuppressOutput != nil {
		out["suppressOutput"] = *h.SuppressOutput
	}
	if h.StopReason != "" {
		out["stopReason"] = h.StopReason
	}
	if h.Decision != "" {
		out["decision"] = h.Decision
	}
	if h.SystemMessage != "" {
		out["systemMessage"] = h.SystemMessage
	}
	if h.Reason != "" {
		out["reason"] = h.Reason
	}
	if h.TerminalSequence != "" {
		out["terminalSequence"] = h.TerminalSequence
	}
	specific, err := mergeHookSpecificOutput(h.HookSpecificOutput, h.Specific)
	if err != nil {
		return nil, err
	}
	if specific != nil {
		out["hookSpecificOutput"] = specific
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads the CLI wire format produced by MarshalJSON. The
// event-specific output lands in HookSpecificOutput; Specific stays nil.
func (h *HookOutput) UnmarshalJSON(data []byte) error {
	var raw struct {
		Continue           *bool          `json:"continue"`
		SuppressOutput     *bool          `json:"suppressOutput"`
		StopReason         string         `json:"stopReason"`
		Decision           string         `json:"decision"`
		SystemMessage      string         `json:"systemMessage"`
		Reason             string         `json:"reason"`
		TerminalSequence   string         `json:"terminalSequence"`
		HookSpecificOutput map[string]any `json:"hookSpecificOutput"`
		Async              bool           `json:"async"`
		AsyncTimeout       *int           `json:"asyncTimeout"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var all map[string]any
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	*h = HookOutput{
		Continue:           raw.Continue,
		SuppressOutput:     raw.SuppressOutput,
		StopReason:         raw.StopReason,
		Decision:           raw.Decision,
		SystemMessage:      raw.SystemMessage,
		Reason:             raw.Reason,
		TerminalSequence:   raw.TerminalSequence,
		HookSpecificOutput: raw.HookSpecificOutput,
		Async:              raw.Async,
		AsyncTimeout:       raw.AsyncTimeout,
	}
	for k, v := range all {
		if !hookOutputModeledKeys[k] {
			if h.Extra == nil {
				h.Extra = map[string]any{}
			}
			h.Extra[k] = v
		}
	}
	return nil
}

// HookCallback runs for a matching hook event. input is the raw hook input dict
// keyed by the CLI's schema (hook_event_name, tool_name, ...); toolUseID is
// empty when the event is not tool-scoped. DecodeHookInput turns input into a
// typed per-event struct, and TypedHook adapts a typed function into a
// HookCallback.
type HookCallback func(ctx context.Context, input map[string]any, toolUseID string, hookCtx HookContext) (HookOutput, error)

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
