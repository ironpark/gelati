package agy

import (
	"log/slog"
	"math"
	"slices"
	"time"
)

// BuiltinTool identifies a tool the harness provides.
type BuiltinTool string

// BuiltinTool values.
const (
	BuiltinListDir        BuiltinTool = "list_directory"
	BuiltinSearchDir      BuiltinTool = "search_directory"
	BuiltinFindFile       BuiltinTool = "find_file"
	BuiltinViewFile       BuiltinTool = "view_file"
	BuiltinCreateFile     BuiltinTool = "create_file"
	BuiltinEditFile       BuiltinTool = "edit_file"
	BuiltinRunCommand     BuiltinTool = "run_command"
	BuiltinAskQuestion    BuiltinTool = "ask_question"
	BuiltinStartSubagent  BuiltinTool = "start_subagent"
	BuiltinGenerateImage  BuiltinTool = "generate_image"
	BuiltinSearchWeb      BuiltinTool = "search_web"
	BuiltinReadURLContent BuiltinTool = "read_url_content"
	BuiltinSchedule       BuiltinTool = "schedule"
	BuiltinFinish         BuiltinTool = "finish"
)

// AllTools returns every builtin tool, in declaration order.
func AllTools() []BuiltinTool {
	return []BuiltinTool{
		BuiltinListDir, BuiltinSearchDir, BuiltinFindFile, BuiltinViewFile,
		BuiltinCreateFile, BuiltinEditFile, BuiltinRunCommand, BuiltinAskQuestion,
		BuiltinStartSubagent, BuiltinGenerateImage, BuiltinSearchWeb,
		BuiltinReadURLContent, BuiltinSchedule, BuiltinFinish,
	}
}

// ReadOnlyTools returns the tools that only read state. It excludes the
// deprecated tools, which are off by default.
func ReadOnlyTools() []BuiltinTool {
	return []BuiltinTool{BuiltinViewFile, BuiltinReadURLContent, BuiltinSchedule, BuiltinFinish}
}

// NondestructiveTools returns the tools that cannot delete content. It
// excludes the deprecated tools, which are off by default.
func NondestructiveTools() []BuiltinTool {
	return []BuiltinTool{
		BuiltinViewFile, BuiltinCreateFile, BuiltinEditFile, BuiltinAskQuestion,
		BuiltinStartSubagent, BuiltinGenerateImage, BuiltinSearchWeb,
		BuiltinReadURLContent, BuiltinSchedule, BuiltinFinish,
	}
}

// FileTools returns the tools that read, write or create files.
func FileTools() []BuiltinTool {
	return []BuiltinTool{BuiltinViewFile, BuiltinCreateFile, BuiltinEditFile}
}

// NoTools returns an empty, non-nil tool list: as EnabledTools it disables
// every builtin tool.
func NoTools() []BuiltinTool { return []BuiltinTool{} }

// MinimalTools returns the minimal software engineering tool set.
func MinimalTools() []BuiltinTool {
	return []BuiltinTool{BuiltinRunCommand, BuiltinViewFile, BuiltinCreateFile, BuiltinEditFile}
}

// DeprecatedTools returns the legacy tools that are only enabled when listed
// in EnabledTools.
func DeprecatedTools() []BuiltinTool {
	return []BuiltinTool{BuiltinListDir, BuiltinSearchDir, BuiltinFindFile}
}

// PolicyFreeTools returns the read-only tools, deprecated ones included:
// the tools an agent may enable with no policy at all (see Validate), and
// those policy.SafeDefaults allows.
func PolicyFreeTools() []BuiltinTool {
	return append(ReadOnlyTools(), DeprecatedTools()...)
}

// DefaultTools returns the tools enabled when neither EnabledTools nor
// DisabledTools is set: everything except ask_question and the deprecated
// tools.
func DefaultTools() []BuiltinTool {
	return slices.DeleteFunc(AllTools(), func(t BuiltinTool) bool {
		return t == BuiltinAskQuestion || slices.Contains(DeprecatedTools(), t)
	})
}

// AgentBehavior is the operational behavior of an agent.
type AgentBehavior string

// AgentBehavior values. The zero value means AgentBehaviorAutonomous.
const (
	// AgentBehaviorAutonomous runs non-interactively: the agent must finish
	// the task on its own.
	AgentBehaviorAutonomous AgentBehavior = "autonomous"
	// AgentBehaviorInteractive works with a human, asking for clarifications;
	// it enables slash commands and planning mode.
	AgentBehaviorInteractive AgentBehavior = "interactive"
	// AgentBehaviorMinimal trims the system instructions for small-context
	// and on-device models.
	AgentBehaviorMinimal AgentBehavior = "minimal"
)

