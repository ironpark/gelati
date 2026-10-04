package claude

import (
	"encoding/json"
	"fmt"
)

// ParseMessage turns one raw CLI output frame into a typed Message.
//
// Parsing is lenient, like the TypeScript SDK, which never validates frames: a
// field that is missing or has an unexpected JSON type is left at its zero
// value. It returns (nil, nil) for message types this SDK version does not
// model (including a frame without a "type"), so that a newer CLI cannot break
// an older SDK. A *MessageParseError is returned only when data is not a JSON
// object.
func ParseMessage(data []byte) (Message, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, NewMessageParseError(
			fmt.Sprintf("Invalid message data: %v", err), json.RawMessage(data))
	}
	return parseMessageMap(raw, json.RawMessage(data))
}

// parseMessageMap parses an already-decoded frame. src is the original,
// non-nil payload, from which typed fields are decoded; generic subtrees are
// taken from data instead of being decoded a second time.
func parseMessageMap(data map[string]any, src json.RawMessage) (Message, error) {
	if data == nil {
		return nil, NewMessageParseError("Invalid message data (expected object)", src)
	}
	parse, ok := messageParsers[str(data["type"])]
	if !ok {
		// Forward-compatible: skip unrecognized (or missing) message types.
		return nil, nil
	}
	return parse(data, src), nil
}

// messageParser parses a frame of one type; see parseMessageMap.
type messageParser func(data map[string]any, src []byte) Message

// messageParsers maps a frame's "type" to its parser.
var messageParsers = map[string]messageParser{
	"user":               parseUserMessage,
	"assistant":          parseAssistantMessage,
	"system":             parseSystemMessage,
	"result":             parseResultMessage,
	"stream_event":       parseStreamEvent,
	"rate_limit_event":   parseRateLimitEvent,
	"conversation_reset": decodeTyped[ConversationResetMessage],
	"tool_progress":      decodeTyped[ToolProgressMessage],
	"tool_use_summary":   decodeTyped[ToolUseSummaryMessage],
	"auth_status":        decodeTyped[AuthStatusMessage],
	"prompt_suggestion":  decodeTyped[PromptSuggestionMessage],
	"active_goal":        decodeTyped[ActiveGoalMessage],
}

// decodeTyped decodes a message whose struct tags follow the wire format.
func decodeTyped[T any, P interface {
	*T
	Message
}](_ map[string]any, src []byte) Message {
	var v T
	decodeLenient(src, &v)
	return P(&v)
}

// ---------------------------------------------------------------------------
// user / assistant
// ---------------------------------------------------------------------------

// The user and assistant messages flatten a nested wire shape (the API
// message sits under "message"), so they are read through wire structs
// rather than tags of their own.

type userWire struct {
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	UUID            string   `json:"uuid"`
	ParentToolUseID string   `json:"parent_tool_use_id"`
	SessionID       string   `json:"session_id"`
	IsReplay        bool     `json:"isReplay"`
	IsSynthetic     bool     `json:"isSynthetic"`
	Priority        string   `json:"priority"`
	ShouldQuery     *bool    `json:"shouldQuery"`
	Timestamp       string   `json:"timestamp"`
	ClientComposed  bool     `json:"client_composed"`
	InlinePastes    []string `json:"inline_pastes"`
	SubagentType    string   `json:"subagent_type"`
	TaskDescription string   `json:"task_description"`
}

func parseUserMessage(data map[string]any, src []byte) Message {
	var w userWire
	decodeLenient(src, &w)
	// Generic subtrees are taken from the already-decoded frame instead of
	// being decoded a second time.
	fileAttachments, _ := data["file_attachments"].([]any)
	pastedContent, _ := data["pasted_content"].([]any)
	msg := &UserMessage{
		UUID:            w.UUID,
		ParentToolUseID: w.ParentToolUseID,
		ToolUseResult:   data["tool_use_result"],
		Origin:          parseOrigin(data),
		SessionID:       w.SessionID,
		IsReplay:        w.IsReplay,
		IsSynthetic:     w.IsSynthetic,
		Priority:        w.Priority,
		ShouldQuery:     w.ShouldQuery,
		Timestamp:       w.Timestamp,
		ClientComposed:  w.ClientComposed,
		FileAttachments: fileAttachments,
		PastedContent:   pastedContent,
		InlinePastes:    w.InlinePastes,
		SubagentType:    w.SubagentType,
		TaskDescription: w.TaskDescription,
	}
	msg.ContentText, msg.Content = parseContent(w.Message.Content)
	return msg
}

