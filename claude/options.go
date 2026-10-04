package claude

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------------
// System prompt
// ---------------------------------------------------------------------------

// SystemPrompt configures the session's system prompt. Implementations are
// SystemPromptText, SystemPromptBlocks, SystemPromptCustom,
// SystemPromptPreset and SystemPromptFile.
//
// Like the TypeScript SDK, every form except SystemPromptFile reaches the
// CLI through the initialize control request rather than the command line,
// so a long prompt does not count against the OS command-line limit.
type SystemPrompt interface {
	isSystemPrompt()
}

// SystemPromptText replaces the system prompt with a custom string.
type SystemPromptText string

func (SystemPromptText) isSystemPrompt() {}

// SystemPromptDynamicBoundary, as a standalone element of SystemPromptBlocks
// or SystemPromptCustom.Prompt, marks the split between the static prefix,
// which is eligible for cross-session prompt caching, and the
// session-specific suffix, which is not.
const SystemPromptDynamicBoundary = "__SYSTEM_PROMPT_DYNAMIC_BOUNDARY__"

// SystemPromptBlocks replaces the system prompt with a list of blocks. Put
// SystemPromptDynamicBoundary between the static and the dynamic blocks:
//
//	claude.SystemPromptBlocks{static, claude.SystemPromptDynamicBoundary, sessionContext}
type SystemPromptBlocks []string

func (SystemPromptBlocks) isSystemPrompt() {}

// SystemPromptCustom replaces the system prompt like SystemPromptBlocks, and
// also controls snapshotting.
type SystemPromptCustom struct {
	// Prompt holds the prompt blocks; a plain prompt is a single block. It
	// may contain SystemPromptDynamicBoundary.
	Prompt []string
	// Snapshot controls whether the rendered prompt is recorded in the
	// session transcript on the first request and reused verbatim by later
	// requests and resumes. nil leaves the CLI default (record); false
	// renders it fresh on every request.
	Snapshot *bool
}

func (*SystemPromptCustom) isSystemPrompt() {}

// SystemPromptPreset uses Claude Code's default system prompt, optionally with
// appended instructions.
type SystemPromptPreset struct {
	// Preset names the preset; empty means "claude_code".
	Preset string
	// Append is appended to the preset prompt.
	Append string
	// ExcludeDynamicSections strips per-user dynamic sections (working
	// directory, auto-memory, git status) so the prompt stays cacheable.
	ExcludeDynamicSections bool
	// Snapshot controls whether the rendered prompt (with Append) is
	// recorded once and reused verbatim by later requests and resumes; see
	// SystemPromptCustom.Snapshot. Recommended: true.
	Snapshot *bool
}

func (*SystemPromptPreset) isSystemPrompt() {}

// SystemPromptFile loads the system prompt from a file, passed to the CLI as
// --system-prompt-file. It is a Go extension with no TypeScript equivalent.
type SystemPromptFile struct {
	Path string
}

func (*SystemPromptFile) isSystemPrompt() {}

// ---------------------------------------------------------------------------
// Tools and skills
// ---------------------------------------------------------------------------

// ToolsConfig selects the base set of built-in tools. Implementations are
// ToolList and ToolsPreset.
type ToolsConfig interface {
	isToolsConfig()
}

// ToolList enables exactly the named built-in tools. An empty, non-nil list
// disables all of them.
type ToolList []string

func (ToolList) isToolsConfig() {}

// ToolsPreset enables all default Claude Code tools.
type ToolsPreset struct{}

func (ToolsPreset) isToolsConfig() {}

// SkillsConfig selects which skills are enabled. Implementations are SkillList
// and SkillsAll.
type SkillsConfig interface {
	isSkillsConfig()
}

// SkillList enables exactly the named skills. An empty, non-nil list hides
// every skill.
type SkillList []string

func (SkillList) isSkillsConfig() {}

// SkillsAll enables every discovered skill.
type SkillsAll struct{}

func (SkillsAll) isSkillsConfig() {}

