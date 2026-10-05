package tools

//go:generate go run ./internal/gen -sdk-version 0.3.286 -out zz_generated.go

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/ironpark/gelati/internal/jsonx"
)

// Built-in tool names as they appear in ToolUseBlock.Name, CanUseTool's
// toolName and hook tool_name fields.
//
// sdk-tools.d.ts does not carry tool names; this mapping was derived from the
// schema type names, the legacy-name alias table in the TypeScript SDK
// (sdk.mjs) and the tool-name constants of the Claude Code CLI.
const (
	Agent                    = "Agent"
	Bash                     = "Bash"
	ExitPlanMode             = "ExitPlanMode"
	Edit                     = "Edit"
	Read                     = "Read"
	Write                    = "Write"
	Glob                     = "Glob"
	Grep                     = "Grep"
	TaskStop                 = "TaskStop"
	ListMcpResources         = "ListMcpResourcesTool"
	RefreshMcpTools          = "RefreshMcpTools"
	NotebookEdit             = "NotebookEdit"
	ReadMcpResourceDir       = "ReadMcpResourceDirTool"
	ReadMcpResource          = "ReadMcpResourceTool"
	ReportFindings           = "ReportFindings"
	TodoWrite                = "TodoWrite"
	WebFetch                 = "WebFetch"
	WebSearch                = "WebSearch"
	AskUserQuestion          = "AskUserQuestion"
	SendFeedback             = "SendFeedback"
	ClaudeDesign             = "ClaudeDesign"
	Projects                 = "Projects"
	EnterPlanMode            = "EnterPlanMode"
	TaskCreate               = "TaskCreate"
	TaskGet                  = "TaskGet"
	TaskUpdate               = "TaskUpdate"
	TaskList                 = "TaskList"
	Workflow                 = "Workflow"
	CronCreate               = "CronCreate"
	CronDelete               = "CronDelete"
	CronList                 = "CronList"
	ScheduleWakeup           = "ScheduleWakeup"
	RemoteTrigger            = "RemoteTrigger"
	ShowOnboardingRolePicker = "ShowOnboardingRolePicker"
	ReadNotifications        = "ReadNotifications"
	Monitor                  = "Monitor"
	ProposeSkills            = "ProposeSkills"
	ProposeGoal              = "ProposeGoal"
	Artifact                 = "Artifact"
	PushNotification         = "PushNotification"
	EnterWorktree            = "EnterWorktree"
	ExitWorktree             = "ExitWorktree"
)

// Legacy tool names still accepted by the CLI; Canonical maps them to the
// current names.
const (
	// Task is the former name of the Agent tool.
	Task = "Task"
	// KillShell and KillBash are former names of the TaskStop tool.
	KillShell = "KillShell"
	KillBash  = "KillBash"
)

// McpPrefix prefixes the names of MCP server tools ("mcp__<server>__<tool>"),
// whose input and output use McpInput and McpOutput.
const McpPrefix = "mcp__"

// ErrUnknownTool is returned (wrapped) by DecodeInput and DecodeOutput for
// tool names without a typed schema.
var ErrUnknownTool = errors.New("tools: unknown tool")

// aliases mirrors the legacy-name table of the TypeScript SDK.
var aliases = map[string]string{
	Task:                 Agent,
	KillShell:            TaskStop,
	KillBash:             TaskStop,
	"ListMcpResources":   ListMcpResources,
	"ReadMcpResource":    ReadMcpResource,
	"ReadMcpResourceDir": ReadMcpResourceDir,
}

// Tool describes the typed schema of one built-in tool.
type Tool struct {
	// Name is the canonical tool name (one of the constants above, or
	// McpPrefix for MCP tools).
	Name string
	// Input is the Go type DecodeInput returns for this tool.
	Input reflect.Type
	// Output is the Go type (or sealed interface) DecodeOutput returns for
	// this tool.
	Output reflect.Type

	decodeOut func([]byte) (any, error)
}

