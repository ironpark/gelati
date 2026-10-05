package agy

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/ironpark/gelati/agy/internal/wire"
	"github.com/ironpark/gelati/internal/jsonx"
)

// Conversions between wire messages and the SDK types.

// builtinToolFields lists the StepUpdate action fields of the builtin tools
// with the SDK tool name, in upstream's lookup order.
var builtinToolFields = []struct {
	tool BuiltinTool
	get  func(*wire.StepUpdate) (proto.Message, bool)
}{
	{BuiltinCreateFile, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetCreateFile(), s.HasCreateFile() }},
	{BuiltinEditFile, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetEditFile(), s.HasEditFile() }},
	{BuiltinFindFile, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetFindFile(), s.HasFindFile() }},
	{BuiltinListDir, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetListDirectory(), s.HasListDirectory() }},
	{BuiltinRunCommand, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetRunCommand(), s.HasRunCommand() }},
	{BuiltinSearchDir, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetSearchDirectory(), s.HasSearchDirectory() }},
	{BuiltinViewFile, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetViewFile(), s.HasViewFile() }},
	{BuiltinStartSubagent, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetInvokeSubagent(), s.HasInvokeSubagent() }},
	{BuiltinGenerateImage, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetGenerateImage(), s.HasGenerateImage() }},
	{BuiltinSearchWeb, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetSearchWeb(), s.HasSearchWeb() }},
	{BuiltinReadURLContent, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetReadUrlContent(), s.HasReadUrlContent() }},
	{BuiltinFinish, func(s *wire.StepUpdate) (proto.Message, bool) { return s.GetFinish(), s.HasFinish() }},
}

// protoToolNames maps the harness's builtin tool names (the StepUpdate
// field names) to the SDK names where they differ.
var protoToolNames = map[string]string{"invoke_subagent": string(BuiltinStartSubagent)}

func sdkToolName(protoName string) string {
	if n, ok := protoToolNames[protoName]; ok {
		return n
	}
	return protoName
}

// toolCallFromWire builds the ToolCall of a tool the harness reports (a
// step's action, a PreTool hook or a policy decision request). The harness
// tool name is mapped to the SDK name, and the wire path arguments of args
// (nil for none) are normalized in place; the first non-empty one becomes
// the canonical path.
func toolCallFromWire(name string, args map[string]any, id, stepID, serverName string) *ToolCall {
	if args == nil {
		args = map[string]any{}
	}
	return &ToolCall{
		Name:          sdkToolName(name),
		Args:          args,
		ID:            id,
		StepID:        stepID,
		CanonicalPath: normalizePathArgs(args),
		ServerName:    serverName,
	}
}

var (
	sourceMap = map[wire.StepUpdate_Source]StepSource{
		wire.StepUpdate_SOURCE_SYSTEM: StepSourceSystem,
		wire.StepUpdate_SOURCE_USER:   StepSourceUser,
		wire.StepUpdate_SOURCE_MODEL:  StepSourceModel,
	}
	statusMap = map[wire.StepUpdate_State]StepStatus{
		wire.StepUpdate_STATE_ACTIVE:           StepStatusActive,
		wire.StepUpdate_STATE_DONE:             StepStatusDone,
		wire.StepUpdate_STATE_WAITING_FOR_USER: StepStatusWaitingForUser,
		wire.StepUpdate_STATE_ERROR:            StepStatusError,
	}
	targetMap = map[wire.StepUpdate_Target]StepTarget{
		wire.StepUpdate_TARGET_USER:        StepTargetUser,
		wire.StepUpdate_TARGET_ENVIRONMENT: StepTargetEnvironment,
		wire.StepUpdate_TARGET_UNSPECIFIED: StepTargetUnspecified,
	}
	stopReasonMap = map[wire.TrajectoryStateUpdate_StopReason]StopReason{
		wire.TrajectoryStateUpdate_STOP_REASON_MAX_MODEL_CALLS_EXCEEDED:   StopReasonMaxModelCallsExceeded,
		wire.TrajectoryStateUpdate_STOP_REASON_MAX_TOOL_CALLS_EXCEEDED:    StopReasonMaxToolCallsExceeded,
		wire.TrajectoryStateUpdate_STOP_REASON_MAX_INPUT_TOKENS_EXCEEDED:  StopReasonMaxInputTokensExceeded,
		wire.TrajectoryStateUpdate_STOP_REASON_MAX_OUTPUT_TOKENS_EXCEEDED: StopReasonMaxOutputTokensExceeded,
		wire.TrajectoryStateUpdate_STOP_REASON_MAX_TOTAL_TOKENS_EXCEEDED:  StopReasonMaxTotalTokensExceeded,
		wire.TrajectoryStateUpdate_STOP_REASON_QUOTA_EXHAUSTED:            StopReasonQuotaExhausted,
	}
)