// ---------------------------------------------------------------------------
// Misc option value types
// ---------------------------------------------------------------------------

// SettingSource names one filesystem settings layer.
type SettingSource = string

// Supported setting sources.
const (
	SettingSourceUser    SettingSource = "user"
	SettingSourceProject SettingSource = "project"
	SettingSourceLocal   SettingSource = "local"
)

// EffortLevel controls how much effort the model puts into a response.
type EffortLevel = string

// Supported effort levels.
const (
	EffortLow    EffortLevel = "low"
	EffortMedium EffortLevel = "medium"
	EffortHigh   EffortLevel = "high"
	EffortXHigh  EffortLevel = "xhigh"
	EffortMax    EffortLevel = "max"
)

// SDKBeta names a beta feature header the CLI should send.
type SDKBeta = string

// SDKBetaContext1M enables the 1M token context window (Sonnet 4/4.5 only).
const SDKBetaContext1M SDKBeta = "context-1m-2025-08-07"

// ThinkingConfig controls the model's extended thinking behavior.
type ThinkingConfig struct {
	// Type is ThinkingAdaptive, ThinkingEnabled or ThinkingDisabled.
	Type string `json:"type"`
	// BudgetTokens sets a fixed thinking budget when Type is
	// ThinkingEnabled. Without a budget, ThinkingEnabled behaves like
	// ThinkingAdaptive.
	BudgetTokens *int `json:"budget_tokens,omitempty"`
	// Display is ThinkingDisplaySummarized or ThinkingDisplayOmitted; empty
	// leaves the CLI default. Ignored when Type is ThinkingDisabled.
	Display string `json:"display,omitempty"`
}

// Thinking types for ThinkingConfig.Type.
const (
	ThinkingAdaptive = "adaptive"
	ThinkingEnabled  = "enabled"
	ThinkingDisabled = "disabled"
)

// Thinking display modes for ThinkingConfig.Display.
const (
	ThinkingDisplaySummarized = "summarized"
	ThinkingDisplayOmitted    = "omitted"
)

// TaskBudget is the API-side task budget in tokens.
type TaskBudget struct {
	Total int `json:"total"`
}

// PluginConfig loads a local plugin into the session.
type PluginConfig struct {
	// Type is always "local" today; empty means "local".
	Type string `json:"type"`
	Path string `json:"path"`
	// SkipMCPDiscovery loads the plugin's skills, hooks, agents and commands
	// but not its MCP servers (.mcp.json or manifest mcpServers). Use it when
	// the host owns the plugin's MCP connections.
	SkipMCPDiscovery bool `json:"skipMcpDiscovery,omitempty"`
}

// PluginDelivery selects how Options.Plugins reach the CLI.
type PluginDelivery = string

// Supported plugin delivery modes.
const (
	// PluginDeliveryArgv passes one --plugin-dir flag per plugin. It is the
	// default and works with every CLI version.
	PluginDeliveryArgv PluginDelivery = "argv"
	// PluginDeliveryInitialize sends the plugin list in the initialize
	// request and starts the CLI with --await-initialize, so the command
	// line does not grow with the plugin count (Windows caps it at 32,767
	// characters). Requires Claude Code 2.1.261 or newer.
	PluginDeliveryInitialize PluginDelivery = "initialize"
)

// PermissionPrompts selects who answers permission prompts.
type PermissionPrompts = string

// Supported PermissionPrompts values.
const (
	// PermissionPromptsHost routes prompts to this process, through
	// CanUseTool or PermissionPromptToolName. It is the CLI default.
	PermissionPromptsHost PermissionPrompts = "host"
	// PermissionPromptsNone denies anything that would prompt; permission
	// modes, rules and hooks still decide, and CanUseTool is never called.
	PermissionPromptsNone PermissionPrompts = "none"
)

// ToolConfig customizes built-in tools.
type ToolConfig struct {
	AskUserQuestion *AskUserQuestionConfig
}

