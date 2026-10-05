package agy

import (
	"fmt"
	"maps"
	"math"
	"slices"
)

// StepType is the high-level type of a step.
type StepType string

// StepType values.
const (
	StepTypeTextResponse  StepType = "TEXT_RESPONSE"
	StepTypeToolCall      StepType = "TOOL_CALL"
	StepTypeSystemMessage StepType = "SYSTEM_MESSAGE"
	StepTypeCompaction    StepType = "COMPACTION"
	StepTypeFinish        StepType = "FINISH"
	StepTypeThinking      StepType = "THINKING"
	StepTypeUnknown       StepType = "UNKNOWN"
)

// StepSource is who produced a step.
type StepSource string

// StepSource values.
const (
	StepSourceSystem  StepSource = "SYSTEM"
	StepSourceUser    StepSource = "USER"
	StepSourceModel   StepSource = "MODEL"
	StepSourceUnknown StepSource = "UNKNOWN"
)

// StepTarget is who a step is directed at.
type StepTarget string

// StepTarget values.
const (
	StepTargetUser        StepTarget = "TARGET_USER"
	StepTargetEnvironment StepTarget = "TARGET_ENVIRONMENT"
	StepTargetUnspecified StepTarget = "TARGET_UNSPECIFIED"
	StepTargetUnknown     StepTarget = "UNKNOWN"
)

// StepStatus is the status of a step.
type StepStatus string

// StepStatus values.
const (
	StepStatusActive         StepStatus = "ACTIVE"
	StepStatusDone           StepStatus = "DONE"
	StepStatusWaitingForUser StepStatus = "WAITING_FOR_USER"
	StepStatusError          StepStatus = "ERROR"
	StepStatusCanceled       StepStatus = "CANCELED"
	StepStatusUnknown        StepStatus = "UNKNOWN"
)

// StopReason says why a turn stopped.
type StopReason string

// StopReason values.
const (
	// StopReasonUnspecified is a normal completion.
	StopReasonUnspecified             StopReason = "UNSPECIFIED"
	StopReasonMaxModelCallsExceeded   StopReason = "MAX_MODEL_CALLS_EXCEEDED"
	StopReasonMaxToolCallsExceeded    StopReason = "MAX_TOOL_CALLS_EXCEEDED"
	StopReasonMaxInputTokensExceeded  StopReason = "MAX_INPUT_TOKENS_EXCEEDED"
	StopReasonMaxOutputTokensExceeded StopReason = "MAX_OUTPUT_TOKENS_EXCEEDED"
	StopReasonMaxTotalTokensExceeded  StopReason = "MAX_TOTAL_TOKENS_EXCEEDED"
	// StopReasonQuotaExhausted means the model API quota ran out.
	StopReasonQuotaExhausted StopReason = "QUOTA_EXHAUSTED"
)

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	// Name is the tool name: a BuiltinTool value for harness tools, the
	// tool name for custom and MCP tools.
	Name string
	// Args are the call's arguments as decoded JSON.
	Args map[string]any
	// ID identifies the call, when the backend assigned one.
	ID string
	// StepID correlates the call with its step in the trajectory.
	StepID string
	// CanonicalPath is the normalized filesystem path of file tools.
	CanonicalPath string
	// ServerName is the MCP server of an MCP tool call.
	ServerName string
}

func (*ToolCall) chunk() {}

// clone returns a copy of c with its own top-level Args map.
func (c *ToolCall) clone() *ToolCall {
	o := *c
	o.Args = maps.Clone(c.Args)
	return &o
}

// ToolResult is the outcome of a tool call.
type ToolResult struct {
	// Name is the tool name.
	Name string
	// ID correlates the result with ToolCall.ID.
	ID     string
	StepID string
	// Result is the tool's return value. For builtin tools reported to
	// PostToolCallHook it is one of the *...Result types of this package
	// when the output parses, or the raw output string.
	Result any
	// Error is the error message of a failed call.
	Error string
	// Err is the original error of a failed custom tool call. It is not
	// sent to the harness.
	Err        error
	ServerName string
}

// Failed reports whether the call failed.
func (r *ToolResult) Failed() bool { return r.Err != nil || r.Error != "" }

// SandboxStatus is the OS command sandbox status the harness reports.
type SandboxStatus struct {
	// Available reports whether the sandbox enforces isolation. When false,
	// run_command executes unsandboxed even if RunCommandConfig asked for
	// the sandbox.
	Available bool
	// UnavailableReason explains why the sandbox is unavailable.
	UnavailableReason string
}

// UsageMetadata is token usage reported by the model API. A nil field means
// no data; zero means the model reported zero tokens.
type UsageMetadata struct {
	PromptTokenCount *int64
	// CachedContentTokenCount is the subset of prompt tokens served from
	// cache.
	CachedContentTokenCount *int64
	// CandidatesTokenCount excludes thinking tokens.
	CandidatesTokenCount *int64
	ThoughtsTokenCount   *int64
	// TotalTokenCount is prompt + candidates + thoughts.
	TotalTokenCount *int64
	ServiceTier     ServiceTier
}