func parseStopReason(r wire.TrajectoryStateUpdate_StopReason) StopReason {
	if s, ok := stopReasonMap[r]; ok {
		return s
	}
	return StopReasonUnspecified
}

// toolCallArgs extracts the arguments of a wire tool call: the structured
// arguments when present, else arguments_json when it holds an object, else
// an empty map.
func toolCallArgs(tc *wire.ToolCall) map[string]any {
	if tc.GetArguments() != nil {
		if m := tc.GetArguments().AsMap(); m != nil {
			return m
		}
		return map[string]any{}
	}
	return argsFromJSON(tc.GetArgumentsJson())
}

// stepFromUpdate converts a StepUpdate into a Step (upstream
// LocalConnectionStep.from_dict).
func stepFromUpdate(su *wire.StepUpdate) *Step {
	trajID := su.GetTrajectoryId()
	idx := int(su.GetStepIndex())
	stepID := makeStepID(trajID, idx)

	var toolName, toolID, serverName string
	var args map[string]any
	builtinPresent := false
	for _, f := range builtinToolFields {
		msg, ok := f.get(su)
		if !ok {
			continue
		}
		builtinPresent = true
		if toolName == "" {
			toolName = string(f.tool)
			args, _ = wire.ProtoMap(msg)
		}
	}
	if toolName == "" && su.GetMcpTool() != nil {
		serverName = su.GetMcpTool().GetServerName()
		toolName = su.GetMcpTool().GetToolName()
		args = argsFromJSON(su.GetMcpTool().GetArgumentsJson())
	}
	if toolName == "" && su.GetCustomTool() != nil && su.GetCustomTool().GetToolCall() != nil {
		tc := su.GetCustomTool().GetToolCall()
		toolName = tc.GetName()
		toolID = tc.GetId()
		args = toolCallArgs(tc)
	}

	step := &Step{
		ID:                 stepID,
		StepIndex:          idx,
		TrajectoryID:       trajID,
		ParentTrajectoryID: su.GetParentTrajectoryId(),
		Content:            su.GetText(),
		ContentDelta:       su.GetTextDelta(),
		Thinking:           su.GetThinking(),
		ThinkingDelta:      su.GetThinkingDelta(),
		Source:             StepSourceUnknown,
		Status:             StepStatusUnknown,
		Target:             StepTargetUnknown,
		Type:               StepTypeUnknown,
	}
	if s, ok := sourceMap[su.GetSource()]; ok {
		step.Source = s
	}
	if s, ok := statusMap[su.GetState()]; ok {
		step.Status = s
	}
	if su.HasTarget() {
		if t, ok := targetMap[su.GetTarget()]; ok {
			step.Target = t
		}
	}

	if toolName != "" {
		step.ToolCalls = []*ToolCall{toolCallFromWire(toolName, args, cmp.Or(toolID, stepID), stepID, serverName)}
	}

	switch {
	case su.GetCompaction() != nil:
		step.Type = StepTypeCompaction
	case su.GetFinish() != nil:
		step.Type = StepTypeFinish
	case toolName != "" || builtinPresent:
		step.Type = StepTypeToolCall
	case su.GetText() != "":
		step.Type = StepTypeTextResponse
	case su.GetThinking() != "":
		step.Type = StepTypeThinking
	}

	step.IsCompleteResponse = step.Source == StepSourceModel && step.Status == StepStatusDone &&
		su.GetText() != "" && su.GetTarget() == wire.StepUpdate_TARGET_USER

	if step.Type == StepTypeFinish {
		if out := su.GetFinish().GetOutputString(); out != "" {
			var v any
			if err := jsonx.Unmarshal([]byte(out), &v); err == nil {
				step.StructuredOutput = v
			}
		}
	}

	step.Error = su.GetError().GetErrorMessage()
	if step.Error == "" {
		step.Error = su.GetErrorMessage()
	}
	step.HTTPCode = int(su.GetError().GetHttpCode())
	return step
}