// AskUserQuestionConfig customizes the AskUserQuestion tool.
type AskUserQuestionConfig struct {
	// PreviewFormat is the content format of the preview field on question
	// options: QuestionPreviewMarkdown (the CLI default) or
	// QuestionPreviewHTML.
	PreviewFormat string
}

// Preview formats for AskUserQuestionConfig.PreviewFormat.
const (
	QuestionPreviewMarkdown = "markdown"
	QuestionPreviewHTML     = "html"
)

// AgentDefinition programmatically defines a subagent invokable via the Agent
// tool. Field names follow the CLI's camelCase wire format.
type AgentDefinition struct {
	Description     string   `json:"description"`
	Prompt          string   `json:"prompt"`
	Tools           []string `json:"tools,omitempty"`
	DisallowedTools []string `json:"disallowedTools,omitempty"`
	// Model is a model alias ("sonnet", "opus", "haiku", "inherit") or a
	// full model ID.
	Model  string   `json:"model,omitempty"`
	Skills []string `json:"skills,omitempty"`
	// Memory is the agent-memory scope: AgentMemoryUser,
	// AgentMemoryProject or AgentMemoryLocal.
	Memory AgentMemoryScope `json:"memory,omitempty"`
	// MCPServers holds server names or inline {name: config} objects.
	MCPServers     []any          `json:"mcpServers,omitempty"`
	InitialPrompt  string         `json:"initialPrompt,omitempty"`
	MaxTurns       *int           `json:"maxTurns,omitempty"`
	Background     *bool          `json:"background,omitempty"`
	Effort         any            `json:"effort,omitempty"`
	PermissionMode PermissionMode `json:"permissionMode,omitempty"`
	// OmitClaudeMD runs the agent, as a subagent, without the user,
	// project and local CLAUDE.md files; managed policy files are kept.
	OmitClaudeMD bool `json:"omitClaudeMd,omitempty"`
	// Observer names an agent type auto-spawned as a read-only background
	// observer whenever this agent runs.
	Observer string `json:"observer,omitempty"`
	// ObserverMessage is appended to each activity digest sent to the
	// observer.
	ObserverMessage string `json:"observerMessage,omitempty"`
	// CriticalSystemReminderExperimental is a critical reminder added to the
	// agent's system prompt. Experimental.
	CriticalSystemReminderExperimental string `json:"criticalSystemReminder_EXPERIMENTAL,omitempty"`
}

// AgentMemoryScope names where an agent's memory files are loaded from.
type AgentMemoryScope = string

// Agent memory scopes for AgentDefinition.Memory.
const (
	// AgentMemoryUser loads ~/.claude/agent-memory/<agentType>/.
	AgentMemoryUser AgentMemoryScope = "user"
	// AgentMemoryProject loads .claude/agent-memory/<agentType>/.
	AgentMemoryProject AgentMemoryScope = "project"
	// AgentMemoryLocal loads .claude/agent-memory-local/<agentType>/.
	AgentMemoryLocal AgentMemoryScope = "local"
)

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

// DefaultMaxBufferSize is the default cap on a single line of CLI stdout.
const DefaultMaxBufferSize = 1024 * 1024

