package antigravity

import (
	"cmp"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	"github.com/ironpark/gelati/antigravity/internal/wire"
)

// Conversions between wire messages and the SDK types.

// builtinToolFields lists the StepUpdate action fields of the builtin tools
// with the SDK tool name, in upstream's lookup order.
var builtinToolFields = []struct {
	tool BuiltinTool
	get  func(*wire.StepUpdate) (any, bool)
}{
	{BuiltinCreateFile, func(s *wire.StepUpdate) (any, bool) { return s.CreateFile, s.CreateFile != nil }},
	{BuiltinEditFile, func(s *wire.StepUpdate) (any, bool) { return s.EditFile, s.EditFile != nil }},
	{BuiltinFindFile, func(s *wire.StepUpdate) (any, bool) { return s.FindFile, s.FindFile != nil }},
	{BuiltinListDir, func(s *wire.StepUpdate) (any, bool) { return s.ListDirectory, s.ListDirectory != nil }},
	{BuiltinRunCommand, func(s *wire.StepUpdate) (any, bool) { return s.RunCommand, s.RunCommand != nil }},
	{BuiltinSearchDir, func(s *wire.StepUpdate) (any, bool) { return s.SearchDirectory, s.SearchDirectory != nil }},
	{BuiltinViewFile, func(s *wire.StepUpdate) (any, bool) { return s.ViewFile, s.ViewFile != nil }},
	{BuiltinStartSubagent, func(s *wire.StepUpdate) (any, bool) { return s.InvokeSubagent, s.InvokeSubagent != nil }},
	{BuiltinGenerateImage, func(s *wire.StepUpdate) (any, bool) { return s.GenerateImage, s.GenerateImage != nil }},
	{BuiltinSearchWeb, func(s *wire.StepUpdate) (any, bool) { return s.SearchWeb, s.SearchWeb != nil }},
	{BuiltinReadURLContent, func(s *wire.StepUpdate) (any, bool) { return s.ReadURLContent, s.ReadURLContent != nil }},
	{BuiltinFinish, func(s *wire.StepUpdate) (any, bool) { return s.Finish, s.Finish != nil }},
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
	sourceMap = map[wire.StepUpdateSource]StepSource{
		wire.StepUpdateSourceSystem: StepSourceSystem,
		wire.StepUpdateSourceUser:   StepSourceUser,
		wire.StepUpdateSourceModel:  StepSourceModel,
	}
	statusMap = map[wire.StepUpdateState]StepStatus{
		wire.StepUpdateStateActive:         StepStatusActive,
		wire.StepUpdateStateDone:           StepStatusDone,
		wire.StepUpdateStateWaitingForUser: StepStatusWaitingForUser,
		wire.StepUpdateStateError:          StepStatusError,
	}
	targetMap = map[wire.StepUpdateTarget]StepTarget{
		wire.StepUpdateTargetUser:        StepTargetUser,
		wire.StepUpdateTargetEnvironment: StepTargetEnvironment,
		wire.StepUpdateTargetUnspecified: StepTargetUnspecified,
	}
	stopReasonMap = map[wire.TrajectoryStateUpdateStopReason]StopReason{
		wire.TrajectoryStateUpdateStopReasonMaxModelCallsExceeded:   StopReasonMaxModelCallsExceeded,
		wire.TrajectoryStateUpdateStopReasonMaxToolCallsExceeded:    StopReasonMaxToolCallsExceeded,
		wire.TrajectoryStateUpdateStopReasonMaxInputTokensExceeded:  StopReasonMaxInputTokensExceeded,
		wire.TrajectoryStateUpdateStopReasonMaxOutputTokensExceeded: StopReasonMaxOutputTokensExceeded,
		wire.TrajectoryStateUpdateStopReasonMaxTotalTokensExceeded:  StopReasonMaxTotalTokensExceeded,
		wire.TrajectoryStateUpdateStopReasonQuotaExhausted:          StopReasonQuotaExhausted,
	}
)

func parseStopReason(r wire.TrajectoryStateUpdateStopReason) StopReason {
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
		if m := tc.Arguments.AsMap(); m != nil {
			return m
		}
		return map[string]any{}
	}
	return argsFromJSON(tc.GetArgumentsJSON())
}

