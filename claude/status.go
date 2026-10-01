package claude

import (
	"encoding/json"
	"fmt"
)

// MCPServerConnectionStatus is the connection state of an MCP server.
type MCPServerConnectionStatus = string

// Known MCP server connection states.
const (
	MCPStatusConnected MCPServerConnectionStatus = "connected"
	MCPStatusFailed    MCPServerConnectionStatus = "failed"
	MCPStatusNeedsAuth MCPServerConnectionStatus = "needs-auth"
	MCPStatusPending   MCPServerConnectionStatus = "pending"
	MCPStatusDisabled  MCPServerConnectionStatus = "disabled"
)

// MCPToolAnnotations are the tool hints reported in MCP server status.
type MCPToolAnnotations struct {
	ReadOnly    *bool `json:"readOnly,omitempty"`
	Destructive *bool `json:"destructive,omitempty"`
	OpenWorld   *bool `json:"openWorld,omitempty"`
}

// MCPToolInfo describes one tool provided by an MCP server.
type MCPToolInfo struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Annotations *MCPToolAnnotations `json:"annotations,omitempty"`
	// Meta holds the MCP Apps members of the tool's _meta (ui,
	// ui/resourceUri), for hosts that render the tool's ui:// resource.
	Meta map[string]any `json:"_meta,omitempty"`
}

// MCPServerInfo is the server identity from the MCP initialize handshake.
type MCPServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// MCPServerStatus is the status of one MCP server connection.
type MCPServerStatus struct {
	// Name is the server name as configured.
	Name string `json:"name"`
	// Status is the current connection state.
	Status MCPServerConnectionStatus `json:"status"`
	// ServerInfo is set once the server is connected.
	ServerInfo *MCPServerInfo `json:"serverInfo,omitempty"`
	// Error is set when Status is "failed".
	Error string `json:"error,omitempty"`
	// Config is the server's configuration as the CLI reports it. Its "type"
	// field is one of stdio, sse, http, sdk or the output-only
	// claudeai-proxy.
	Config map[string]any `json:"config,omitempty"`
	// Scope is the configuration scope (project, user, local, claudeai,
	// managed, ...).
	Scope string `json:"scope,omitempty"`
	// Source is where the server definition came from: "sdk" for an
	// in-process server this host registered, "plugin", or the
	// configuration scope. Key trust decisions on it rather than on the
	// name. Empty on CLIs that predate the field.
	Source string `json:"source,omitempty"`
	// Tools lists the server's tools, when connected.
	Tools []MCPToolInfo `json:"tools,omitempty"`
}

// MCPStatusResponse is the result of [Client.MCPServerStatus].
type MCPStatusResponse struct {
	MCPServers []MCPServerStatus `json:"mcpServers"`
	// Raw is the full response payload, including fields not modeled above.
	Raw map[string]any `json:"-"`
}

// ContextUsageCategory is one slice of the context window (system prompt,
// tools, messages, ...).
type ContextUsageCategory struct {
	Name       string `json:"name"`
	Tokens     int    `json:"tokens"`
	Color      string `json:"color"`
	IsDeferred bool   `json:"isDeferred,omitempty"`
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
	IsLoaded   *bool  `json:"isLoaded,omitempty"`
}

// ContextUsageTool is a named tool or prompt section and its tokens.
type ContextUsageTool struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
	// IsLoaded is set for deferred built-in tools.
	IsLoaded *bool `json:"isLoaded,omitempty"`
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
	SlashCommands        *ContextUsageSlashCommands `json:"slashCommands,omitempty"`
	Skills               *ContextUsageSkills        `json:"skills,omitempty"`
	// AutoCompactThreshold is the token count at which autocompaction
	// runs, when enabled.
	AutoCompactThreshold *int                          `json:"autoCompactThreshold,omitempty"`
	IsAutoCompactEnabled bool                          `json:"isAutoCompactEnabled"`
	MessageBreakdown     *ContextUsageMessageBreakdown `json:"messageBreakdown,omitempty"`
	// APIUsage is the last API response's usage; nil before the first.
	APIUsage *ContextUsageAPIUsage `json:"apiUsage,omitempty"`
	// Raw is the full response payload, including fields not modeled above.
	Raw map[string]any `json:"-"`
}

// toWireMap renders v as the generic JSON object it marshals to, so typed
// values can be merged into wire maps. A value that marshals to null yields a
// nil map. what names the value in errors.
func toWireMap(v any, what string) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("claude: encoding %s: %w", what, err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("claude: encoding %s: %w", what, err)
	}
	return out, nil
}

// decodeResponse converts a control response payload into a typed value.
func decodeResponse(data map[string]any, out any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("claude: re-encoding control response: %w", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("claude: decoding control response: %w", err)
	}
	return nil
}

// rawHolder is implemented by response types that keep the full payload in a
// Raw field.
type rawHolder interface{ setRaw(map[string]any) }

// decodeControl decodes a control response into a new T, filling its Raw field
// when it has one. Its parameters match a control call's results, so it can
// wrap one directly: decodeControl[T](c.call(ctx, subtype, fields)).
func decodeControl[T any](data map[string]any, err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	out := new(T)
	if h, ok := any(out).(rawHolder); ok {
		h.setRaw(data)
	}
	if err := decodeResponse(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *MCPStatusResponse) setRaw(m map[string]any)    { r.Raw = m }
func (r *ContextUsageResponse) setRaw(m map[string]any) { r.Raw = m }