// RunCommandConfig configures the builtin run_command tool.
type RunCommandConfig struct {
	// EnableDaemons lets the agent start long-running daemon commands
	// (run_command with IsDaemon) without blocking session completion.
	EnableDaemons bool
	// Timeout bounds a command's run time. Zero uses the harness default
	// (10 minutes); negative values are invalid.
	Timeout time.Duration
	// EnableSandbox runs commands in the OS-level sandbox where the platform
	// supports it. See Agent.SandboxStatus.
	EnableSandbox bool
}

// ToolOutputTruncationConfig truncates large tool outputs: the beginning is
// kept up to MaxTokens estimated tokens and the rest replaced by a notice.
// MaxTokens zero explicitly disables truncation.
type ToolOutputTruncationConfig struct {
	MaxTokens int
}

// CapabilitiesConfig configures what the agent may do.
//
// EnabledTools and DisabledTools control which builtin tools the model sees
// at all; a disabled tool costs no tokens. Policies, by contrast, leave a
// tool visible and reject calls at run time. Prefer disabling tools the
// agent never needs, and policies for conditional restrictions.
//
// Nil and empty slices differ: a nil EnabledTools leaves the defaults
// (DefaultTools) in place, while an empty non-nil one disables every
// builtin tool; a non-nil DisabledTools is subtracted from DefaultTools.
type CapabilitiesConfig struct {
	// DisableSubagents turns subagent spawning off (upstream
	// enable_subagents=False).
	DisableSubagents bool
	// AgentBehavior defaults to AgentBehaviorAutonomous.
	AgentBehavior AgentBehavior
	// EnabledTools is an allowlist of builtin tools; mutually exclusive with
	// DisabledTools.
	EnabledTools []BuiltinTool
	// DisabledTools is a denylist subtracted from DefaultTools; mutually
	// exclusive with EnabledTools.
	DisabledTools []BuiltinTool
	// CompactionThreshold is deprecated: set Options.Compaction instead. Zero
	// means unset.
	CompactionThreshold int
	// FinishToolSchemaJSON is the JSON schema of the finish tool's
	// structured output. Options.ResponseSchema sets it.
	FinishToolSchemaJSON string
	// MaxSubagentDepth bounds subagent recursion; zero means the harness
	// default of 1.
	MaxSubagentDepth int
	// AllowedSubagents lists the subagents the root agent may invoke; nil
	// allows every registered subagent.
	AllowedSubagents []string
	// RunCommand configures the run_command tool.
	RunCommand *RunCommandConfig
	// ToolOutputTruncation truncates large tool outputs.
	ToolOutputTruncation *ToolOutputTruncationConfig
}

func (c *CapabilitiesConfig) clone() *CapabilitiesConfig {
	if c == nil {
		return nil
	}
	o := *c
	o.EnabledTools = slices.Clone(c.EnabledTools)
	o.DisabledTools = slices.Clone(c.DisabledTools)
	o.AllowedSubagents = slices.Clone(c.AllowedSubagents)
	if c.RunCommand != nil {
		r := *c.RunCommand
		o.RunCommand = &r
	}
	if c.ToolOutputTruncation != nil {
		t := *c.ToolOutputTruncation
		o.ToolOutputTruncation = &t
	}
	return &o
}

func (c *CapabilitiesConfig) behavior() AgentBehavior {
	if c.AgentBehavior == "" {
		return AgentBehaviorAutonomous
	}
	return c.AgentBehavior
}