func (u UsageMetadata) fields() [5]*int64 {
	return [5]*int64{u.PromptTokenCount, u.CachedContentTokenCount, u.CandidatesTokenCount, u.ThoughtsTokenCount, u.TotalTokenCount}
}

func usageFromFields(f [5]*int64, tier ServiceTier) UsageMetadata {
	return UsageMetadata{f[0], f[1], f[2], f[3], f[4], tier}
}

func val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Clone returns a deep copy of u.
func (u UsageMetadata) Clone() UsageMetadata {
	var f [5]*int64
	for i, p := range u.fields() {
		if p != nil {
			f[i] = new(*p)
		}
	}
	return usageFromFields(f, u.ServiceTier)
}

// Add returns the field-wise sum of u and o, treating nil fields as zero;
// every field of the result is set. Equal tiers are kept, a missing tier
// takes the other one, and two different tiers combine to standard.
func (u UsageMetadata) Add(o UsageMetadata) UsageMetadata {
	var f [5]*int64
	a, b := u.fields(), o.fields()
	for i := range f {
		f[i] = new(val(a[i]) + val(b[i]))
	}
	tier := u.ServiceTier
	switch {
	case u.ServiceTier == o.ServiceTier:
	case u.ServiceTier == "" || o.ServiceTier == "":
		tier = u.ServiceTier + o.ServiceTier
	default:
		tier = ServiceTierStandard
	}
	return usageFromFields(f, tier)
}

// Sub returns the field-wise difference u - o, treating nil fields as zero;
// every field of the result is set. The tier is u's, or o's when u has none.
func (u UsageMetadata) Sub(o UsageMetadata) UsageMetadata {
	var f [5]*int64
	a, b := u.fields(), o.fields()
	for i := range f {
		f[i] = new(val(a[i]) - val(b[i]))
	}
	tier := u.ServiceTier
	if tier == "" {
		tier = o.ServiceTier
	}
	return usageFromFields(f, tier)
}

// Scale multiplies every set count by factor, rounding half to even as
// Python's round does. factor must be finite and non-negative.
func (u UsageMetadata) Scale(factor float64) (UsageMetadata, error) {
	if math.IsNaN(factor) || math.IsInf(factor, 0) || factor < 0 {
		return UsageMetadata{}, fmt.Errorf("agy: multiplication factor must be a finite, non-negative number, got %v", factor)
	}
	var f [5]*int64
	for i, p := range u.fields() {
		if p != nil {
			f[i] = new(int64(math.RoundToEven(float64(*p) * factor)))
		}
	}
	return usageFromFields(f, u.ServiceTier), nil
}

// SumUsage adds up usages; the sum of none is the zero UsageMetadata, and
// the sum of one is a copy of it (like Python's sum with the 0 identity).
func SumUsage(usages ...UsageMetadata) UsageMetadata {
	if len(usages) == 0 {
		return UsageMetadata{}
	}
	total := usages[0].Clone()
	for _, u := range usages[1:] {
		total = total.Add(u)
	}
	return total
}

// Step is one action in the agent trajectory.
type Step struct {
	// ID is "<trajectory id>:<step index>", or the step index alone when
	// the trajectory is unknown. Tool-call steps synthesized for custom
	// tool calls use the call ID.
	ID        string
	StepIndex int
	// TrajectoryID identifies the trajectory (the conversation for the root
	// agent, or a subagent run).
	TrajectoryID string
	// ParentTrajectoryID is the trajectory that spawned this one, empty for
	// the root agent.
	ParentTrajectoryID string
	// Depth is the subagent nesting depth (0 for the root conversation).
	Depth  int
	Type   StepType
	Source StepSource
	Target StepTarget
	Status StepStatus
	// Content is the step's full text so far; ContentDelta is the text added
	// since the previous update of the step.
	Content      string
	ContentDelta string
	// Thinking is the model's reasoning so far; ThinkingDelta the part added
	// since the previous update.
	Thinking      string
	ThinkingDelta string
	ToolCalls     []*ToolCall
	// Error is the error message of a failed step.
	Error string
	// HTTPCode is the HTTP status of a failed model call, when reported.
	HTTPCode int
	// IsCompleteResponse reports a completed model response directed at the
	// user (as opposed to a streaming chunk). Several steps of a turn may
	// have it set.
	IsCompleteResponse bool
	// StructuredOutput is the parsed JSON output of a finish step.
	StructuredOutput any
}

func (s *Step) clone() *Step {
	o := *s
	o.ToolCalls = slices.Clone(s.ToolCalls)
	return &o
}

// Chunk is one event of a TurnStream: *TextChunk, *ThoughtChunk or
// *ToolCall.
type Chunk interface{ chunk() }

// TextChunk is a delta of the model's response text (upstream Text).
type TextChunk struct {
	StepIndex int
	Text      string
}

// ThoughtChunk is a delta of the model's reasoning (upstream Thought).
type ThoughtChunk struct {
	StepIndex int
	Text      string
	Signature []byte
}

func (*TextChunk) chunk()    {}
func (*ThoughtChunk) chunk() {}