func tool[In, Out any](name string) Tool {
	return Tool{
		Name:   name,
		Input:  reflect.TypeFor[In](),
		Output: reflect.TypeFor[Out](),
		decodeOut: func(b []byte) (any, error) {
			var v Out
			err := json.Unmarshal(b, &v, jsonx.Foreign)
			return v, err
		},
	}
}

// sealedTool is tool for outputs that are sealed interfaces decoded by a
// generated Unmarshal function.
func sealedTool[In, Out any](name string, unmarshal func([]byte) (Out, error)) Tool {
	t := tool[In, Out](name)
	t.decodeOut = func(b []byte) (any, error) { return unmarshal(b) }
	return t
}

var registry = func() map[string]Tool {
	list := []Tool{
		sealedTool[AgentInput](Agent, UnmarshalAgentOutput),
		tool[BashInput, BashOutput](Bash),
		tool[ExitPlanModeInput, ExitPlanModeOutput](ExitPlanMode),
		tool[FileEditInput, FileEditOutput](Edit),
		sealedTool[FileReadInput](Read, UnmarshalFileReadOutput),
		tool[FileWriteInput, FileWriteOutput](Write),
		tool[GlobInput, GlobOutput](Glob),
		tool[GrepInput, GrepOutput](Grep),
		tool[TaskStopInput, TaskStopOutput](TaskStop),
		tool[ListMcpResourcesInput, ListMcpResourcesOutput](ListMcpResources),
		tool[RefreshMcpToolsInput, RefreshMcpToolsOutput](RefreshMcpTools),
		tool[McpInput, McpOutput](McpPrefix),
		tool[NotebookEditInput, NotebookEditOutput](NotebookEdit),
		tool[ReadMcpResourceDirInput, ReadMcpResourceDirOutput](ReadMcpResourceDir),
		tool[ReadMcpResourceInput, ReadMcpResourceOutput](ReadMcpResource),
		tool[ReportFindingsInput, ReportFindingsOutput](ReportFindings),
		tool[TodoWriteInput, TodoWriteOutput](TodoWrite),
		tool[WebFetchInput, WebFetchOutput](WebFetch),
		tool[WebSearchInput, WebSearchOutput](WebSearch),
		tool[AskUserQuestionInput, AskUserQuestionOutput](AskUserQuestion),
		tool[SendFeedbackInput, SendFeedbackOutput](SendFeedback),
		tool[ClaudeDesignInput, ClaudeDesignOutput](ClaudeDesign),
		sealedTool[ProjectsInput](Projects, UnmarshalProjectsOutput),
		tool[EnterPlanModeInput, EnterPlanModeOutput](EnterPlanMode),
		tool[TaskCreateInput, TaskCreateOutput](TaskCreate),
		tool[TaskGetInput, TaskGetOutput](TaskGet),
		tool[TaskUpdateInput, TaskUpdateOutput](TaskUpdate),
		tool[TaskListInput, TaskListOutput](TaskList),
		tool[WorkflowInput, WorkflowOutput](Workflow),
		tool[CronCreateInput, CronCreateOutput](CronCreate),
		tool[CronDeleteInput, CronDeleteOutput](CronDelete),
		tool[CronListInput, CronListOutput](CronList),
		tool[ScheduleWakeupInput, ScheduleWakeupOutput](ScheduleWakeup),
		tool[RemoteTriggerInput, RemoteTriggerOutput](RemoteTrigger),
		tool[ShowOnboardingRolePickerInput, ShowOnboardingRolePickerOutput](ShowOnboardingRolePicker),
		tool[ReadNotificationsInput, ReadNotificationsOutput](ReadNotifications),
		tool[MonitorInput, MonitorOutput](Monitor),
		tool[ProposeSkillsInput, ProposeSkillsOutput](ProposeSkills),
		tool[ProposeGoalInput, ProposeGoalOutput](ProposeGoal),
		tool[ArtifactInput, ArtifactOutput](Artifact),
		tool[PushNotificationInput, PushNotificationOutput](PushNotification),
		tool[EnterWorktreeInput, EnterWorktreeOutput](EnterWorktree),
		tool[ExitWorktreeInput, ExitWorktreeOutput](ExitWorktree),
	}
	m := make(map[string]Tool, len(list))
	for _, t := range list {
		if _, dup := m[t.Name]; dup {
			panic("tools: duplicate registry entry " + t.Name)
		}
		m[t.Name] = t
	}
	return m
}()