// Options configures a Query or a Client. The zero value is usable: it starts
// a default session with the CLI's own defaults for everything.
type Options struct {
	// ---- Model & reasoning ----

	// Model is the model to use; empty uses the CLI default.
	Model string

	// FallbackModel is used when the primary model is unavailable.
	FallbackModel string

	// Betas enables beta features.
	Betas []SDKBeta

	// Thinking controls extended thinking. Takes precedence over
	// MaxThinkingTokens.
	Thinking *ThinkingConfig

	// MaxThinkingTokens caps thinking tokens.
	//
	// Deprecated: use Thinking.
	MaxThinkingTokens *int

	// Effort guides thinking depth.
	Effort EffortLevel

	// MaxTurns caps the number of conversation turns.
	MaxTurns *int

	// MaxBudgetUSD caps the spend of the query.
	MaxBudgetUSD *float64

	// TaskBudget makes the model aware of a remaining token budget.
	TaskBudget *TaskBudget

	// ---- Prompt & output ----

	// SystemPrompt configures the system prompt. nil sends an empty custom
	// prompt, as the TypeScript SDK does; use &SystemPromptPreset{} for
	// Claude Code's default prompt.
	SystemPrompt SystemPrompt

	// PlanModeInstructions replaces the default workflow body of the
	// plan-mode system reminder while PermissionMode is plan.
	PlanModeInstructions string

	// OutputFormat requests structured output, e.g.
	// {"type": "json_schema", "schema": {...}}. The schema is sent both as
	// --json-schema and in the initialize request.
	OutputFormat map[string]any

	// VerbatimPrompts marks every user message the SDK sends as
	// client-composed, so the CLI delivers the text exactly as written: no
	// @path file-mention expansion and no slash-command dispatch. Use it when
	// prompt text is assembled from content the end user did not type, so an
	// @/absolute/path inside it cannot make Claude Code read a local file.
	//
	// While set there is no per-message opt-out: a client_composed value on a
	// UserInput.Raw frame is overwritten. Current CLIs also skip the
	// turn-start attachment pass for such turns (nested CLAUDE.md files, skill
	// listings and other per-turn reminders are not attached).
	VerbatimPrompts bool

	// IncludePartialMessages emits StreamEvent messages while the assistant
	// is streaming.
	IncludePartialMessages bool

	// IncludeHookEvents emits hook lifecycle events into the message
	// stream.
	IncludeHookEvents bool

	// ForwardSubagentText forwards subagent text and thinking blocks as
	// messages.
	ForwardSubagentText bool

	// PromptSuggestions makes the CLI emit a predicted next user prompt
	// after each turn's result.
	PromptSuggestions bool

	// AgentProgressSummaries makes the CLI emit periodic AI-generated
	// summaries of running subagents on task progress events.
	AgentProgressSummaries bool

	// ---- Tools, skills & agents ----

	// Tools selects the base set of built-in tools. nil leaves the CLI
	// default in place.
	Tools ToolsConfig

	// AllowedTools names tools that run without prompting for permission.
	AllowedTools []string

	// DisallowedTools names tools removed from the model's context.
	DisallowedTools []string

	// ToolAliases redirects model-emitted tool names before lookup, e.g.
	// {"Bash": "mcp__workspace__bash"}. The redirect is single-hop. It does
	// not replace DisallowedTools.
	ToolAliases map[string]string

	// ToolConfig customizes built-in tools.
	ToolConfig *ToolConfig

	// Skills selects which skills are enabled. nil applies no SDK
	// configuration.
	Skills SkillsConfig

	// Agents defines subagents invokable via the Agent tool.
	Agents map[string]AgentDefinition

	// Agent names the agent, from Agents or settings, that runs the main
	// thread: its prompt, tool restrictions and model apply to the
	// conversation.
	Agent string

	// ---- Permissions ----

	// PermissionMode selects how permission prompts are handled.
	PermissionMode PermissionMode

	// AllowDangerouslySkipPermissions must be set to use
	// PermissionModeBypassPermissions; it passes
	// --allow-dangerously-skip-permissions.
	AllowDangerouslySkipPermissions bool

	// PermissionPromptToolName routes permission prompts through an MCP
	// tool. Mutually exclusive with CanUseTool: when CanUseTool is set, the
	// SDK sets it to "stdio" internally, routing prompts over the control
	// protocol.
	PermissionPromptToolName string

	// PermissionPrompts selects who answers permission prompts:
	// PermissionPromptsHost (the default) or PermissionPromptsNone.
	PermissionPrompts PermissionPrompts

	// ---- Callbacks ----

	// CanUseTool answers permission requests the CLI would otherwise show
	// to a user. Mutually exclusive with PermissionPromptToolName.
	CanUseTool CanUseTool

	// Hooks registers hook callbacks per event.
	Hooks map[HookEvent][]HookMatcher

	// OnElicitation answers MCP elicitation requests (a server asking for
	// user input) that no Elicitation hook handled. When nil, every
	// elicitation is declined.
	OnElicitation OnElicitation

	// OnUserDialog renders the blocking dialogs the CLI asks the host to
	// show (request_user_dialog). The CLI only sends the kinds listed in
	// SupportedDialogKinds. When nil, dialogs are left unanswered so another
	// attached client, or the CLI's dialog deadline, settles them.
	OnUserDialog OnUserDialog

	// SupportedDialogKinds declares the dialog kinds OnUserDialog can
	// render. The CLI treats an absent kind as "cannot display" and falls
	// back to its no-dialog behavior. Requires OnUserDialog.
	SupportedDialogKinds []string

	// PerTaskStopAffordance declares that the host renders a per-task stop
	// control wired to Client.StopTask, so an interrupt spares running
	// background tasks.
	PerTaskStopAffordance bool

	// ---- MCP ----

	// MCPServers configures MCP servers by name.
	MCPServers map[string]MCPServerConfig

	// MCPConfigPath points at an MCP config JSON file, passed to the CLI
	// instead of the servers in MCPServers that the CLI runs itself.
	// In-process SDK servers in MCPServers are still declared.
	MCPConfigPath string

	// StrictMCPConfig ignores every MCP configuration the CLI would
	// otherwise load, using only MCPServers.
	StrictMCPConfig bool

	// ---- Settings & plugins ----

	// Settings adds flag-tier settings, passed to --settings. It is a
	// string holding a settings file path or an inline JSON object, or a
	// value encoded as a JSON object: Settings, map[string]any,
	// json.RawMessage or any JSON-marshalable struct. A file path cannot be
	// combined with Sandbox.
	Settings any

	// SettingSources selects which filesystem settings layers to load. nil
	// loads all of them; a non-nil empty slice loads none.
	SettingSources *[]string

	// ManagedSettings supplies policy-tier settings from the embedding
	// process, passed to --managed-settings. The CLI keeps only restrictive
	// keys, and drops them entirely when an admin-managed tier exists that
	// does not opt in to parent settings.
	ManagedSettings Settings

	// Sandbox holds sandbox settings, merged into the --settings object as
	// its "sandbox" key: a *SandboxSettings, a map[string]any, a
	// json.RawMessage or any value encoded as a JSON object. When it
	// enables the sandbox without setting failIfUnavailable, the SDK sets
	// failIfUnavailable to true so a missing sandbox fails the run instead
	// of silently running unsandboxed.
	Sandbox any

	// ProjectConfigRoot is the absolute path of the trusted checkout that
	// Cwd is a worktree of. Project settings, .mcp.json, the .claude config
	// trees and CLAUDE_PROJECT_DIR come from there instead of Cwd.
	ProjectConfigRoot string

	// Plugins loads local plugins. PluginDelivery selects how the list
	// reaches the CLI.
	Plugins []PluginConfig

	// PluginDelivery selects how Plugins reach the CLI; empty means
	// PluginDeliveryArgv.
	PluginDelivery PluginDelivery

	// ---- Session ----

	// ContinueConversation resumes the most recent conversation in Cwd.
	ContinueConversation bool

	// Resume is the session ID to resume.
	Resume string

	// ResumeSessionAt truncates the resumed conversation after the entry
	// with this UUID.
	ResumeSessionAt string

	// ResumeDropsTurn is the UUID of the user prompt whose turn a
	// truncating resume intends to discard; the CLI validates it.
	ResumeDropsTurn string

	// ForkSession makes a resumed session fork to a new session ID.
	ForkSession bool

	// SessionID pins the session ID. Must be a valid UUID.
	SessionID string

	// Title names a new session instead of deriving a title from the first
	// prompt. A resumed session keeps its stored title.
	Title string

	// NoSessionPersistence stops the CLI from writing the session
	// transcript to disk, so the session cannot be resumed. It mirrors the
	// TypeScript option persistSession: false and cannot be combined with
	// SessionStore.
	NoSessionPersistence bool

	// EnableFileCheckpointing lets Client.RewindFiles restore files to
	// their state at an earlier user message.
	EnableFileCheckpointing bool

	// SessionStore mirrors session transcripts to external storage. Every
	// transcript line the CLI writes locally is also passed to
	// SessionStore.Append, and Resume / ContinueConversation load the
	// session from the store into a temporary CLAUDE_CONFIG_DIR that is
	// removed when the session ends (skipped with a custom Transport, which
	// never sees the rewritten options). ContinueConversation without
	// Resume requires the store to implement SessionLister. It cannot be
	// combined with EnableFileCheckpointing.
	SessionStore SessionStore

	// SessionStoreFlush controls when mirrored entries reach SessionStore.
	// Empty means SessionStoreFlushBatched. Ignored without SessionStore.
	SessionStoreFlush SessionStoreFlushMode

	// LoadTimeout bounds each SessionStore.Load, ListSessions and
	// ListSubkeys call made while materializing a resumed session. Zero
	// means DefaultSessionLoadTimeout.
	LoadTimeout time.Duration

	// ---- Process ----

	// Cwd is the working directory of the CLI subprocess.
	Cwd string

	// AddDirs are additional directories the CLI may access.
	AddDirs []string

	// CLIPath is the path to the claude executable. Empty triggers
	// discovery on PATH and in the usual install locations.
	CLIPath string

	// Executable is the JavaScript runtime ("node", "bun" or "deno") used
	// when CLIPath names a .js, .mjs, .ts, .tsx or .jsx file. Empty means
	// "node".
	Executable string

	// ExecutableArgs are placed before the CLI arguments: runtime flags for
	// a JavaScript CLIPath, or leading arguments for a native binary.
	ExecutableArgs []string

	// Spawn replaces how the CLI process is started, e.g. to run it in a
	// container, VM or remote host. The SDK still builds the command line
	// and environment and passes them in SpawnOptions. When CLIPath is
	// empty the command is "claude" and no local discovery happens, and Cwd
	// is not checked locally. nil uses SpawnLocalProcess.
	//
	// ctx is not the caller's context: it is cancelled only after the SDK's
	// graceful shutdown (stdin EOF, grace period, SIGTERM) has run, so it is
	// safe to tie forced teardown of a container or VM to it.
	Spawn func(ctx context.Context, opts SpawnOptions) (SpawnedProcess, error)

	// Env holds extra environment variables for the subprocess.
	//
	// Unlike the TypeScript SDK, where env replaces the whole child
	// environment, Env is merged over this process's environment (minus
	// CLAUDECODE). The SDK then sets CLAUDE_AGENT_SDK_VERSION and, unless
	// present, CLAUDE_CODE_ENTRYPOINT and CLAUDE_CODE_SDK_READS_SESSION_STATE;
	// it removes NODE_OPTIONS, and removes DEBUG unless
	// DEBUG_CLAUDE_AGENT_SDK is truthy (then DEBUG=1).
	Env map[string]string

	// ExtraArgs passes additional CLI flags through. Keys omit the leading
	// dashes; a nil value makes the entry a boolean flag.
	ExtraArgs map[string]*string

	// User is an optional user identifier for the session.
	User string

	// MaxBufferSize caps a single line of CLI stdout. Zero means
	// DefaultMaxBufferSize.
	MaxBufferSize int

	// Stderr receives the subprocess's standard error, line by line.
	Stderr func(line string)

	// Debug enables CLI debug logging (--debug).
	Debug bool

	// DebugFile writes CLI debug logs to this file (--debug-file); it
	// implies Debug.
	DebugFile string

	// Transport replaces the CLI subprocess. It is meant for tests and for
	// embedding the SDK in a host that already owns the session; when nil,
	// a subprocess transport is built from these options. A custom
	// transport receives only the options carried by the initialize
	// request: those rendered as CLI flags or environment variables are
	// not applied.
	Transport Transport
}

// bufferSize reports the effective stdout line cap.
func (o *Options) bufferSize() int {
	if o == nil || o.MaxBufferSize <= 0 {
		return DefaultMaxBufferSize
	}
	return o.MaxBufferSize
}
