package claude

import (
	"bytes"
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

// parseMessageMap parses an already-decoded frame. src is the original payload,
// from which typed fields are decoded; it may be nil, in which case data is
// re-encoded.
func parseMessageMap(data map[string]any, src json.RawMessage) (Message, error) {
	if data == nil {
		return nil, NewMessageParseError("Invalid message data (expected object)", src)
	}
	if src == nil {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, NewMessageParseError(
				fmt.Sprintf("Invalid message data: %v", err), nil)
		}
		src = b
	}

	switch str(data["type"]) {
	case "user":
		return parseUserMessage(data, src), nil
	case "assistant":
		return parseAssistantMessage(data, src), nil
	case "system":
		return parseSystemMessage(data, src), nil
	case "result":
		return parseResultMessage(data, src), nil
	case "stream_event":
		return parseStreamEvent(data, src), nil
	case "rate_limit_event":
		return parseRateLimitEvent(data, src), nil
	case "conversation_reset":
		return parseConversationReset(src), nil
	case "tool_progress":
		return decodeTyped[ToolProgressMessage](src), nil
	case "tool_use_summary":
		return decodeTyped[ToolUseSummaryMessage](src), nil
	case "auth_status":
		return decodeTyped[AuthStatusMessage](src), nil
	case "prompt_suggestion":
		return decodeTyped[PromptSuggestionMessage](src), nil
	case "active_goal":
		return decodeTyped[ActiveGoalMessage](src), nil
	default:
		// Forward-compatible: skip unrecognized (or missing) message types.
		return nil, nil
	}
}

// decodeTyped decodes a message whose struct tags follow the wire format.
func decodeTyped[T any, P interface {
	*T
	Message
}](src []byte) Message {
	var v T
	decodeLenient(src, &v)
	return P(&v)
}

// ---------------------------------------------------------------------------
// user / assistant
// ---------------------------------------------------------------------------

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

// parseContent decodes a message's content: a string, a block array, or (for
// anything else that is not null) its JSON text.
func parseContent(raw json.RawMessage) (*string, []ContentBlock) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	switch raw[0] {
	case '"':
		var s string
		decodeLenient(raw, &s)
		return &s, nil
	case '[':
		var items []json.RawMessage
		decodeLenient(raw, &items)
		return nil, parseBlocks(items)
	}
	s := string(raw)
	return &s, nil
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
// Content blocks
// ---------------------------------------------------------------------------