// parseUsage converts wire usage metadata. An unknown service tier (Vertex
// reports tiers of its own) is dropped rather than failing the event.
func parseUsage(u *wire.UsageMetadata) UsageMetadata {
	var out UsageMetadata
	conv := func(has bool, v uint64) *int64 {
		if !has {
			return nil
		}
		return new(int64(v))
	}
	out.PromptTokenCount = conv(u.HasPromptTokenCount(), u.GetPromptTokenCount())
	out.CachedContentTokenCount = conv(u.HasCachedContentTokenCount(), u.GetCachedContentTokenCount())
	out.CandidatesTokenCount = conv(u.HasCandidatesTokenCount(), u.GetCandidatesTokenCount())
	out.ThoughtsTokenCount = conv(u.HasThoughtsTokenCount(), u.GetThoughtsTokenCount())
	out.TotalTokenCount = conv(u.HasTotalTokenCount(), u.GetTotalTokenCount())
	if tier, ok := knownServiceTier(u.GetServiceTier()); ok {
		out.ServiceTier = tier
	}
	return out
}

// initializeResult is the parsed InitializeConversationResponse.
type initializeResult struct {
	conversationID   string
	history          []*Step
	cumulativeUsage  *UsageMetadata
	trajectoryUsages map[string]UsageMetadata
	sandbox          *SandboxStatus
}

func parseInitializeResponse(resp *wire.InitializeConversationResponse) initializeResult {
	r := initializeResult{conversationID: resp.GetCascadeId()}
	for _, su := range resp.GetHistory() {
		r.history = append(r.history, stepFromUpdate(su))
	}
	if u := resp.GetCumulativeUsage(); u != nil {
		pu := parseUsage(u)
		r.cumulativeUsage = &pu
	}
	r.trajectoryUsages = map[string]UsageMetadata{}
	for _, e := range resp.GetTrajectoryUsage() {
		if e.GetTrajectoryId() != "" && e.GetUsage() != nil {
			r.trajectoryUsages[e.GetTrajectoryId()] = parseUsage(e.GetUsage())
		}
	}
	if s := resp.GetSandboxStatus(); s != nil {
		r.sandbox = &SandboxStatus{Available: s.GetAvailable(), UnavailableReason: s.GetUnavailableReason()}
	}
	return r
}

// userInputParts converts prompt content into UserInput parts. No content
// sends one empty text part.
func userInputParts(content []Content) ([]*wire.UserInput_Part, error) {
	if len(content) == 0 {
		return []*wire.UserInput_Part{wire.UserInput_Part_builder{Text: new("")}.Build()}, nil
	}
	parts := make([]*wire.UserInput_Part, 0, len(content))
	for _, c := range content {
		switch c := c.(type) {
		case Text:
			parts = append(parts, wire.UserInput_Part_builder{Text: new(sanitizePrompt(string(c)))}.Build())
		case SlashCommand:
			parts = append(parts, wire.UserInput_Part_builder{SlashCommand: wire.UserInput_SlashCommand_builder{Name: new(string(c))}.Build()}.Build())
		case Media:
			if err := validateMedia(c); err != nil {
				return nil, err
			}
			data, mimeType, desc := c.MediaData()
			m := wire.UserInput_Media_builder{MimeType: new(mimeType), Data: nonNilBytes(data)}.Build()
			if desc != "" {
				m.SetDescription(desc)
			}
			parts = append(parts, wire.UserInput_Part_builder{Media: m}.Build())
		default:
			return nil, validationErrorf("Unsupported prompt content type: %T", c)
		}
	}
	return parts, nil
}

func nonNilBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// contentFromUserInput converts wire UserInput parts back into prompt
// content, dropping parts that do not map to a known content type.
func contentFromUserInput(ui *wire.UserInput) []Content {
	var out []Content
	for _, p := range ui.GetParts() {
		switch {
		case p.HasText():
			out = append(out, Text(p.GetText()))
		case p.GetSlashCommand() != nil:
			if name := SlashCommand(p.GetSlashCommand().GetName()); name == SlashCommandPlan {
				out = append(out, name)
			}
		case p.GetMedia() != nil:
			m, err := FromBytes(p.GetMedia().GetData(), p.GetMedia().GetMimeType(), p.GetMedia().GetDescription())
			if err == nil {
				out = append(out, m)
			}
		}
	}
	return out
}