type assistantWire struct {
	Message struct {
		ID           string          `json:"id"`
		Model        string          `json:"model"`
		Content      json.RawMessage `json:"content"`
		StopReason   string          `json:"stop_reason"`
		StopSequence string          `json:"stop_sequence"`
		StopDetails  *StopDetails    `json:"stop_details"`
	} `json:"message"`
	ParentToolUseID               string              `json:"parent_tool_use_id"`
	Error                         string              `json:"error"`
	SessionID                     string              `json:"session_id"`
	UUID                          string              `json:"uuid"`
	RequestID                     string              `json:"request_id"`
	UserMessageUUID               string              `json:"user_message_uuid"`
	UserMessageUUIDs              []string            `json:"user_message_uuids"`
	ResumeReason                  string              `json:"resume_reason"`
	ResumedFromIncompleteThinking bool                `json:"resumed_from_incomplete_thinking"`
	Supersedes                    []string            `json:"supersedes"`
	Aborted                       bool                `json:"aborted"`
	SubagentType                  string              `json:"subagent_type"`
	TaskDescription               string              `json:"task_description"`
	Timestamp                     string              `json:"timestamp"`
	ContextUsage                  *ContextUsageReport `json:"context_usage"`
}

func parseAssistantMessage(data map[string]any, src []byte) Message {
	var w assistantWire
	decodeLenient(src, &w)
	inner := &w.Message
	// Generic subtrees are taken from the already-decoded frame instead of
	// being decoded a second time.
	innerMap, _ := data["message"].(map[string]any)
	usage, _ := innerMap["usage"].(map[string]any)
	container, _ := innerMap["container"].(map[string]any)
	contextManagement, _ := innerMap["context_management"].(map[string]any)
	usageReport, _ := data["usage_report"].(map[string]any)
	text, blocks := parseContent(inner.Content)
	if text != nil {
		// The API always sends a block list; tolerate a bare string.
		blocks = []ContentBlock{&TextBlock{Text: *text}}
	}
	if blocks == nil {
		blocks = []ContentBlock{}
	}
	return &AssistantMessage{
		Content:                       blocks,
		Model:                         inner.Model,
		ParentToolUseID:               w.ParentToolUseID,
		Error:                         w.Error,
		Usage:                         usage,
		MessageID:                     inner.ID,
		StopReason:                    inner.StopReason,
		SessionID:                     w.SessionID,
		UUID:                          w.UUID,
		StopSequence:                  inner.StopSequence,
		StopDetails:                   inner.StopDetails,
		Container:                     container,
		ContextManagement:             contextManagement,
		RequestID:                     w.RequestID,
		UserMessageUUID:               w.UserMessageUUID,
		UserMessageUUIDs:              w.UserMessageUUIDs,
		ResumeReason:                  w.ResumeReason,
		ResumedFromIncompleteThinking: w.ResumedFromIncompleteThinking,
		Supersedes:                    w.Supersedes,
		Aborted:                       w.Aborted,
		SubagentType:                  w.SubagentType,
		TaskDescription:               w.TaskDescription,
		Timestamp:                     w.Timestamp,
		ContextUsage:                  w.ContextUsage,
		UsageReport:                   usageReport,
	}
}

// ---------------------------------------------------------------------------
// system
// ---------------------------------------------------------------------------

// systemMessage is implemented by the pointer types of every struct that
// embeds SystemMessage.
type systemMessage interface {
	Message
	systemBase() *SystemMessage
}

// systemParser parses a system frame of one subtype, given its base.
type systemParser func(data map[string]any, src []byte, base SystemMessage) Message

// systemParsers maps a system frame's subtype to its parser. A subtype not
// listed arrives as a plain *SystemMessage.
var systemParsers = map[string]systemParser{
	"init":                      decodeSystem[InitMessage],
	"task_started":              decodeSystem[TaskStartedMessage],
	"task_progress":             decodeSystem[TaskProgressMessage],
	"task_notification":         decodeSystem[TaskNotificationMessage],
	"task_updated":              parseTaskUpdated,
	"hook_started":              parseHookEvent,
	"hook_progress":             parseHookEvent,
	"hook_response":             parseHookEvent,
	"mirror_error":              parseMirrorError,
	"compact_boundary":          decodeSystem[CompactBoundaryMessage],
	"status":                    decodeSystem[StatusMessage],
	"api_retry":                 decodeSystem[APIRetryMessage],
	"control_request_progress":  decodeSystem[ControlRequestProgressMessage],
	"model_refusal_fallback":    decodeSystem[ModelRefusalFallbackMessage],
	"model_refusal_no_fallback": decodeSystem[ModelRefusalNoFallbackMessage],
	"local_command_output":      decodeSystem[LocalCommandOutputMessage],
	"plugin_install":            decodeSystem[PluginInstallMessage],
	"background_tasks_changed":  decodeSystem[BackgroundTasksChangedMessage],
	"thinking_tokens":           decodeSystem[ThinkingTokensMessage],
	"session_state_changed":     decodeSystem[SessionStateChangedMessage],
	"worker_shutting_down":      decodeSystem[WorkerShuttingDownMessage],
	"commands_changed":          decodeSystem[CommandsChangedMessage],
	"notification":              decodeSystem[NotificationMessage],
	"files_persisted":           decodeSystem[FilesPersistedMessage],
	"memory_recall":             decodeSystem[MemoryRecallMessage],
	"elicitation_complete":      decodeSystem[ElicitationCompleteMessage],
	"permission_denied":         decodeSystem[PermissionDeniedMessage],
	"informational":             decodeSystem[InformationalMessage],
}