// parseBlocks decodes a content array. Items that are not objects are
// skipped; objects of an unknown type become UnknownBlock.
func parseBlocks(items []json.RawMessage) []ContentBlock {
	blocks := make([]ContentBlock, 0, len(items))
	for _, item := range items {
		if b := parseBlock(item); b != nil {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// parseBlock decodes one content block, or returns nil when item is not a
// JSON object.
func parseBlock(item json.RawMessage) ContentBlock {
	item = bytes.TrimSpace(item)
	if len(item) == 0 || item[0] != '{' {
		return nil
	}
	var head struct {
		Type string `json:"type"`
	}
	decodeLenient(item, &head)
	var b ContentBlock
	switch head.Type {
	case "text":
		b = &TextBlock{}
	case "thinking":
		b = &ThinkingBlock{}
	case "redacted_thinking":
		b = &RedactedThinkingBlock{}
	case "tool_use":
		b = &ToolUseBlock{}
	case "tool_result":
		b = &ToolResultBlock{}
	case "server_tool_use":
		b = &ServerToolUseBlock{}
	case BlockAdvisorToolResult, BlockWebSearchToolResult, BlockWebFetchToolResult,
		BlockCodeExecutionToolResult, BlockBashCodeExecutionToolResult,
		BlockTextEditorCodeExecutionToolResult, BlockToolSearchToolResult:
		b = &ServerToolResultBlock{}
	case "mcp_tool_use":
		b = &MCPToolUseBlock{}
	case "mcp_tool_result":
		b = &MCPToolResultBlock{}
	case "mcp_tool_listing":
		b = &MCPToolListingBlock{}
	case "container_upload":
		b = &ContainerUploadBlock{}
	case "compaction":
		b = &CompactionBlock{}
	case "fallback":
		b = &FallbackBlock{}
	case "image":
		b = &ImageBlock{}
	case "document":
		b = &DocumentBlock{}
	default:
		b = &UnknownBlock{}
	}
	decodeLenient(item, b)
	return b
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

// decodeSystem decodes a typed system message and attaches its base.
func decodeSystem[T any, P interface {
	*T
	systemMessage
}](src []byte, base SystemMessage) Message {
	var v T
	p := P(&v)
	decodeLenient(src, p)
	*p.systemBase() = base
	return p
}

func parseSystemMessage(data map[string]any, src []byte) Message {
	subtype := str(data["subtype"])
	base := SystemMessage{Subtype: subtype, Data: data}
	switch subtype {
	case "init":
		return decodeSystem[InitMessage](src, base)
	case "task_started":
		return decodeSystem[TaskStartedMessage](src, base)
	case "task_progress":
		return decodeSystem[TaskProgressMessage](src, base)
	case "task_notification":
		return decodeSystem[TaskNotificationMessage](src, base)
	case "task_updated":
		m := decodeSystem[TaskUpdatedMessage](src, base).(*TaskUpdatedMessage)
		m.Status = m.Patch.Status
		return m
	case "hook_started", "hook_progress", "hook_response":
		return parseHookEvent(src, base)
	case "mirror_error":
		// Synthesized by the engine when SessionStore.Append fails; never
		// emitted by the CLI.
		return &MirrorErrorMessage{
			SystemMessage: base,
			Key:           parseSessionKey(data["key"]),
			Error:         str(data["error"]),
		}
	case "compact_boundary":
		return decodeSystem[CompactBoundaryMessage](src, base)
	case "status":
		return decodeSystem[StatusMessage](src, base)
	case "api_retry":
		return decodeSystem[APIRetryMessage](src, base)
	case "control_request_progress":
		return decodeSystem[ControlRequestProgressMessage](src, base)
	case "model_refusal_fallback":
		return decodeSystem[ModelRefusalFallbackMessage](src, base)
	case "model_refusal_no_fallback":
		return decodeSystem[ModelRefusalNoFallbackMessage](src, base)
	case "local_command_output":
		return decodeSystem[LocalCommandOutputMessage](src, base)
	case "plugin_install":
		return decodeSystem[PluginInstallMessage](src, base)
	case "background_tasks_changed":
		return decodeSystem[BackgroundTasksChangedMessage](src, base)
	case "thinking_tokens":
		return decodeSystem[ThinkingTokensMessage](src, base)
	case "session_state_changed":
		return decodeSystem[SessionStateChangedMessage](src, base)
	case "worker_shutting_down":
		return decodeSystem[WorkerShuttingDownMessage](src, base)
	case "commands_changed":
		return decodeSystem[CommandsChangedMessage](src, base)
	case "notification":
		return decodeSystem[NotificationMessage](src, base)
	case "files_persisted":
		return decodeSystem[FilesPersistedMessage](src, base)
	case "memory_recall":
		return decodeSystem[MemoryRecallMessage](src, base)
	case "elicitation_complete":
		return decodeSystem[ElicitationCompleteMessage](src, base)
	case "permission_denied":
		return decodeSystem[PermissionDeniedMessage](src, base)
	case "informational":
		return decodeSystem[InformationalMessage](src, base)
	default:
		return &base
	}
}

func parseHookEvent(src []byte, base SystemMessage) Message {
	var w struct {
		HookID        string `json:"hook_id"`
		HookName      string `json:"hook_name"`
		HookEvent     string `json:"hook_event"`
		HookEventName string `json:"hook_event_name"`
		SessionID     string `json:"session_id"`
		UUID          string `json:"uuid"`
		Stdout        string `json:"stdout"`
		Stderr        string `json:"stderr"`
		Output        string `json:"output"`
		ExitCode      *int   `json:"exit_code"`
		Outcome       string `json:"outcome"`
	}
	decodeLenient(src, &w)
	event := w.HookEvent
	if event == "" {
		event = w.HookEventName
	}
	if event == "" {
		// Older CLIs put the event name in hook_name.
		event = w.HookName
	}
	return &HookEventMessage{
		SystemMessage: base,
		HookEventName: event,
		SessionID:     w.SessionID,
		UUID:          w.UUID,
		HookID:        w.HookID,
		HookName:      w.HookName,
		Stdout:        w.Stdout,
		Stderr:        w.Stderr,
		Output:        w.Output,
		ExitCode:      w.ExitCode,
		Outcome:       w.Outcome,
	}
}

// parseSessionKey decodes a session key object, returning nil when v is not
// one. Both the Go snake_case shape {"project_key", "session_id", "subpath"}
// and the TypeScript camelCase shape {"projectKey", "sessionId", "subpath"}
// are accepted.
func parseSessionKey(v any) *SessionKey {
	switch k := v.(type) {
	case *SessionKey:
		return k
	case SessionKey:
		return &k
	case map[string]any:
		return &SessionKey{
			ProjectKey: firstString(k, "project_key", "projectKey"),
			SessionID:  firstString(k, "session_id", "sessionId"),
			Subpath:    str(k["subpath"]),
		}
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// result / stream_event / rate_limit_event
// ---------------------------------------------------------------------------

type resultWire struct {
	Subtype                string                `json:"subtype"`
	DurationMS             int                   `json:"duration_ms"`
	DurationAPIMS          int                   `json:"duration_api_ms"`
	IsError                bool                  `json:"is_error"`
	NumTurns               int                   `json:"num_turns"`
	SessionID              string                `json:"session_id"`
	StopReason             string                `json:"stop_reason"`
	TotalCostUSD           *float64              `json:"total_cost_usd"`
	Result                 string                `json:"result"`
	StructuredOutput       json.RawMessage       `json:"structured_output"`
	ModelUsage             map[string]ModelUsage `json:"modelUsage"`
	PermissionDenials      []PermissionDenial    `json:"permission_denials"`
	DeferredToolUse        *DeferredToolUse      `json:"deferred_tool_use"`
	APIErrorStatus         *int                  `json:"api_error_status"`
	UUID                   string                `json:"uuid"`
	TerminalReason         string                `json:"terminal_reason"`
	QueuedTurnCount        *int                  `json:"queued_turn_count"`
	UserMessageUUID        string                `json:"user_message_uuid"`
	UserMessageUUIDs       []string              `json:"user_message_uuids"`
	ResumeReason           string                `json:"resume_reason"`
	ResultIndex            *int                  `json:"result_index"`
	FastModeState          string                `json:"fast_mode_state"`
	FastModeDisabledReason string                `json:"fast_mode_disabled_reason"`
	StartupFailureReason   string                `json:"startup_failure_reason"`
	LocalCommand           string                `json:"local_command"`
}

func parseResultMessage(data map[string]any, src []byte) Message {
	var w resultWire
	decodeLenient(src, &w)
	usage, _ := data["usage"].(map[string]any)
	msg := &ResultMessage{
		Subtype:                w.Subtype,
		DurationMS:             w.DurationMS,
		DurationAPIMS:          w.DurationAPIMS,
		IsError:                w.IsError,
		NumTurns:               w.NumTurns,
		SessionID:              w.SessionID,
		StopReason:             w.StopReason,
		TotalCostUSD:           w.TotalCostUSD,
		Usage:                  usage,
		Result:                 w.Result,
		ModelUsage:             w.ModelUsage,
		PermissionDenials:      w.PermissionDenials,
		DeferredToolUse:        w.DeferredToolUse,
		Errors:                 normalizeResultErrors(data["errors"]),
		APIErrorStatus:         w.APIErrorStatus,
		UUID:                   w.UUID,
		TerminalReason:         w.TerminalReason,
		Origin:                 parseOrigin(data),
		QueuedTurnCount:        w.QueuedTurnCount,
		UserMessageUUID:        w.UserMessageUUID,
		UserMessageUUIDs:       w.UserMessageUUIDs,
		ResumeReason:           w.ResumeReason,
		ResultIndex:            w.ResultIndex,
		FastModeState:          w.FastModeState,
		FastModeDisabledReason: w.FastModeDisabledReason,
		StartupFailureReason:   w.StartupFailureReason,
		LocalCommand:           w.LocalCommand,
		Data:                   data,
	}
	// Counters are read from the map too, so a fractional value is truncated
	// rather than dropped.
	for key, dst := range map[string]*int{
		"duration_ms": &msg.DurationMS, "duration_api_ms": &msg.DurationAPIMS, "num_turns": &msg.NumTurns,
	} {
		if n, ok := toInt(data[key]); ok {
			*dst = n
		}
	}
	if so := bytes.TrimSpace(w.StructuredOutput); len(so) > 0 && string(so) != "null" {
		msg.StructuredOutput = so
	}
	var timing ResultTiming
	decodeLenient(src, &timing)
	if timing != (ResultTiming{}) {
		msg.Timing = &timing
	}
	return msg
}

func parseStreamEvent(data map[string]any, src []byte) Message {
	var w struct {
		UUID             string   `json:"uuid"`
		SessionID        string   `json:"session_id"`
		ParentToolUseID  string   `json:"parent_tool_use_id"`
		TTFTMS           *float64 `json:"ttft_ms"`
		UserMessageUUID  string   `json:"user_message_uuid"`
		UserMessageUUIDs []string `json:"user_message_uuids"`
		ResumeReason     string   `json:"resume_reason"`
	}
	decodeLenient(src, &w)
	// The event, the bulk of the frame, comes from the decoded frame map.
	event, _ := data["event"].(map[string]any)
	return &StreamEvent{
		UUID:             w.UUID,
		SessionID:        w.SessionID,
		Event:            event,
		ParentToolUseID:  w.ParentToolUseID,
		TTFTMS:           w.TTFTMS,
		UserMessageUUID:  w.UserMessageUUID,
		UserMessageUUIDs: w.UserMessageUUIDs,
		ResumeReason:     w.ResumeReason,
	}
}

func parseRateLimitEvent(data map[string]any, src []byte) Message {
	var w struct {
		Info struct {
			Status                          string   `json:"status"`
			RateLimitType                   string   `json:"rateLimitType"`
			Utilization                     *float64 `json:"utilization"`
			OverageStatus                   string   `json:"overageStatus"`
			OverageDisabledReason           string   `json:"overageDisabledReason"`
			IsUsingOverage                  bool     `json:"isUsingOverage"`
			OverageInUse                    bool     `json:"overageInUse"`
			SurpassedThreshold              *float64 `json:"surpassedThreshold"`
			LimitScope                      string   `json:"limitScope"`
			ErrorCode                       string   `json:"errorCode"`
			CanUserPurchaseCredits          bool     `json:"canUserPurchaseCredits"`
			HasChargeableSavedPaymentMethod bool     `json:"hasChargeableSavedPaymentMethod"`
		} `json:"rate_limit_info"`
		UUID      string `json:"uuid"`
		SessionID string `json:"session_id"`
	}
	decodeLenient(src, &w)
	info, _ := data["rate_limit_info"].(map[string]any)
	in := w.Info
	rli := RateLimitInfo{
		Status:                          in.Status,
		RateLimitType:                   in.RateLimitType,
		Utilization:                     in.Utilization,
		OverageStatus:                   in.OverageStatus,
		OverageDisabledReason:           in.OverageDisabledReason,
		IsUsingOverage:                  in.IsUsingOverage,
		OverageInUse:                    in.OverageInUse,
		SurpassedThreshold:              in.SurpassedThreshold,
		LimitScope:                      in.LimitScope,
		ErrorCode:                       in.ErrorCode,
		CanUserPurchaseCredits:          in.CanUserPurchaseCredits,
		HasChargeableSavedPaymentMethod: in.HasChargeableSavedPaymentMethod,
		Raw:                             info,
	}
	// Timestamps are read from the map so fractional seconds still parse.
	if n, ok := toInt64(info["resetsAt"]); ok {
		rli.ResetsAt = &n
	}
	if n, ok := toInt64(info["overageResetsAt"]); ok {
		rli.OverageResetsAt = &n
	}
	return &RateLimitEvent{RateLimitInfo: rli, UUID: w.UUID, SessionID: w.SessionID}
}

func parseConversationReset(src []byte) Message {
	var w struct {
		NewConversationID string `json:"new_conversation_id"`
		UUID              string `json:"uuid"`
		SessionID         string `json:"session_id"`
		Trigger           string `json:"trigger"`
		UserMessageUUID   string `json:"user_message_uuid"`
		Timestamp         string `json:"timestamp"`
	}
	decodeLenient(src, &w)
	return &ConversationResetMessage{
		NewConversationID: w.NewConversationID,
		UUID:              w.UUID,
		SessionID:         w.SessionID,
		Trigger:           w.Trigger,
		UserMessageUUID:   w.UserMessageUUID,
		Timestamp:         w.Timestamp,
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// parseOrigin returns data["origin"] when it is a non-empty object, passing
// keys this SDK version does not model through to Extra. A missing or
// non-string kind falls back to OriginUnclassified rather than discarding the
// rest of the origin.
func parseOrigin(data map[string]any) *MessageOrigin {
	raw, ok := data["origin"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	o := &MessageOrigin{
		Server:       str(raw["server"]),
		From:         str(raw["from"]),
		Name:         str(raw["name"]),
		FromSession:  str(raw["fromSession"]),
		SenderTaskID: str(raw["senderTaskId"]),
		Body:         str(raw["body"]),
		Subkind:      str(raw["subkind"]),
	}
	o.VerifiedPeerPID, _ = toInt(raw["verifiedPeerPid"])
	for k, v := range raw {
		if !originModeledKeys[k] {
			o.putExtra(k, v)
		}
	}
	if kind, ok := raw["kind"].(string); ok && kind != "" {
		o.Kind = kind
	} else {
		o.Kind = OriginUnclassified
		if v := raw["kind"]; v != nil {
			o.putExtra("kind", v)
		}
	}
	return o
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func toInt64(v any) (int64, bool) {
	n, ok := toInt(v)
	return int64(n), ok
}