// stepFromUpdate converts a StepUpdate into a Step (upstream
// LocalConnectionStep.from_dict).
func stepFromUpdate(su *wire.StepUpdate) *Step {
	trajID := su.GetTrajectoryID()
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
	if toolName == "" && su.MCPTool != nil {
		serverName = su.MCPTool.GetServerName()
		toolName = su.MCPTool.GetToolName()
		args = argsFromJSON(su.MCPTool.GetArgumentsJSON())
	}
	if toolName == "" && su.CustomTool != nil && su.CustomTool.ToolCall != nil {
		tc := su.CustomTool.ToolCall
		toolName = tc.GetName()
		toolID = tc.GetID()
		args = toolCallArgs(tc)
	}

	step := &Step{
		ID:                 stepID,
		StepIndex:          idx,
		TrajectoryID:       trajID,
		ParentTrajectoryID: su.GetParentTrajectoryID(),
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
	if su.Target != nil {
		if t, ok := targetMap[*su.Target]; ok {
			step.Target = t
		}
	}

	if toolName != "" {
		step.ToolCalls = []*ToolCall{toolCallFromWire(toolName, args, cmp.Or(toolID, stepID), stepID, serverName)}
	}

	switch {
	case su.Compaction != nil:
		step.Type = StepTypeCompaction
	case su.Finish != nil:
		step.Type = StepTypeFinish
	case toolName != "" || builtinPresent:
		step.Type = StepTypeToolCall
	case su.GetText() != "":
		step.Type = StepTypeTextResponse
	case su.GetThinking() != "":
		step.Type = StepTypeThinking
	}

	step.IsCompleteResponse = step.Source == StepSourceModel && step.Status == StepStatusDone &&
		su.GetText() != "" && su.GetTarget() == wire.StepUpdateTargetUser

	if step.Type == StepTypeFinish {
		if out := su.Finish.GetOutputString(); out != "" {
			var v any
			if err := json.Unmarshal([]byte(out), &v); err == nil {
				step.StructuredOutput = v
			}
		}
	}

	step.Error = su.GetError().GetErrorMessage()
	if step.Error == "" {
		step.Error = su.GetErrorMessage()
	}
	step.HTTPCode = int(su.GetError().GetHTTPCode())
	return step
}

// parseUsage converts wire usage metadata. An unknown service tier (Vertex
// reports tiers of its own) is dropped rather than failing the event.
func parseUsage(u *wire.UsageMetadata) UsageMetadata {
	var out UsageMetadata
	conv := func(p *wire.Uint64) *int64 {
		if p == nil {
			return nil
		}
		return new(int64(*p))
	}
	out.PromptTokenCount = conv(u.PromptTokenCount)
	out.CachedContentTokenCount = conv(u.CachedContentTokenCount)
	out.CandidatesTokenCount = conv(u.CandidatesTokenCount)
	out.ThoughtsTokenCount = conv(u.ThoughtsTokenCount)
	out.TotalTokenCount = conv(u.TotalTokenCount)
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
	r := initializeResult{conversationID: resp.GetCascadeID()}
	for _, su := range resp.GetHistory() {
		r.history = append(r.history, stepFromUpdate(su))
	}
	if u := resp.GetCumulativeUsage(); u != nil {
		pu := parseUsage(u)
		r.cumulativeUsage = &pu
	}
	r.trajectoryUsages = map[string]UsageMetadata{}
	for _, e := range resp.GetTrajectoryUsage() {
		if e.GetTrajectoryID() != "" && e.GetUsage() != nil {
			r.trajectoryUsages[e.GetTrajectoryID()] = parseUsage(e.GetUsage())
		}
	}
	if s := resp.GetSandboxStatus(); s != nil {
		r.sandbox = &SandboxStatus{Available: s.GetAvailable(), UnavailableReason: s.GetUnavailableReason()}
	}
	return r
}

// userInputParts converts prompt content into UserInput parts. No content
// sends one empty text part.
func userInputParts(content []Content) ([]*wire.UserInputPart, error) {
	if len(content) == 0 {
		return []*wire.UserInputPart{{Text: new("")}}, nil
	}
	parts := make([]*wire.UserInputPart, 0, len(content))
	for _, c := range content {
		switch c := c.(type) {
		case Text:
			parts = append(parts, &wire.UserInputPart{Text: new(sanitizePrompt(string(c)))})
		case SlashCommand:
			parts = append(parts, &wire.UserInputPart{SlashCommand: &wire.UserInputSlashCommand{Name: new(string(c))}})
		case Media:
			if err := validateMedia(c); err != nil {
				return nil, err
			}
			data, mimeType, desc := c.MediaData()
			m := &wire.UserInputMedia{MimeType: new(mimeType), Data: nonNilBytes(data)}
			if desc != "" {
				m.Description = new(desc)
			}
			parts = append(parts, &wire.UserInputPart{Media: m})
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
		case p.Text != nil:
			out = append(out, Text(*p.Text))
		case p.SlashCommand != nil:
			if name := SlashCommand(p.SlashCommand.GetName()); name == SlashCommandPlan {
				out = append(out, name)
			}
		case p.Media != nil:
			m, err := FromBytes(p.Media.Data, p.Media.GetMimeType(), p.Media.GetDescription())
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
		return nil, fmt.Errorf("antigravity: ToolResult for '%s' is missing an id. The local connection protocol requires an id to correlate results with calls.", r.Name)
	}
	if r.Failed() {
		msg := r.Error
		if msg == "" && r.Err != nil {
			msg = r.Err.Error()
		}
		return &wire.ToolResponse{ID: new(r.ID), ErrorMessage: new(msg)}, nil
	}
	cleaned, media := extractMedia(r.Result)
	res := *r
	if len(media) > 0 {
		if cleaned == nil {
			cleaned = fmt.Sprintf("Returned %d media attachment(s).", len(media))
		}
		res.Result = cleaned
	}
	b, err := json.Marshal(toolResultPayload(&res))
	if err != nil {
		return nil, err
	}
	resp := &wire.ToolResponse{ID: new(r.ID), ResponseJSON: new(string(b))}
	for _, m := range media {
		data, mimeType, desc := m.MediaData()
		wm := &wire.Media{MimeType: new(mimeType), Data: nonNilBytes(data)}
		if desc != "" {
			wm.Description = new(desc)
		}
		resp.SupplementalMedia = append(resp.SupplementalMedia, wm)
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