// Canonical returns the current name for a legacy tool name, McpPrefix for
// any MCP tool name, and name itself otherwise.
func Canonical(name string) string {
	if strings.HasPrefix(name, McpPrefix) {
		return McpPrefix
	}
	if c, ok := aliases[name]; ok {
		return c
	}
	return name
}

// Lookup returns the typed schema of a tool by name. Legacy names and
// "mcp__*" names are resolved as by Canonical.
func Lookup(name string) (Tool, bool) {
	t, ok := registry[Canonical(name)]
	return t, ok
}

// Tools returns the schemas of all known tools, in no particular order.
func Tools() []Tool {
	out := make([]Tool, 0, len(registry))
	for _, t := range registry {
		out = append(out, t)
	}
	return out
}

// DecodeInput converts a tool's raw input (ToolUseBlock.Input, the input
// passed to CanUseTool, or a hook's tool_input) into its typed struct, e.g.
// BashInput for "Bash" and McpInput for "mcp__server__tool". The concrete
// value (not a pointer) is returned, so callers can type-switch on it.
// Unknown tool names yield an error wrapping ErrUnknownTool.
func DecodeInput(name string, input map[string]any) (any, error) {
	t, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownTool, name)
	}
	if t.Name == McpPrefix {
		return McpInput(input), nil
	}
	b, err := json.Marshal(input, marshalOpts)
	if err != nil {
		return nil, fmt.Errorf("tools: encode %s input: %w", name, err)
	}
	v := reflect.New(t.Input)
	if err := json.Unmarshal(b, v.Interface(), jsonx.Foreign); err != nil {
		return nil, fmt.Errorf("tools: decode %s input: %w", name, err)
	}
	return v.Elem().Interface(), nil
}

// DecodeOutput converts a tool's structured result (UserMessage.ToolUseResult
// or a PostToolUse hook's tool_response) into its typed form, e.g. BashOutput
// for "Bash" or one of the AgentOutput variants for "Agent". output may be
// jsontext.Value or []byte holding raw JSON, or any value that encodes to it
// (a map[string]any, []any, string, ...). Unknown tool names yield an error
// wrapping ErrUnknownTool.
func DecodeOutput(name string, output any) (any, error) {
	t, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownTool, name)
	}
	var b []byte
	switch o := output.(type) {
	case jsontext.Value:
		b = o
	case []byte:
		b = o
	default:
		var err error
		if b, err = json.Marshal(output, marshalOpts); err != nil {
			return nil, fmt.Errorf("tools: encode %s output: %w", name, err)
		}
	}
	v, err := t.decodeOut(b)
	if err != nil {
		return nil, fmt.Errorf("tools: decode %s output: %w", name, err)
	}
	return v, nil
}

// As decodes a raw tool input (or any JSON-shaped map) into T, e.g.
//
//	in, err := tools.As[tools.BashInput](input)
//
// It does not check the tool name; use DecodeInput to dispatch by name.
func As[T any](input map[string]any) (T, error) {
	var v T
	b, err := json.Marshal(input, marshalOpts)
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(b, &v, jsonx.Foreign)
	return v, err
}

// ToMap encodes a typed input (or any JSON-encodable value that encodes to an
// object) back into the map[string]any form used by ToolUseBlock.Input and
// PermissionResultAllow.UpdatedInput.
func ToMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v, marshalOpts)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m, jsonx.Foreign); err != nil {
		return nil, fmt.Errorf("tools: %T does not encode to a JSON object: %w", v, err)
	}
	return m, nil
}