// mediaOf returns v as Media when it is one of the media types or a
// pointer to one.
func mediaOf(v any) (Media, bool) {
	switch m := v.(type) {
	case Image, Document, Audio, Video:
		return m.(Media), true
	case *Image:
		return m, m != nil
	case *Document:
		return m, m != nil
	case *Audio:
		return m, m != nil
	case *Video:
		return m, m != nil
	}
	return nil, false
}

// extractMedia splits media attachments out of a tool result. It walks
// slices, arrays and maps; the cleaned value has the media removed and is
// nil when nothing else is left.
func extractMedia(v any) (any, []Media) {
	if m, ok := mediaOf(v); ok {
		return nil, []Media{m}
	}
	if v == nil {
		return nil, nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return v, nil
		}
		var cleaned []any
		var media []Media
		for i := range rv.Len() {
			c, m := extractMedia(rv.Index(i).Interface())
			media = append(media, m...)
			if c != nil {
				cleaned = append(cleaned, c)
			}
		}
		if len(media) == 0 {
			return v, nil
		}
		if len(cleaned) == 0 {
			return nil, media
		}
		return cleaned, media
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return v, nil
		}
		cleaned := map[string]any{}
		var media []Media
		iter := rv.MapRange()
		keys := []string{}
		vals := map[string]any{}
		for iter.Next() {
			k := iter.Key().String()
			keys = append(keys, k)
			vals[k] = iter.Value().Interface()
		}
		slices.Sort(keys)
		for _, k := range keys {
			c, m := extractMedia(vals[k])
			media = append(media, m...)
			if c != nil {
				cleaned[k] = c
			}
		}
		if len(media) == 0 {
			return v, nil
		}
		if len(cleaned) == 0 {
			return nil, media
		}
		return cleaned, media
	}
	return v, nil
}

// toolResultPayload converts a tool result to the JSON object sent to the
// model: {"error": msg} for failures, the result itself when it encodes as
// an object, else {"result": value}. A value encoding/json cannot encode is
// sent as its fmt representation.
func toolResultPayload(r *ToolResult) map[string]any {
	if r.Failed() {
		return map[string]any{"error": r.Error}
	}
	out, err := toJSONValue(r.Result)
	if err != nil {
		out = fmt.Sprint(r.Result)
	}
	if m, ok := out.(map[string]any); ok {
		return m
	}
	return map[string]any{"result": out}
}

// toolResponse builds the ToolResponse sent back to the harness for a
// custom tool call.
func toolResponse(r *ToolResult) (*wire.ToolResponse, error) {
	if r.ID == "" {
		return nil, fmt.Errorf("agy: ToolResult for '%s' is missing an id. The local connection protocol requires an id to correlate results with calls.", r.Name)
	}
	if r.Failed() {
		msg := r.Error
		if msg == "" && r.Err != nil {
			msg = r.Err.Error()
		}
		return wire.ToolResponse_builder{Id: new(r.ID), ErrorMessage: new(msg)}.Build(), nil
	}
	cleaned, media := extractMedia(r.Result)
	res := *r
	if len(media) > 0 {
		if cleaned == nil {
			cleaned = fmt.Sprintf("Returned %d media attachment(s).", len(media))
		}
		res.Result = cleaned
	}
	b, err := jsonx.Marshal(toolResultPayload(&res))
	if err != nil {
		return nil, err
	}
	resp := wire.ToolResponse_builder{Id: new(r.ID), ResponseJson: new(string(b))}.Build()
	for _, m := range media {
		data, mimeType, desc := m.MediaData()
		wm := wire.Media_builder{MimeType: new(mimeType), Data: nonNilBytes(data)}.Build()
		if desc != "" {
			wm.SetDescription(desc)
		}
		resp.SetSupplementalMedia(append(resp.GetSupplementalMedia(), wm))
	}
	return resp, nil
}

// structOfArgs converts tool arguments to a wire Struct. Values that cannot
// be encoded are sent as their fmt representation, as upstream falls back
// to str().
func structOfArgs(args map[string]any) *wire.Struct {
	s, err := wire.StructOf(args)
	if err != nil {
		return &wire.Struct{}
	}
	return s
}