func (c *CapabilitiesConfig) validate(logger *slog.Logger) error {
	if err := validateToolSet(c.EnabledTools, c.DisabledTools, c.RunCommand); err != nil {
		return err
	}
	if c.CompactionThreshold < 0 {
		return validationErrorf("compaction_threshold must be positive, got %d", c.CompactionThreshold)
	}
	if c.MaxSubagentDepth < 0 {
		return validationErrorf("max_subagent_depth must be at least 1, got %d", c.MaxSubagentDepth)
	}
	if t := c.ToolOutputTruncation; t != nil && (t.MaxTokens < 0 || t.MaxTokens > math.MaxInt32) {
		return validationErrorf("tool_output_truncation max_tokens must be in [0, %d], got %d", math.MaxInt32, t.MaxTokens)
	}
	if c.DisableSubagents || toolListedOff(c.EnabledTools, c.DisabledTools, BuiltinStartSubagent) {
		if c.MaxSubagentDepth != 0 {
			return validationErrorf("max_subagent_depth cannot be configured when subagents are disabled (enable_subagents=False or START_SUBAGENT not enabled).")
		}
		if c.AllowedSubagents != nil {
			return validationErrorf("allowed_subagents cannot be specified when subagents are disabled.")
		}
	}
	if slices.Contains(c.EnabledTools, BuiltinAskQuestion) && c.behavior() != AgentBehaviorInteractive {
		logger.Warn("BuiltinAskQuestion is enabled, but AgentBehavior is not interactive. " +
			"Set CapabilitiesConfig.AgentBehavior = AgentBehaviorInteractive if interactive question-and-answer behavior is desired.")
	}
	return nil
}

// validateToolSet checks the builtin tool selection shared by
// CapabilitiesConfig and SubagentCapabilities.
func validateToolSet(enabled, disabled []BuiltinTool, rc *RunCommandConfig) error {
	if enabled != nil && disabled != nil {
		return validationErrorf("enabled_tools and disabled_tools should be mutually exclusive.")
	}
	if rc != nil && rc.Timeout < 0 {
		return validationErrorf("run_command timeout must be positive, got %v", rc.Timeout)
	}
	return nil
}

// toolListedOff reports whether the tool lists turn t off: it is disabled,
// or missing from an allowlist.
func toolListedOff(enabled, disabled []BuiltinTool, t BuiltinTool) bool {
	return slices.Contains(disabled, t) || (enabled != nil && !slices.Contains(enabled, t))
}

// SubagentCapabilities configures what a subagent may do. The fields mean
// what the CapabilitiesConfig fields of the same name mean.
type SubagentCapabilities struct {
	AgentBehavior    AgentBehavior
	AllowedSubagents []string
	EnabledTools     []BuiltinTool
	DisabledTools    []BuiltinTool
	RunCommand       *RunCommandConfig
}

func (c *SubagentCapabilities) clone() *SubagentCapabilities {
	if c == nil {
		return nil
	}
	o := *c
	o.AllowedSubagents = slices.Clone(c.AllowedSubagents)
	o.EnabledTools = slices.Clone(c.EnabledTools)
	o.DisabledTools = slices.Clone(c.DisabledTools)
	if c.RunCommand != nil {
		r := *c.RunCommand
		o.RunCommand = &r
	}
	return &o
}

func (c *SubagentCapabilities) behavior() AgentBehavior {
	if c.AgentBehavior == "" {
		return AgentBehaviorAutonomous
	}
	return c.AgentBehavior
}

func (c *SubagentCapabilities) validate(logger *slog.Logger) error {
	if err := validateToolSet(c.EnabledTools, c.DisabledTools, c.RunCommand); err != nil {
		return err
	}
	if toolListedOff(c.EnabledTools, c.DisabledTools, BuiltinStartSubagent) && c.AllowedSubagents != nil {
		return validationErrorf("allowed_subagents cannot be specified when START_SUBAGENT is disabled or omitted from enabled_tools.")
	}
	if slices.Contains(c.EnabledTools, BuiltinAskQuestion) && c.behavior() != AgentBehaviorInteractive {
		logger.Warn("BuiltinAskQuestion is enabled on subagent, but its AgentBehavior is not interactive. " +
			"Set SubagentCapabilities.AgentBehavior = AgentBehaviorInteractive if interactive question-and-answer behavior is desired.")
	}
	return nil
}

// SubagentConfig declares a static subagent.
type SubagentConfig struct {
	// Name uniquely identifies the subagent.
	Name        string
	Description string
	// SystemInstructions are appended to the subagent's defaults
	// (TextSystemInstructions, TemplatedSystemInstructions) or replace them
	// (CustomSystemInstructions).
	SystemInstructions SystemInstructions
	// Capabilities default to read-only tools.
	Capabilities *SubagentCapabilities
	// Tools are custom tools available to this subagent.
	Tools []*Tool
	// ToolNames reference tools by name: a custom tool registered elsewhere
	// in the config, or a tool known to the harness.
	ToolNames []string
	// Model pins the subagent to a model name; it always runs against the
	// agent-level endpoint.
	Model string
}

