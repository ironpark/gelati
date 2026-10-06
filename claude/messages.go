package claude

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"reflect"

	"github.com/ironpark/gelati/internal/jsonx"
)

// Message is one item of the stream produced by Query or Client. The set of
// implementations is closed: UserMessage, AssistantMessage, SystemMessage (and
// its specializations, such as InitMessage, TaskStartedMessage or
// HookEventMessage), ResultMessage, StreamEvent, RateLimitEvent,
// ConversationResetMessage, ToolProgressMessage, ToolUseSummaryMessage,
// AuthStatusMessage, PromptSuggestionMessage, ActiveGoalMessage and
// UnknownMessage.
//
// Parsing is lenient, like the TypeScript SDK: a field the CLI leaves out is
// left at its zero value instead of failing the stream. Unknown system subtypes
// arrive as a plain *SystemMessage, unknown top-level types as *UnknownMessage.
type Message interface {
	isMessage()
}

// ---------------------------------------------------------------------------
// Message origin
// ---------------------------------------------------------------------------

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
	VerifiedPeerPID int `json:"verifiedPeerPid,omitzero"`
	// Subkind narrows Kind, for kinds that classify further.
	Subkind string `json:"subkind,omitempty"`
	// Extra holds origin keys this SDK version does not model, so a newer CLI
	// loses nothing. It is merged back in on marshal; modeled fields win.
	Extra map[string]any `json:"-"`
}

// originFields are the origin keys MessageOrigin has a field for. Anything
// else goes to Extra.
var originFields = jsonMemberNames(reflect.TypeFor[MessageOrigin]())

// MarshalJSON writes the modeled fields with any Extra keys merged alongside.
func (o MessageOrigin) MarshalJSON() ([]byte, error) {
	type alias MessageOrigin
	return marshalWithExtra(alias(o), o.Extra)
}

// UnmarshalJSON reads the modeled fields and collects the rest into Extra,
// leniently as parseOrigin does. A value that is not an object leaves o
// unchanged.
func (o *MessageOrigin) UnmarshalJSON(b []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw, lenient); err != nil {
		if fatalDecodeErr(err) {
			return err
		}
		return nil
	}
	if raw != nil {
		*o = originFromMap(raw)
	}
	return nil
}