func parseSystemMessage(data map[string]any, src []byte) Message {
	subtype := str(data["subtype"])
	base := SystemMessage{Subtype: subtype, Data: data}
	if parse, ok := systemParsers[subtype]; ok {
		return parse(data, src, base)
	}
	return &base
}

// decodeSystem decodes a typed system message whose struct tags follow the
// wire format, and attaches its base.
func decodeSystem[T any, P interface {
	*T
	systemMessage
}](_ map[string]any, src []byte, base SystemMessage) Message {
	var v T
	p := P(&v)
	decodeLenient(src, p)
	*p.systemBase() = base
	return p
}

func parseTaskUpdated(data map[string]any, src []byte, base SystemMessage) Message {
	m := decodeSystem[TaskUpdatedMessage](data, src, base).(*TaskUpdatedMessage)
	m.Status = m.Patch.Status
	return m
}

func parseHookEvent(data map[string]any, src []byte, base SystemMessage) Message {
	m := decodeSystem[HookEventMessage](data, src, base).(*HookEventMessage)
	if m.HookEventName == "" {
		// Older CLIs send hook_event_name, or the event only in hook_name.
		m.HookEventName = firstString(data, "hook_event_name", "hook_name")
	}
	return m
}

// parseMirrorError parses the frame the engine synthesizes when
// SessionStore.Append fails; the CLI never emits one.
func parseMirrorError(data map[string]any, src []byte, base SystemMessage) Message {
	m := decodeSystem[MirrorErrorMessage](data, src, base).(*MirrorErrorMessage)
	m.Key = parseSessionKey(data["key"])
	return m
}

// parseSessionKey decodes a session key object, returning nil when v is not
// one. Both the Go snake_case shape {"project_key", "session_id", "subpath"}
// and the TypeScript camelCase shape {"projectKey", "sessionId", "subpath"}
// are accepted.
func parseSessionKey(v any) *SessionKey {
	k, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return &SessionKey{
		ProjectKey: firstString(k, "project_key", "projectKey"),
		SessionID:  firstString(k, "session_id", "sessionId"),
		Subpath:    str(k["subpath"]),
	}
}

// ---------------------------------------------------------------------------
// result / stream_event / rate_limit_event
// ---------------------------------------------------------------------------

func parseResultMessage(data map[string]any, src []byte) Message {
	// The timing measurements are members of the result itself. The usage,
	// errors and origin come from the decoded frame map instead.
	var w struct {
		ResultMessage
		ResultTiming
		Usage  skipValue `json:"usage"`
		Errors skipValue `json:"errors"`
		Origin skipValue `json:"origin"`
	}
	decodeLenient(src, &w)
	m := &w.ResultMessage
	m.Usage, _ = data["usage"].(map[string]any)
	m.Errors = normalizeResultErrors(data["errors"])
	m.Origin = parseOrigin(data)
	m.Data = data
	// Counters are read from the map too, so a fractional value is truncated
	// rather than dropped.
	for key, dst := range map[string]*int{
		"duration_ms": &m.DurationMS, "duration_api_ms": &m.DurationAPIMS, "num_turns": &m.NumTurns,
	} {
		if n, ok := toInt(data[key]); ok {
			*dst = n
		}
	}
	if string(m.StructuredOutput) == "null" {
		m.StructuredOutput = nil
	}
	if w.ResultTiming != (ResultTiming{}) {
		m.Timing = &w.ResultTiming
	}
	return m
}

func parseStreamEvent(data map[string]any, src []byte) Message {
	var w struct {
		StreamEvent
		Event skipValue `json:"event"`
	}
	decodeLenient(src, &w)
	// The event, the bulk of the frame, comes from the decoded frame map.
	w.StreamEvent.Event, _ = data["event"].(map[string]any)
	return &w.StreamEvent
}

// int64Ptr returns v as an integer, or nil when it is not a JSON number.
func int64Ptr(v any) *int64 {
	if n, ok := toInt64(v); ok {
		return &n
	}
	return nil
}

func parseRateLimitEvent(data map[string]any, src []byte) Message {
	m := decodeTyped[RateLimitEvent](data, src).(*RateLimitEvent)
	info, _ := data["rate_limit_info"].(map[string]any)
	rli := &m.RateLimitInfo
	rli.Raw = info
	// Timestamps are read from the map so fractional seconds still parse
	// (encoding/json refuses them for an integer field).
	rli.ResetsAt, rli.OverageResetsAt = int64Ptr(info["resetsAt"]), int64Ptr(info["overageResetsAt"])
	return m
}