// resolveActiveTools resolves the set of active builtin tools: enabled when
// set, else DefaultTools minus disabled.
func resolveActiveTools(enabled, disabled []BuiltinTool) map[BuiltinTool]bool {
	if enabled == nil {
		enabled = slices.DeleteFunc(DefaultTools(), func(t BuiltinTool) bool { return slices.Contains(disabled, t) })
	}
	set := make(map[BuiltinTool]bool, len(enabled))
	for _, t := range enabled {
		set[t] = true
	}
	return set
}

// CompactionConfig configures trajectory compaction: older history is
// compacted once the active trajectory exceeds TokenThreshold tokens. Zero
// uses the backend default.
type CompactionConfig struct {
	TokenThreshold int
}

// ModelAPIRetryConfig configures retries of transient model API errors with
// exponential backoff. Nil fields use the backend defaults.
type ModelAPIRetryConfig struct {
	MaxRetries             *uint32
	InitialSleepDurationMs *uint32
	ExponentialMultiplier  *float64
	JitterRange            *float64
}

// ModelOutputRetryConfig configures retries of malformed model outputs.
type ModelOutputRetryConfig struct {
	MaxRetries *uint32
}

// RetryConfig combines API and model-output retry settings. Unset parts use
// the backend's built-in defaults.
type RetryConfig struct {
	APIRetry         *ModelAPIRetryConfig
	ModelOutputRetry *ModelOutputRetryConfig
}

// BenchmarkRetryConfig returns the retry preset for evaluation suites and
// benchmarks: effectively unbounded API retries (max uint32) starting at one
// second, and the default model-output retries.
func BenchmarkRetryConfig() *RetryConfig {
	return &RetryConfig{APIRetry: &ModelAPIRetryConfig{
		MaxRetries:             new(uint32(math.MaxUint32)),
		InitialSleepDurationMs: new(uint32(1000)),
	}}
}

func (r *RetryConfig) validate() error {
	if r == nil || r.APIRetry == nil {
		return nil
	}
	if m := r.APIRetry.ExponentialMultiplier; m != nil && !(*m >= 0) {
		return validationErrorf("exponential_multiplier must be non-negative, got %v", *m)
	}
	if j := r.APIRetry.JitterRange; j != nil && !(*j >= 0) {
		return validationErrorf("jitter_range must be non-negative, got %v", *j)
	}
	return nil
}

// BudgetScope is the window a budget is evaluated over.
type BudgetScope string

// BudgetScope values. The zero value means BudgetScopeLifetime.
const (
	// BudgetScopeLifetime counts from the start of the session.
	BudgetScopeLifetime BudgetScope = "LIFETIME"
	// BudgetScopeForwardLooking counts from when the budget was configured
	// or the session resumed.
	BudgetScopeForwardLooking BudgetScope = "FORWARD_LOOKING"
)

// BudgetConfig caps a session. Zero fields are unset; set fields must be at
// least 1. When a cap is hit the turn stops with the matching StopReason.
type BudgetConfig struct {
	MaxModelCalls int64
	MaxToolCalls  int64
	// MaxInputTokens caps net uncached input tokens (prompt minus cached).
	MaxInputTokens int64
	// MaxOutputTokens caps candidates plus thoughts tokens.
	MaxOutputTokens int64
	// MaxTotalTokens caps net input plus output tokens.
	MaxTotalTokens int64
	Scope          BudgetScope
}

func (b *BudgetConfig) validate() error {
	if b == nil {
		return nil
	}
	check := func(name string, v, max int64) error {
		if v < 0 || v > max {
			return validationErrorf("%s must be in [1, %d], got %d", name, max, v)
		}
		return nil
	}
	for _, c := range []struct {
		name   string
		v, max int64
	}{
		{"max_model_calls", b.MaxModelCalls, math.MaxInt32},
		{"max_tool_calls", b.MaxToolCalls, math.MaxInt32},
		{"max_input_tokens", b.MaxInputTokens, math.MaxInt64},
		{"max_output_tokens", b.MaxOutputTokens, math.MaxInt64},
		{"max_total_tokens", b.MaxTotalTokens, math.MaxInt64},
	} {
		if err := check(c.name, c.v, c.max); err != nil {
			return err
		}
	}
	switch b.Scope {
	case "", BudgetScopeLifetime, BudgetScopeForwardLooking:
		return nil
	}
	return validationErrorf("unknown budget scope %q", b.Scope)
}