// parseOrigin returns data["origin"] when it is a non-empty object, or nil.
func parseOrigin(data map[string]any) *MessageOrigin {
	raw, ok := data["origin"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	o := originFromMap(raw)
	return &o
}

// originFromMap reads a decoded origin object, passing keys this SDK version
// does not model through to Extra. A missing or non-string kind falls back to
// OriginUnclassified rather than discarding the rest of the origin; the raw
// kind, if any, is kept in Extra.
func originFromMap(raw map[string]any) MessageOrigin {
	o := MessageOrigin{
		Kind:         jsonx.Str(raw["kind"]),
		Server:       jsonx.Str(raw["server"]),
		From:         jsonx.Str(raw["from"]),
		Name:         jsonx.Str(raw["name"]),
		FromSession:  jsonx.Str(raw["fromSession"]),
		SenderTaskID: jsonx.Str(raw["senderTaskId"]),
		Body:         jsonx.Str(raw["body"]),
		Subkind:      jsonx.Str(raw["subkind"]),
	}
	// A fractional PID is truncated rather than dropped.
	o.VerifiedPeerPID, _ = toInt(raw["verifiedPeerPid"])
	for k, v := range raw {
		if !originFields[k] {
			o.putExtra(k, v)
		}
	}
	if kind, ok := raw["kind"].(string); !ok || kind == "" {
		o.Kind = OriginUnclassified
		if v := raw["kind"]; v != nil {
			o.putExtra("kind", v)
		}
	}
	return o
}

// putExtra records an unmodeled origin key, allocating Extra on first use.
func (o *MessageOrigin) putExtra(key string, value any) {
	if o.Extra == nil {
		o.Extra = make(map[string]any)
	}
	o.Extra[key] = value
}

// ---------------------------------------------------------------------------
// user / assistant
// ---------------------------------------------------------------------------

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

// AssistantMessage is an assistant-role message with its content blocks.
// While a response streams, the CLI emits one assistant message per completed
// block, so consecutive messages can share MessageID; on those StopReason is
// empty and Usage is not final.
type AssistantMessage struct {
	Content         []ContentBlock
	Model           string
	ParentToolUseID string
	Error           AssistantMessageError
	// Usage is the API response's token usage; not final while the
	// response streams (see above).
	Usage      Usage
	MessageID  string
	StopReason string
	SessionID  string
	UUID       string
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

// StopDetails explains a "refusal" stop reason on an AssistantMessage.
type StopDetails struct {
	// Type is "refusal".
	Type string `json:"type,omitempty"`
	// Category is the refusal policy category (cyber, bio, frontier_llm,
	// reasoning_extraction, general_harms); empty when none applies.
	Category string `json:"category,omitempty"`
	// Explanation is display-only prose; never parse it.
	Explanation string `json:"explanation,omitempty"`
}

// ---------------------------------------------------------------------------
// result
// ---------------------------------------------------------------------------

// ResultMessage terminates a turn and reports its cost, usage and outcome.
type ResultMessage struct {
	// Subtype is one of the ResultSubtype* constants.
	Subtype           string                `json:"subtype"`
	DurationMS        int                   `json:"duration_ms"`
	DurationAPIMS     int                   `json:"duration_api_ms"`
	IsError           bool                  `json:"is_error"`
	NumTurns          int                   `json:"num_turns"`
	SessionID         string                `json:"session_id"`
	StopReason        string                `json:"stop_reason,omitempty"`
	TotalCostUSD      *float64              `json:"total_cost_usd,omitzero"`
	Usage             Usage                 `json:"usage,omitzero"`
	Result            string                `json:"result,omitempty"`
	StructuredOutput  jsontext.Value        `json:"structured_output,omitempty"`
	ModelUsage        map[string]ModelUsage `json:"modelUsage,omitempty"`
	PermissionDenials []PermissionDenial    `json:"permission_denials,omitempty"`
	DeferredToolUse   *DeferredToolUse      `json:"deferred_tool_use,omitzero"`
	Errors            []string              `json:"errors,omitempty"`
	APIErrorStatus    *int                  `json:"api_error_status,omitzero"`
	UUID              string                `json:"uuid"`
	// TerminalReason is one of the TerminalReason* constants, when reported.
	TerminalReason TerminalReason `json:"terminal_reason,omitempty"`
	Origin         *MessageOrigin `json:"origin,omitzero"`
	// QueuedTurnCount is the number of user sends still queued, when
	// reported.
	QueuedTurnCount *int `json:"queued_turn_count,omitzero"`
	// UserMessageUUID is the client UUID of the user message that triggered
	// the turn; UserMessageUUIDs lists every user message it consumed.
	UserMessageUUID  string   `json:"user_message_uuid,omitempty"`
	UserMessageUUIDs []string `json:"user_message_uuids,omitempty"`
	// ResumeReason is set on the automatic re-run of an interrupted turn.
	ResumeReason string `json:"resume_reason,omitempty"`
	// ResultIndex is the delivery sequence number of this result within the
	// run, when reported; a gap means a result was lost.
	ResultIndex            *int                   `json:"result_index,omitzero"`
	FastModeState          FastModeState          `json:"fast_mode_state,omitempty"`
	FastModeDisabledReason FastModeDisabledReason `json:"fast_mode_disabled_reason,omitempty"`
	// StartupFailureReason is set on the error result written for a known
	// startup failure.
	StartupFailureReason StartupFailureReason `json:"startup_failure_reason,omitempty"`
	// LocalCommand names the local slash command the turn ran, if any.
	LocalCommand string `json:"local_command,omitempty"`
	// Timing carries the latency measurements of a success result, when the
	// CLI reported any. On the wire they are members of the result itself.
	Timing *ResultTiming `json:"-"`
	// Data is the raw result payload, retained so ResultError can report it.
	Data map[string]any `json:"-"`
}

func (*ResultMessage) isMessage() {}

// Text returns the final response text of the turn: the Result field.
func (m *ResultMessage) Text() string { return m.Result }

// Usage is the token usage the Anthropic API reports, for one response
// (AssistantMessage) or summed over a run (ResultMessage). Members the CLI
// sends as null read as zero.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	// ServerToolUse counts server-side tool requests, when reported.
	ServerToolUse *ServerToolUse `json:"server_tool_use,omitzero"`
	// ServiceTier is the tier the request was served on, such as
	// "standard".
	ServiceTier string `json:"service_tier,omitempty"`
	// Extra holds usage keys this SDK version does not model (cache_creation,
	// inference_geo, speed, iterations, ...), so a newer CLI loses nothing.
	// It is merged back in on marshal; modeled fields win.
	Extra map[string]any `json:"-"`
}

// DecodeStructuredOutput decodes the turn's structured output (see
// Options.OutputFormat) into v. It fails when the turn produced none.
func (m *ResultMessage) DecodeStructuredOutput(v any) error {
	if m == nil || len(m.StructuredOutput) == 0 {
		return errors.New("claude: the turn produced no structured output")
	}
	return jsonx.Unmarshal(m.StructuredOutput, v)
}

// ServerToolUse counts the server-side tool requests of a Usage.
type ServerToolUse struct {
	WebSearchRequests int `json:"web_search_requests"`
	WebFetchRequests  int `json:"web_fetch_requests,omitzero"`
}

// usageFields are the usage keys Usage has a field for. Anything else goes to
// Extra.
var usageFields = jsonMemberNames(reflect.TypeFor[Usage]())

// MarshalJSON writes the modeled fields with any Extra keys merged alongside.
func (u Usage) MarshalJSON() ([]byte, error) {
	type alias Usage
	return marshalWithExtra(alias(u), u.Extra)
}

// UnmarshalJSON reads the modeled fields leniently, as the message parser
// does, and collects the rest into Extra. A value that is not an object
// leaves u unchanged.
func (u *Usage) UnmarshalJSON(b []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw, lenient); err != nil {
		if fatalDecodeErr(err) {
			return err
		}
		return nil
	}
	if raw != nil {
		*u = usageFromMap(raw)
	}
	return nil
}

