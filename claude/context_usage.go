package claude

// Context window usage: the /context breakdown returned by
// Client.ContextUsage, and its structured twin carried on assistant messages.

// ContextUsageCategory is one slice of the context window (system prompt,
// tools, messages, ...).
type ContextUsageCategory struct {
	Name       string `json:"name"`
	Tokens     int    `json:"tokens"`
	Color      string `json:"color"`
	IsDeferred bool   `json:"isDeferred,omitzero"`
	// Kind classifies the row: "used", "free", "buffer" or "deferred".
	// Classify on it, never on the English name.
	Kind string `json:"kind,omitempty"`
}

// ContextUsageGridCell is one square of the /context grid visualization.
type ContextUsageGridCell struct {
	Color          string  `json:"color"`
	IsFilled       bool    `json:"isFilled"`
	CategoryName   string  `json:"categoryName"`
	Tokens         int     `json:"tokens"`
	Percentage     float64 `json:"percentage"`
	SquareFullness float64 `json:"squareFullness"`
}

// ContextUsageMemoryFile is one memory file (CLAUDE.md and friends) in the
// context.
type ContextUsageMemoryFile struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Tokens int    `json:"tokens"`
}

// ContextUsageMCPTool is one MCP tool's share of the context.
type ContextUsageMCPTool struct {
	Name       string `json:"name"`
	ServerName string `json:"serverName"`
	Tokens     int    `json:"tokens"`
	IsLoaded   *bool  `json:"isLoaded,omitzero"`
}

// ContextUsageTool is a named tool or prompt section and its tokens.
type ContextUsageTool struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
	// IsLoaded is set for deferred built-in tools.
	IsLoaded *bool `json:"isLoaded,omitzero"`
}

// ContextUsageAgent is one agent definition's share of the context.
type ContextUsageAgent struct {
	AgentType string `json:"agentType"`
	Source    string `json:"source"`
	Tokens    int    `json:"tokens"`
}

// ContextUsageSlashCommands summarizes the slash commands in the context.
type ContextUsageSlashCommands struct {
	TotalCommands    int `json:"totalCommands"`
	IncludedCommands int `json:"includedCommands"`
	Tokens           int `json:"tokens"`
}

// ContextUsageSkillFrontmatter is one skill's frontmatter in the context.
type ContextUsageSkillFrontmatter struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Tokens int    `json:"tokens"`
}

// ContextUsageSkills summarizes the skills in the context.
type ContextUsageSkills struct {
	TotalSkills      int                            `json:"totalSkills"`
	IncludedSkills   int                            `json:"includedSkills"`
	Tokens           int                            `json:"tokens"`
	SkillFrontmatter []ContextUsageSkillFrontmatter `json:"skillFrontmatter"`
}

// ContextUsageToolCalls is one tool's call and result tokens.
type ContextUsageToolCalls struct {
	Name         string `json:"name"`
	CallTokens   int    `json:"callTokens"`
	ResultTokens int    `json:"resultTokens"`
}

// ContextUsageNamedTokens is a name and a token count.
type ContextUsageNamedTokens struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
}

// ContextUsageMessageBreakdown splits the message tokens by kind.
type ContextUsageMessageBreakdown struct {
	ToolCallTokens          int                       `json:"toolCallTokens"`
	ToolResultTokens        int                       `json:"toolResultTokens"`
	AttachmentTokens        int                       `json:"attachmentTokens"`
	AssistantMessageTokens  int                       `json:"assistantMessageTokens"`
	UserMessageTokens       int                       `json:"userMessageTokens"`
	RedirectedContextTokens int                       `json:"redirectedContextTokens"`
	UnattributedTokens      int                       `json:"unattributedTokens"`
	ToolCallsByType         []ContextUsageToolCalls   `json:"toolCallsByType"`
	AttachmentsByType       []ContextUsageNamedTokens `json:"attachmentsByType"`
}