// SessionContinuationMode says how a session with Options.ConversationID is
// established.
type SessionContinuationMode string

// SessionContinuationMode values. The zero value leaves the choice to the
// harness.
const (
	// SessionResume resumes an existing session and fails if it is missing.
	SessionResume SessionContinuationMode = "resume"
	// SessionCreateOrResume resumes a session, creating it when missing.
	SessionCreateOrResume SessionContinuationMode = "create_or_resume"
	// SessionCreateOnly creates a session and fails if it exists.
	SessionCreateOnly SessionContinuationMode = "create_only"
)

// SystemInstructions configures the agent's system instructions:
// TextSystemInstructions, TemplatedSystemInstructions or
// CustomSystemInstructions.
type SystemInstructions interface{ systemInstructions() }

// TextSystemInstructions is shorthand for a TemplatedSystemInstructions with
// a single section titled "user_system_instructions", appended to the
// default instructions. Upstream accepts a plain str here.
type TextSystemInstructions string

func (TextSystemInstructions) systemInstructions() {}

// SystemInstructionSection is a named section appended to the default
// system instructions.
type SystemInstructionSection struct {
	Content string
	// Title defaults to "user_system_instructions".
	Title string
}

// TemplatedSystemInstructions overrides the agent's identity and appends
// sections to the default system instructions. This is the recommended way
// to customize instructions.
type TemplatedSystemInstructions struct {
	Identity string
	Sections []SystemInstructionSection
}

func (TemplatedSystemInstructions) systemInstructions() {}

// CustomSystemInstructions replace all default system instructions,
// including the safety, engineering and tool-usage guidance. For advanced
// use only.
type CustomSystemInstructions struct {
	Text string
}

func (CustomSystemInstructions) systemInstructions() {}

// MCPServer configures a Model Context Protocol server the harness
// connects to: *MCPStdioServer or *MCPStreamableHTTPServer.
type MCPServer interface {
	// ServerName returns the server's unique name.
	ServerName() string
	mcpServer() *mcpCommon
}

// mcpCommon holds the fields shared by every MCP server config
// (upstream BaseMcpServerConfig).
type mcpCommon struct {
	name           string
	timeoutSeconds int
	enabledTools   []string
	disabledTools  []string
}

// MCPStdioServer is an MCP server the harness launches and talks to over
// stdio.
type MCPStdioServer struct {
	// Name must match ^[a-zA-Z0-9_-]+$.
	Name    string
	Command string
	Args    []string
	// Env is merged into the server process's environment.
	Env map[string]string
	// TimeoutSeconds bounds connecting and listing tools; zero uses the
	// harness default.
	TimeoutSeconds int
	// EnabledTools is an allowlist; DisabledTools a denylist. They are
	// mutually exclusive; nil means all tools.
	EnabledTools  []string
	DisabledTools []string
}

// ServerName returns Name.
func (s *MCPStdioServer) ServerName() string { return s.Name }
func (s *MCPStdioServer) mcpServer() *mcpCommon {
	return &mcpCommon{s.Name, s.TimeoutSeconds, s.EnabledTools, s.DisabledTools}
}

// MCPStreamableHTTPServer is an MCP server reached over Streamable HTTP.
type MCPStreamableHTTPServer struct {
	// Name must match ^[a-zA-Z0-9_-]+$.
	Name    string
	URL     string
	Headers map[string]string
	// TimeoutSeconds bounds connecting and listing tools; zero uses the
	// harness default.
	TimeoutSeconds int
	EnabledTools   []string
	DisabledTools  []string
}

// ServerName returns Name.
func (s *MCPStreamableHTTPServer) ServerName() string { return s.Name }
func (s *MCPStreamableHTTPServer) mcpServer() *mcpCommon {
	return &mcpCommon{s.Name, s.TimeoutSeconds, s.EnabledTools, s.DisabledTools}
}

func validMCPName(name string) bool { return name != "" && onlyChars(name, "_-") }

func validateMCPServer(s MCPServer) error {
	if s == nil {
		return validationErrorf("nil MCP server config")
	}
	c := s.mcpServer()
	if !validMCPName(c.name) {
		return validationErrorf("MCP server name %q must match ^[a-zA-Z0-9_-]+$", c.name)
	}
	if c.enabledTools != nil && c.disabledTools != nil {
		return validationErrorf("enabled_tools and disabled_tools should be mutually exclusive.")
	}
	return nil
}