// usageFromMap reads a decoded usage object. Counters of the wrong type read
// as zero and fractional ones are truncated; unmodeled keys go to Extra.
func usageFromMap(raw map[string]any) Usage {
	var u Usage
	u.InputTokens, _ = toInt(raw["input_tokens"])
	u.OutputTokens, _ = toInt(raw["output_tokens"])
	u.CacheCreationInputTokens, _ = toInt(raw["cache_creation_input_tokens"])
	u.CacheReadInputTokens, _ = toInt(raw["cache_read_input_tokens"])
	u.ServiceTier = jsonx.Str(raw["service_tier"])
	if stu, ok := raw["server_tool_use"].(map[string]any); ok {
		u.ServerToolUse = &ServerToolUse{}
		u.ServerToolUse.WebSearchRequests, _ = toInt(stu["web_search_requests"])
		u.ServerToolUse.WebFetchRequests, _ = toInt(stu["web_fetch_requests"])
	}
	for k, v := range raw {
		if !usageFields[k] {
			if u.Extra == nil {
				u.Extra = map[string]any{}
			}
			u.Extra[k] = v
		}
	}
	return u
}

// ModelUsage is the per-model token and cost breakdown reported on a result
// message. Field names follow the CLI's camelCase wire format.
type ModelUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	// ThinkingTokens is the share of OutputTokens spent thinking, when the
	// CLI recorded it.
	ThinkingTokens           int     `json:"thinkingTokens,omitzero"`
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

// DeferredToolUse is a tool call deferred by a PreToolUse hook that returned
// the "defer" permission decision.
type DeferredToolUse struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ResultTiming carries the latency measurements on a success result. All
// durations are in milliseconds; a zero value means "not reported".
type ResultTiming struct {
	TTFTMS                      float64 `json:"ttft_ms,omitzero"`
	TTFTStreamMS                float64 `json:"ttft_stream_ms,omitzero"`
	TimeToRequestMS             float64 `json:"time_to_request_ms,omitzero"`
	RequestSentWallMS           float64 `json:"request_sent_wall_ms,omitzero"`
	FirstContentFrameMS         float64 `json:"first_content_frame_ms,omitzero"`
	FirstStreamPostMS           float64 `json:"first_stream_post_ms,omitzero"`
	FirstStreamPostAckMS        float64 `json:"first_stream_post_ack_ms,omitzero"`
	FirstStreamPostQueueWaitMS  float64 `json:"first_stream_post_queue_wait_ms,omitzero"`
	FirstStreamPostQueuedBehind string  `json:"first_stream_post_queued_behind,omitempty"`
	FirstStreamPostWallMS       float64 `json:"first_stream_post_wall_ms,omitzero"`
	FirstTextPostMS             float64 `json:"first_text_post_ms,omitzero"`
	FirstTextPostWallMS         float64 `json:"first_text_post_wall_ms,omitzero"`
	TimeToRequestFromSpawnMS    float64 `json:"time_to_request_from_spawn_ms,omitzero"`
	WarmSpareClaimed            bool    `json:"warm_spare_claimed,omitzero"`
	TimeOriginMS                float64 `json:"time_origin_ms,omitzero"`
}

// ---------------------------------------------------------------------------
// stream_event / rate_limit_event / conversation_reset
// ---------------------------------------------------------------------------