// ContextUsageAPIUsage is the token usage of the last API response.
type ContextUsageAPIUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// ContextUsageResponse is the result of [Client.ContextUsage]: the breakdown
// the CLI's /context command shows.
type ContextUsageResponse struct {
	Categories []ContextUsageCategory `json:"categories"`
	// TotalTokens is the number of tokens currently in the context window.
	TotalTokens int `json:"totalTokens"`
	// MaxTokens is the effective maximum, which an autocompact buffer may
	// reduce below RawMaxTokens.
	MaxTokens int `json:"maxTokens"`
	// RawMaxTokens is the model's raw context window size.
	RawMaxTokens int `json:"rawMaxTokens"`
	// Percentage is the share of the window in use, 0-100.
	Percentage float64 `json:"percentage"`
	// GridRows is the /context grid visualization, row by row.
	GridRows [][]ContextUsageGridCell `json:"gridRows,omitempty"`
	// Model is the model the window belongs to.
	Model                string                     `json:"model,omitempty"`
	MemoryFiles          []ContextUsageMemoryFile   `json:"memoryFiles,omitempty"`
	MCPTools             []ContextUsageMCPTool      `json:"mcpTools,omitempty"`
	DeferredBuiltinTools []ContextUsageTool         `json:"deferredBuiltinTools,omitempty"`
	SystemTools          []ContextUsageTool         `json:"systemTools,omitempty"`
	SystemPromptSections []ContextUsageTool         `json:"systemPromptSections,omitempty"`
	Agents               []ContextUsageAgent        `json:"agents,omitempty"`
	SlashCommands        *ContextUsageSlashCommands `json:"slashCommands,omitzero"`
	Skills               *ContextUsageSkills        `json:"skills,omitzero"`
	// AutoCompactThreshold is the token count at which autocompaction
	// runs, when enabled.
	AutoCompactThreshold *int                          `json:"autoCompactThreshold,omitzero"`
	IsAutoCompactEnabled bool                          `json:"isAutoCompactEnabled"`
	MessageBreakdown     *ContextUsageMessageBreakdown `json:"messageBreakdown,omitzero"`
	// APIUsage is the last API response's usage; nil before the first.
	APIUsage *ContextUsageAPIUsage `json:"apiUsage,omitzero"`
	// Raw is the full response payload, including fields not modeled above.
	Raw map[string]any `json:"-"`
}

func (r *ContextUsageResponse) setRaw(m map[string]any) { r.Raw = m }

// ContextUsageReport is the structured twin of a /context report, carried on
// AssistantMessage.ContextUsage.
type ContextUsageReport struct {
	// Model is the main-loop model the usage was computed for.
	Model string `json:"model"`
	// TotalTokens is the estimated usage; it may exceed RawMaxTokens.
	TotalTokens  int     `json:"total_tokens"`
	RawMaxTokens int     `json:"raw_max_tokens"`
	Percentage   float64 `json:"percentage"`
	// OverLimit is set when TotalTokens exceeds RawMaxTokens.
	OverLimit   *ContextReportOverLimit   `json:"over_limit,omitzero"`
	Categories  []ContextReportCategory   `json:"categories"`
	MCPTools    []ContextReportMCPTool    `json:"mcp_tools"`
	MemoryFiles []ContextReportMemoryFile `json:"memory_files"`
	Agents      []ContextReportAgent      `json:"agents"`
	Skills      []ContextReportSkill      `json:"skills,omitempty"`
}

// ContextReportOverLimit says by how much a context window is exceeded.
type ContextReportOverLimit struct {
	TokensOver int `json:"tokens_over"`
	// Kind is "hard_limit" or "compaction_window".
	Kind string `json:"kind"`
}

// ContextReportCategory is one row of the usage-by-category breakdown.
type ContextReportCategory struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
	// Kind is used, free, buffer or deferred; classify rows by it.
	Kind string `json:"kind"`
}

// ContextReportMCPTool is the context cost of one MCP tool.
type ContextReportMCPTool struct {
	Name       string `json:"name"`
	ServerName string `json:"server_name"`
	Tokens     int    `json:"tokens"`
}

// ContextReportMemoryFile is the context cost of one memory file.
type ContextReportMemoryFile = ContextUsageMemoryFile

// ContextReportAgent is the context cost of one custom agent.
type ContextReportAgent struct {
	AgentType string `json:"agent_type"`
	Source    string `json:"source"`
	Tokens    int    `json:"tokens"`
}

// ContextReportSkill is the context cost of one skill.
type ContextReportSkill struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	PluginName string `json:"plugin_name,omitempty"`
	Tokens     int    `json:"tokens"`
}