// StreamEvent carries a partial-message update from the streaming API. Event is
// the raw Anthropic API stream event.
type StreamEvent struct {
	UUID            string         `json:"uuid"`
	SessionID       string         `json:"session_id"`
	Event           map[string]any `json:"event"`
	ParentToolUseID string         `json:"parent_tool_use_id,omitempty"`
	// TTFTMS is the time to first token in milliseconds, when reported.
	TTFTMS *float64 `json:"ttft_ms,omitzero"`
	// UserMessageUUID and UserMessageUUIDs bind the stream to the sends it
	// answers; they are stamped on the turn's first non-ping event only.
	UserMessageUUID  string   `json:"user_message_uuid,omitempty"`
	UserMessageUUIDs []string `json:"user_message_uuids,omitempty"`
	// ResumeReason is set on the automatic re-run of an interrupted turn.
	ResumeReason string `json:"resume_reason,omitempty"`
}

func (*StreamEvent) isMessage() {}

// TextDelta returns the text appended by a content_block_delta event carrying
// a text_delta, the event IncludePartialMessages is usually enabled for.
func (e *StreamEvent) TextDelta() (string, bool) {
	return e.delta("text_delta", "text")
}

// thinkingDelta returns the reasoning appended by a content_block_delta event
// carrying a thinking_delta.
func (e *StreamEvent) thinkingDelta() (string, bool) {
	return e.delta("thinking_delta", "thinking")
}

// delta returns the string member field of the delta of a
// content_block_delta event whose delta type is kind.
func (e *StreamEvent) delta(kind, field string) (string, bool) {
	if e.Event["type"] != "content_block_delta" {
		return "", false
	}
	delta, _ := e.Event["delta"].(map[string]any)
	if delta["type"] != kind {
		return "", false
	}
	text, ok := delta[field].(string)
	return text, ok
}

// messageStartID returns the API message id of a message_start event.
func (e *StreamEvent) messageStartID() (string, bool) {
	if e.Event["type"] != "message_start" {
		return "", false
	}
	msg, _ := e.Event["message"].(map[string]any)
	return jsonx.Str(msg["id"]), true
}

// RateLimitInfo describes the rate limit state at the moment it changed.
type RateLimitInfo struct {
	Status   RateLimitStatus `json:"status"`
	ResetsAt *int64          `json:"resetsAt,omitzero"`
	// RateLimitType is one of the RateLimitType* constants.
	RateLimitType   string          `json:"rateLimitType,omitempty"`
	Utilization     *float64        `json:"utilization,omitzero"`
	OverageStatus   RateLimitStatus `json:"overageStatus,omitempty"`
	OverageResetsAt *int64          `json:"overageResetsAt,omitzero"`
	// OverageDisabledReason is one of the OverageDisabled* constants.
	OverageDisabledReason string   `json:"overageDisabledReason,omitempty"`
	IsUsingOverage        bool     `json:"isUsingOverage,omitzero"`
	OverageInUse          bool     `json:"overageInUse,omitzero"`
	SurpassedThreshold    *float64 `json:"surpassedThreshold,omitzero"`
	// LimitScope says which spend limit blocked the request when it is not
	// the member's own: service, channel or group_pool.
	LimitScope string `json:"limitScope,omitempty"`
	// ErrorCode is "credits_required" when credits must be bought.
	ErrorCode                       string `json:"errorCode,omitempty"`
	CanUserPurchaseCredits          bool   `json:"canUserPurchaseCredits,omitzero"`
	HasChargeableSavedPaymentMethod bool   `json:"hasChargeableSavedPaymentMethod,omitzero"`
	// Raw is the full dict from the CLI, including unmodeled fields.
	Raw map[string]any `json:"-"`
}

// RateLimitEvent is emitted when the rate limit status transitions.
type RateLimitEvent struct {
	RateLimitInfo RateLimitInfo `json:"rate_limit_info"`
	UUID          string        `json:"uuid"`
	SessionID     string        `json:"session_id"`
}

func (*RateLimitEvent) isMessage() {}

// ConversationResetMessage is emitted when the session's conversation is
// replaced without ending the connection, e.g. after /clear. Running totals on
// subsequent result messages restart from zero.
type ConversationResetMessage struct {
	NewConversationID string `json:"new_conversation_id"`
	UUID              string `json:"uuid"`
	SessionID         string `json:"session_id"`
	// Trigger is clear, plan_mode_exit, fresh_session or onboarding; empty
	// or unrecognized means an unspecified reset.
	Trigger ConversationResetTrigger `json:"trigger,omitempty"`
	// UserMessageUUID is the user message whose /clear ran (Trigger clear).
	UserMessageUUID string `json:"user_message_uuid,omitempty"`
	// Timestamp is the ISO 8601 reset time, for display only.
	Timestamp string `json:"timestamp,omitempty"`
}

func (*ConversationResetMessage) isMessage() {}
